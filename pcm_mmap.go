package alsa

import (
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// AvailUpdate synchronizes the PCM state with the kernel and returns the number of available frames.
// For playback streams, this is the number of frames that can be written.
// For capture streams, this is the number of frames that can be read.
// This function is only available for MMAP streams.
func (p *PCM) AvailUpdate() (int, error) {
	if (p.flags & PCM_MMAP) == 0 {
		return 0, fmt.Errorf("AvailUpdate() is only available for MMAP streams")
	}

	if err := p.ioctlSync(SNDRV_PCM_SYNC_PTR_APPL | SNDRV_PCM_SYNC_PTR_AVAIL_MIN | SNDRV_PCM_SYNC_PTR_HWSYNC); err != nil {
		// If ioctlSync failed, the error code is the most reliable source for the stream's state.
		if errors.Is(err, syscall.EPIPE) {
			p.setState(PCM_STATE_XRUN)

			p.xruns++
		} else if errors.Is(err, unix.EBADFD) {
			p.setState(PCM_STATE_SETUP)
		} else if errors.Is(err, syscall.ESTRPIPE) {
			p.setState(PCM_STATE_SUSPENDED)
		} else if errors.Is(err, syscall.ENODEV) {
			p.setState(PCM_STATE_DISCONNECTED)
		}

		return 0, err
	}

	// After ioctlSync, the fresh values are in p.syncPtr.
	currentState := p.syncPtr.S.State
	p.setState(currentState)

	// CRITICAL: Check the state after synchronization.
	switch currentState {
	case PCM_STATE_XRUN:
		p.xruns++

		return 0, syscall.EPIPE
	case PCM_STATE_OPEN, PCM_STATE_SETUP, PCM_STATE_DRAINING:
		return 0, unix.EBADFD
	case PCM_STATE_SUSPENDED:
		return 0, syscall.ESTRPIPE
	case PCM_STATE_DISCONNECTED:
		return 0, syscall.ENODEV
	}

	// Use the pointers from the struct updated by the ioctl.
	hwPtr := p.syncPtr.S.HwPtr
	// Atomically read the application pointer from the control structure. This is the canonical
	// value, pointed to by p.mmapControl (either in shared memory or the syncPtr fallback).
	var applPtr SndPcmUframesT
	if unsafe.Sizeof(applPtr) == 8 {
		applPtr = SndPcmUframesT(atomic.LoadUint64((*uint64)(unsafe.Pointer(&p.mmapControl.ApplPtr))))
	} else {
		applPtr = SndPcmUframesT(atomic.LoadUint32((*uint32)(unsafe.Pointer(&p.mmapControl.ApplPtr))))
	}

	var avail SndPcmUframesT
	if (p.flags & PCM_IN) != 0 {
		// Capture: available frames to read (hwPtr - applPtr)
		avail = hwPtr - applPtr
	} else {
		// Playback: available space to write (hwPtr + bufferSize - applPtr)
		avail = hwPtr + SndPcmUframesT(p.bufferSize) - applPtr
	}

	// Handle boundary wrapping.
	if p.boundary > 0 && avail >= p.boundary {
		avail -= p.boundary
	}

	// Availability (for playback: free space, for capture: filled space) cannot exceed the buffer size.
	// This prevents underflow/overflow if pointers are inconsistent (e.g., during initialization or XRUN).
	if avail > SndPcmUframesT(p.bufferSize) {
		avail = SndPcmUframesT(p.bufferSize)
	}

	return int(avail), nil
}

// AvailMax returns the maximum number of frames that can be written to a playback stream or read from a capture stream.
// This function is only available for MMAP streams.
func (p *PCM) AvailMax() (int, error) {
	avail, err := p.AvailUpdate()
	if err != nil {
		return 0, err
	}

	return int(p.bufferSize) - avail, nil
}

// MmapWrite writes interleaved audio data to a playback MMAP PCM device.
// The provided `data` must be a slice of a supported numeric type (e.g., []int16, []float32).
// It automatically handles waiting for buffer space and starting the stream once the 'StartThreshold' is met.
func (p *PCM) MmapWrite(data any) (int, error) {
	if (p.flags & PCM_MMAP) == 0 {
		return 0, fmt.Errorf("MmapWrite can only be used with MMAP streams")
	}

	if (p.flags & PCM_IN) != 0 {
		return 0, fmt.Errorf("cannot write to a capture device")
	}

	dataPtr, dataByteLen, err := checkSliceAndGetData(data)
	if err != nil {
		return 0, fmt.Errorf("invalid data type for MmapWrite: %w", err)
	}

	// Keep the data slice alive while we use pointers derived from it.
	defer runtime.KeepAlive(data)

	// Ensure the stream is prepared if it was stopped (e.g., via linked recovery) or not yet prepared.
	// MMAP operations require the stream to be at least in the PREPARED state.
	s := p.getState()
	if s == PCM_STATE_SETUP || s == PCM_STATE_OPEN {
		if err := p.Prepare(); err != nil {
			return 0, err
		}
	}

	offset := 0
	remainingBytes := int(dataByteLen)

	for remainingBytes > 0 {
		wantFrames := PcmBytesToFrames(p, uint32(remainingBytes))
		if wantFrames == 0 {
			break
		}

		buffer, _, framesToCopy, avail, err := p.MmapBegin(wantFrames)
		if err != nil {
			if errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
				if recoveryErr := p.xrunRecover(err); recoveryErr != nil {
					return offset, recoveryErr // Recovery failed, so abort.
				}

				continue // Recovery succeeded, retry the operation.
			}

			return offset, err
		}

		if framesToCopy == 0 {
			// Buffer is full, so we must wait for space.
			timeout := -1
			if (p.flags & PCM_NOIRQ) != 0 {
				avail, availErr := p.AvailUpdate()
				if availErr == nil && uint32(avail) < p.config.AvailMin {
					if p.noirqFramesPerMsec > 0 {
						timeout = int((p.config.AvailMin - uint32(avail)) / p.noirqFramesPerMsec)
						if timeout < 1 {
							timeout = 1
						}
					}
				}
			}

			ready, waitErr := p.Wait(timeout)
			if waitErr != nil {
				if errors.Is(waitErr, syscall.EPIPE) || errors.Is(waitErr, unix.EBADFD) {
					// An XRUN was detected. Recover and retry.
					if recoveryErr := p.xrunRecover(waitErr); recoveryErr != nil {
						return offset, recoveryErr // Recovery failed, so abort.
					}

					continue // Recovery succeeded, retry the operation.
				}

				return offset, fmt.Errorf("pcm wait failed: %w", waitErr)
			}

			if !ready { // Timeout or stream not ready
				if (p.flags & PCM_NONBLOCK) != 0 {
					return offset, syscall.EAGAIN
				}

				// If blocking, we must ensure the stream is prepared before retrying the wait,
				// in case the state transitioned back to SETUP (e.g., linked stream stop).
				if p.getState() == PCM_STATE_SETUP {
					if err := p.Prepare(); err != nil {
						return offset, err
					}
				}

				continue
			}

			continue
		}

		bytesToCopy := int(PcmFramesToBytes(p, framesToCopy))
		srcPtr := unsafe.Pointer(uintptr(dataPtr) + uintptr(offset))
		srcSlice := unsafe.Slice((*byte)(srcPtr), bytesToCopy)
		copy(buffer, srcSlice)

		// Update offset and remainingBytes immediately after successful copy, before MmapCommit.
		offset += bytesToCopy
		remainingBytes -= bytesToCopy

		if err := p.MmapCommit(framesToCopy); err != nil {
			// Check if the error is a recoverable XRUN (underrun for playback).
			if errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
				if recoveryErr := p.xrunRecover(err); recoveryErr != nil {
					return offset, recoveryErr
				}

				// Recovery succeeded (stream is PREPARED). Continue the loop to allow
				// the auto-start logic to re-evaluate on the next iteration.
				continue
			} else {
				// If it's not an XRUN, it's a fatal error.
				return offset, err
			}
		}

		// After a successful commit, check if we need to start the stream.
		if p.getState() == PCM_STATE_PREPARED {
			// We use the 'avail' value returned by MmapBegin to avoid a second syscall.
			// 'avail' was the free space *before* the current write operation.
			oldFramesInBuf := p.bufferSize - uint32(avail)
			newFramesInBuf := oldFramesInBuf + framesToCopy

			if newFramesInBuf >= p.config.StartThreshold {
				if startErr := p.Start(); startErr != nil {
					// A failed start is a failure for this write attempt.
					return offset, startErr
				}
			}
		}
	}

	return offset, nil
}

// MmapRead reads interleaved audio data from a capture MMAP PCM device.
// The provided `data` must be a slice of a supported numeric type (e.g., []int16, []float32) that will receive the data.
// It automatically handles waiting for data. The stream must be started via Start() or by a linked playback stream before this function is called.
func (p *PCM) MmapRead(data any) (int, error) {
	if (p.flags & PCM_MMAP) == 0 {
		return 0, fmt.Errorf("MmapRead can only be used with MMAP streams")
	}

	if (p.flags & PCM_IN) == 0 {
		return 0, fmt.Errorf("cannot read from a playback device")
	}

	dataPtr, dataByteLen, err := checkSliceAndGetData(data)
	if err != nil {
		return 0, fmt.Errorf("invalid data type for MmapRead: %w", err)
	}

	// Keep the data slice alive while we use pointers derived from it.
	defer runtime.KeepAlive(data)

	// Ensure the stream is prepared if it was stopped (e.g., via linked recovery) or not yet prepared.
	// MMAP operations require the stream to be at least in the PREPARED state.
	s := p.getState()
	if s == PCM_STATE_SETUP || s == PCM_STATE_OPEN {
		if err := p.Prepare(); err != nil {
			return 0, err
		}
	}

	offset := 0
	remainingBytes := int(dataByteLen)

	for remainingBytes > 0 {
		wantFrames := PcmBytesToFrames(p, uint32(remainingBytes))
		if wantFrames == 0 {
			break
		}

		buffer, _, framesToCopy, _, err := p.MmapBegin(wantFrames)
		if err != nil {
			if errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
				if recoveryErr := p.xrunRecover(err); recoveryErr != nil {
					return offset, recoveryErr // Recovery failed, so abort.
				}

				// Recovery succeeded (stream is PREPARED).
				// We rely on the Wait loop below or the auto-start logic at the beginning of the function
				// to restart the stream if necessary. This avoids restart race conditions with linked streams.
				continue
			}

			return offset, err
		}

		if framesToCopy == 0 {
			timeout := -1
			if (p.flags & PCM_NOIRQ) != 0 {
				avail, availErr := p.AvailUpdate()
				if availErr == nil && uint32(avail) < p.config.AvailMin {
					if p.noirqFramesPerMsec > 0 {
						timeout = int((p.config.AvailMin - uint32(avail)) / p.noirqFramesPerMsec)
						if timeout < 1 {
							timeout = 1
						}
					}
				}
			}

			ready, waitErr := p.Wait(timeout)
			if waitErr != nil {
				if errors.Is(waitErr, syscall.EPIPE) || errors.Is(waitErr, unix.EBADFD) {
					// An XRUN was detected. Recover and retry.
					if recoveryErr := p.xrunRecover(waitErr); recoveryErr != nil {
						return offset, recoveryErr // Recovery failed, so abort.
					}

					// Recovery succeeded (stream is PREPARED).
					// Continue the loop, where MmapBegin will be retried, likely resulting in framesToCopy=0 again,
					// leading back to Wait, where the restart logic resides (in the !ready block).
					continue
				}

				return offset, fmt.Errorf("pcm wait failed: %w", waitErr)
			}

			if !ready { // Timeout or stream not ready
				if (p.flags & PCM_NONBLOCK) != 0 {
					return offset, syscall.EAGAIN
				}

				// If blocking, ensure the stream is prepared before retrying the wait,
				// in case the state transitioned back to SETUP.
				if p.getState() == PCM_STATE_SETUP {
					if err := p.Prepare(); err != nil {
						return offset, err
					}
				}

				continue
			}

			continue
		}

		bytesToCopy := int(PcmFramesToBytes(p, framesToCopy))
		dstPtr := unsafe.Pointer(uintptr(dataPtr) + uintptr(offset))
		dstSlice := unsafe.Slice((*byte)(dstPtr), bytesToCopy)
		copy(dstSlice, buffer)

		// Update offset and remainingBytes immediately after successful copy, before MmapCommit.
		offset += bytesToCopy
		remainingBytes -= bytesToCopy

		if err := p.MmapCommit(framesToCopy); err != nil {
			// Check if the error is a recoverable XRUN (overrun for capture).
			// This typically happens if the SYNC_PTR fallback is used and detects an XRUN during the pointer update.
			if errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
				// An XRUN occurred during commit. The data we read is valid,
				// but the stream needs recovery before we can continue reading more data.
				if recoveryErr := p.xrunRecover(err); recoveryErr != nil {
					// Recovery failed. Return the data read so far and the error.
					return offset, recoveryErr
				}

				// Recovery succeeded (stream is PREPARED).
				// Continue the loop. The restart will be handled by the main loop logic.
				continue
			}

			// If it's not an XRUN, it's a fatal error. Return data read so far and the error.
			return offset, err
		}
	}

	return offset, nil
}

// MmapBegin prepares for a memory-mapped transfer. It returns a slice of the main buffer corresponding to the available contiguous
// space for writing or reading, the offset in frames from the start of the buffer, and the number of frames available in that slice.
func (p *PCM) MmapBegin(wantFrames uint32) (buffer []byte, offsetFrames, actualFrames uint32, avail SndPcmUframesT, err error) {
	if (p.flags & PCM_MMAP) == 0 {
		err = fmt.Errorf("MmapBegin() is only available for MMAP streams")
		return
	}

	// For mmap streams, sync the hardware pointer to ensure the values we read are up to date.
	// We use SYNC_PTR instead of HWSYNC because it's synchronous and more reliable across different drivers,
	// preventing race conditions where we might read stale pointers after HWSYNC returns.
	if syncErr := p.ioctlSync(SNDRV_PCM_SYNC_PTR_APPL | SNDRV_PCM_SYNC_PTR_HWSYNC); syncErr != nil {
		// If the ioctl fails, update our state from the error code and propagate the error.
		if errors.Is(syncErr, syscall.EPIPE) {
			p.setState(PCM_STATE_XRUN)
		} else if errors.Is(syncErr, unix.EBADFD) {
			p.setState(PCM_STATE_SETUP)
		} else if errors.Is(syncErr, syscall.ESTRPIPE) {
			p.setState(PCM_STATE_SUSPENDED)
		} else if errors.Is(syncErr, syscall.ENODEV) {
			p.setState(PCM_STATE_DISCONNECTED)
		}

		err = syncErr
		return
	}

	// After a successful ioctlSync, the fresh state and hardware pointer are in p.syncPtr.
	// We use these values for our calculations.
	currentState := p.syncPtr.S.State
	p.setState(currentState) // Update cached state

	// CRITICAL: Check the state after syncing pointers. If the stream is in an error or transitional state,
	// we must return an error immediately so the caller can initiate recovery.
	switch currentState {
	case PCM_STATE_XRUN:
		err = syscall.EPIPE
		return
	case PCM_STATE_OPEN, PCM_STATE_SETUP, PCM_STATE_DRAINING:
		// EBADFD is conventionally used to signal that the stream is stopped or in a state unsuitable for I/O.
		err = unix.EBADFD
		return
	case PCM_STATE_SUSPENDED:
		err = syscall.ESTRPIPE
		return
	case PCM_STATE_DISCONNECTED:
		err = syscall.ENODEV
		return
	}

	// Read the application pointer from the mmap'd control page (or the fallback struct).
	// This is the value that was just synchronized with the kernel.
	// Read the hardware pointer from the syncPtr struct, which was just updated by the kernel.
	var applPtr, hwPtr SndPcmUframesT
	if unsafe.Sizeof(applPtr) == 8 {
		applPtr = SndPcmUframesT(atomic.LoadUint64((*uint64)(unsafe.Pointer(&p.mmapControl.ApplPtr))))
	} else {
		applPtr = SndPcmUframesT(atomic.LoadUint32((*uint32)(unsafe.Pointer(&p.mmapControl.ApplPtr))))
	}
	hwPtr = p.syncPtr.S.HwPtr

	offset := applPtr % SndPcmUframesT(p.bufferSize)
	offsetFrames = uint32(offset)

	if (p.flags & PCM_IN) != 0 {
		avail = hwPtr - applPtr
	} else {
		avail = hwPtr + SndPcmUframesT(p.bufferSize) - applPtr
	}

	if p.boundary > 0 && avail >= p.boundary {
		avail -= p.boundary
	}

	// Availability (for playback: free space, for capture: filled space) cannot exceed the buffer size.
	if avail > SndPcmUframesT(p.bufferSize) {
		avail = SndPcmUframesT(p.bufferSize)
	}

	continuousFrames := p.bufferSize - offsetFrames
	framesToCopy := wantFrames
	if framesToCopy > uint32(avail) {
		framesToCopy = uint32(avail)
	}

	if framesToCopy > continuousFrames {
		framesToCopy = continuousFrames
	}

	actualFrames = framesToCopy

	frameSize := uint64(PcmFramesToBytes(p, 1))
	byteOffset := uint64(offsetFrames) * frameSize
	byteCount := uint64(actualFrames) * frameSize

	if byteOffset+byteCount > uint64(len(p.mmapBuffer)) {
		// This condition indicates that the application pointer (applPtr) is invalid due to inconsistent state.
		// Return EBADFD to signal an invalid state and force the caller to recover (e.g., Prepare).
		_ = fmt.Errorf("mmap begin calculation exceeds buffer bounds: offset=%d, count=%d, buffer_len=%d. State=%v: %w",
			byteOffset, byteCount, len(p.mmapBuffer), currentState, unix.EBADFD)
		err = unix.EBADFD

		return
	}

	if byteCount > 0 {
		buffer = p.mmapBuffer[byteOffset : byteOffset+byteCount]
	}

	return
}

// MmapCommit commits the number of frames transferred after a MmapBegin call.
func (p *PCM) MmapCommit(frames uint32) error {
	if (p.flags & PCM_MMAP) == 0 {
		return fmt.Errorf("MmapCommit() is only available for MMAP streams")
	}

	// Use atomic operations to update ApplPtr in shared memory.
	// Load ApplPtr atomically before modification (acquire barrier).
	var applPtr SndPcmUframesT
	// We determine the size of SndPcmUframesT at runtime using unsafe.Sizeof.
	ptrSize := unsafe.Sizeof(applPtr)

	var currentApplPtr SndPcmUframesT

	if ptrSize == 8 {
		currentApplPtr = SndPcmUframesT(atomic.LoadUint64((*uint64)(unsafe.Pointer(&p.mmapControl.ApplPtr))))
	} else {
		currentApplPtr = SndPcmUframesT(atomic.LoadUint32((*uint32)(unsafe.Pointer(&p.mmapControl.ApplPtr))))
	}

	newApplPtr := currentApplPtr + SndPcmUframesT(frames)
	if p.boundary > 0 && newApplPtr >= p.boundary {
		// We assume frames is small enough that it only wraps once.
		newApplPtr -= p.boundary
	}

	// Use atomic store for ApplPtr to ensure release semantics (write barrier).
	// This ensures audio data writes are visible before the pointer is updated.
	if ptrSize == 8 {
		atomic.StoreUint64((*uint64)(unsafe.Pointer(&p.mmapControl.ApplPtr)), uint64(newApplPtr))
	} else {
		atomic.StoreUint32((*uint32)(unsafe.Pointer(&p.mmapControl.ApplPtr)), uint32(newApplPtr))
	}

	// After updating the application pointer, we must notify the kernel and get the latest state back.
	// HWSYNC is crucial here because it forces the kernel to update the status part of our syncPtr struct,
	// allowing us to see if the stream auto-started as a result of this commit.
	if err := p.ioctlSync(SNDRV_PCM_SYNC_PTR_APPL | SNDRV_PCM_SYNC_PTR_HWSYNC); err != nil {
		// The ioctl failed, which often indicates an XRUN or other state change.
		// Update our state from the error code and propagate the error.
		if errors.Is(err, syscall.EPIPE) {
			p.setState(PCM_STATE_XRUN)
		} else if errors.Is(err, unix.EBADFD) {
			p.setState(PCM_STATE_SETUP)
		} else if errors.Is(err, syscall.ESTRPIPE) {
			p.setState(PCM_STATE_SUSPENDED)
		}

		return err
	}

	// After a successful sync, update our cached state from the kernel's response.
	// This is critical for detecting if the kernel auto-started the stream.
	p.setState(p.syncPtr.S.State)

	return nil
}

// Timestamp returns available frames and the corresponding timestamp.
// The clock source is CLOCK_MONOTONIC if the PCM_MONOTONIC flag was used, otherwise it is CLOCK_REALTIME.
// This method is only available for MMAP streams.
func (p *PCM) Timestamp() (availFrames uint32, t time.Time, err error) {
	if (p.flags & PCM_MMAP) == 0 {
		err = fmt.Errorf("method Timestamp() is only available for MMAP streams")

		return
	}

	// Use the STATUS ioctl to get a consistent snapshot of pointers and timestamp.
	var status sndPcmStatus
	if ioctlErr := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_STATUS, uintptr(unsafe.Pointer(&status))); ioctlErr != nil {
		err = fmt.Errorf("ioctl STATUS failed: %w", ioctlErr)
		return
	}

	p.setState(status.State)

	// status.Avail is the number of frames available for I/O.
	// For playback, it's writable space. For capture, it's readable data.
	availFrames = uint32(status.Avail)
	ts := status.Tstamp
	t = time.Unix(ts.Sec, ts.Nsec)

	return
}

// HWPtr returns the current hardware pointer position and the corresponding timestamp.
// This method is only available for MMAP streams.
func (p *PCM) HWPtr() (hwPtr SndPcmUframesT, tstamp time.Time, err error) {
	if !p.IsReady() {
		err = fmt.Errorf("PCM is not ready")

		return
	}

	if (p.flags & PCM_MMAP) == 0 {
		err = fmt.Errorf("method HWPtr() is only available for MMAP streams")

		return
	}

	// Use the STATUS ioctl to get a consistent snapshot of pointer and timestamp.
	var status sndPcmStatus
	if ioctlErr := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_STATUS, uintptr(unsafe.Pointer(&status))); ioctlErr != nil {
		err = fmt.Errorf("ioctl STATUS failed: %w", ioctlErr)

		return
	}

	p.setState(status.State)

	// The snd-aloop driver may not provide a valid timestamp, so we don't treat a zero value as an error.
	ts := status.Tstamp
	tstamp = time.Unix(ts.Sec, ts.Nsec)
	hwPtr = status.HwPtr

	return
}
