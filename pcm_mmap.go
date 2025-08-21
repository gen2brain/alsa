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
// This method is only available for MMAP streams.
func (p *PCM) AvailUpdate() (int, error) {
	if (p.flags & PCM_MMAP) == 0 {
		return 0, fmt.Errorf("method AvailUpdate() is only available for MMAP streams")
	}

	if err := p.syncPtr(SNDRV_PCM_SYNC_PTR_HWSYNC); err != nil {
		// On error, check the stream state. If it's an XRUN, the pointers are invalid.
		// For playback, the buffer is empty (avail = buffer_size). For capture, no frames are readable.
		if p.State() == SNDRV_PCM_STATE_XRUN {
			if (p.flags & PCM_IN) != 0 {
				return 0, syscall.EPIPE // Capture: No frames available
			}

			return int(p.bufferSize), syscall.EPIPE // Playback: Full buffer available
		}

		return 0, err
	}

	var applPtr, hwPtr SndPcmUframesT
	if unsafe.Sizeof(applPtr) == 8 {
		applPtr = SndPcmUframesT(atomic.LoadUint64((*uint64)(unsafe.Pointer(&p.mmapControl.ApplPtr))))
		hwPtr = SndPcmUframesT(atomic.LoadUint64((*uint64)(unsafe.Pointer(&p.mmapStatus.HwPtr))))
	} else {
		applPtr = SndPcmUframesT(atomic.LoadUint32((*uint32)(unsafe.Pointer(&p.mmapControl.ApplPtr))))
		hwPtr = SndPcmUframesT(atomic.LoadUint32((*uint32)(unsafe.Pointer(&p.mmapStatus.HwPtr))))
	}

	var avail int
	if (p.flags & PCM_IN) != 0 {
		// For capture, avail is the number of frames ready to be read.
		avail = int(hwPtr) - int(applPtr)
		if avail < 0 {
			avail += int(p.boundary)
		}
	} else {
		// For playback, avail is the free space.
		used := int(applPtr) - int(hwPtr)
		if used < 0 {
			used += int(p.boundary)
		}
		avail = int(p.bufferSize) - used
	}

	return avail, nil
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
		return 0, fmt.Errorf("method MmapWrite() can only be used with MMAP streams")
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

	offset := 0
	remainingBytes := int(dataByteLen)

	for remainingBytes > 0 {
		wantFrames := PcmBytesToFrames(p, uint32(remainingBytes))
		if wantFrames == 0 {
			break
		}

		// Ensure the stream is prepared if it was stopped (e.g., via linked recovery) or not yet prepared.
		s := p.State()
		if s == SNDRV_PCM_STATE_SETUP || s == SNDRV_PCM_STATE_OPEN {
			if err := p.Prepare(); err != nil {
				return offset, err
			}
		}

		buffer, _, framesToCopy, _, err := p.MmapBegin(wantFrames)
		if err != nil {
			if errors.Is(err, syscall.EPIPE) {
				if recoveryErr := p.xrunRecover(err); recoveryErr != nil {
					return offset, recoveryErr // XRUN recovery failed, so abort.
				}

				continue // XRUN recovery succeeded, retry the operation.
			}

			if errors.Is(err, unix.EBADFD) {
				// EBADFD means the stream is in a state like SETUP. Just prepare it.
				if prepErr := p.Prepare(); prepErr != nil {
					return offset, prepErr
				}

				continue // Retry now that the stream is PREPARED.
			}

			return offset, err
		}

		if framesToCopy == 0 {
			// Buffer is full, so we must wait for space.
			ready, waitErr := p.Wait(-1)
			if waitErr != nil {
				if errors.Is(waitErr, syscall.EPIPE) {
					// An XRUN was detected. Recover and retry.
					if recoveryErr := p.xrunRecover(waitErr); recoveryErr != nil {
						return offset, recoveryErr // Recovery failed, so abort.
					}

					continue // Recovery succeeded, retry the operation.
				}
				return offset, fmt.Errorf("pcm wait failed: %w", waitErr)
			}
			if !ready {
				// This case should ideally not be hit with a timeout of -1,
				// but we handle it defensively.
				continue
			}

			// After a successful wait, continue the loop to try MmapBegin again.
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
			if errors.Is(err, syscall.EPIPE) {
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

		// If the stream is in the PREPARED state after we've committed new frames,
		// it's our responsibility to start it.
		if p.State() == SNDRV_PCM_STATE_PREPARED {
			if err := p.Start(); err != nil {
				// EBADFD is not a fatal error here. It indicates the stream was
				// started by another thread or the kernel in the small window
				// between the State() check and the Start() call.
				if !errors.Is(err, unix.EBADFD) {
					return offset, err
				}
			}
		}
	}

	return offset, nil
}

// MmapRead reads interleaved audio data from a capture MMAP PCM device.
// The provided `data` must be a slice of a supported numeric type (e.g., []int16, []float32) that will receive the data.
// It automatically handles waiting for data and starting the stream if it is in a prepared state.
func (p *PCM) MmapRead(data any) (int, error) {
	if (p.flags & PCM_MMAP) == 0 {
		return 0, fmt.Errorf("method MmapRead() can only be used with MMAP streams")
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
	s := p.State()
	if s == SNDRV_PCM_STATE_SETUP || s == SNDRV_PCM_STATE_OPEN {
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
			if errors.Is(err, syscall.EPIPE) {
				if recoveryErr := p.xrunRecover(err); recoveryErr != nil {
					return offset, recoveryErr // XRUN recovery failed, so abort.
				}

				continue // XRUN recovery succeeded, retry.
			}

			if errors.Is(err, unix.EBADFD) {
				// EBADFD means the stream is in a state like SETUP. Just prepare it.
				if prepErr := p.Prepare(); prepErr != nil {
					return offset, prepErr
				}

				continue // Retry now that the stream is PREPARED.
			}

			return offset, err
		}

		if framesToCopy == 0 {
			// If the stream is running but has no data, we must wait.
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
					if recoveryErr := p.xrunRecover(waitErr); recoveryErr != nil {
						return offset, recoveryErr
					}

					continue
				}

				return offset, fmt.Errorf("pcm wait failed: %w", waitErr)
			}

			if !ready { // Timeout or stream not ready
				if (p.flags & PCM_NONBLOCK) != 0 {
					return offset, syscall.EAGAIN
				}

				if p.State() == SNDRV_PCM_STATE_SETUP {
					if err := p.Prepare(); err != nil {
						return offset, err
					}
				}

				continue
			}

			// After a successful wait, continue loop to try MmapBegin again.
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
		err = fmt.Errorf("method MmapBegin() is only available for MMAP streams")
		return
	}

	currentState := p.State()

	switch currentState {
	case SNDRV_PCM_STATE_XRUN:
		err = syscall.EPIPE
		return
	case SNDRV_PCM_STATE_OPEN, SNDRV_PCM_STATE_SETUP, SNDRV_PCM_STATE_DRAINING:
		err = unix.EBADFD
		return
	case SNDRV_PCM_STATE_SUSPENDED:
		err = syscall.ESTRPIPE
		return
	case SNDRV_PCM_STATE_DISCONNECTED:
		err = syscall.ENODEV
		return
	}

	var applPtr, hwPtr SndPcmUframesT
	if unsafe.Sizeof(applPtr) == 8 {
		applPtr = SndPcmUframesT(atomic.LoadUint64((*uint64)(unsafe.Pointer(&p.mmapControl.ApplPtr))))
		hwPtr = SndPcmUframesT(atomic.LoadUint64((*uint64)(unsafe.Pointer(&p.mmapStatus.HwPtr))))
	} else {
		applPtr = SndPcmUframesT(atomic.LoadUint32((*uint32)(unsafe.Pointer(&p.mmapControl.ApplPtr))))
		hwPtr = SndPcmUframesT(atomic.LoadUint32((*uint32)(unsafe.Pointer(&p.mmapStatus.HwPtr))))
	}

	var availS SndPcmSframesT
	if (p.flags & PCM_IN) != 0 {
		// Capture: available frames are hw_ptr - appl_ptr
		availS = SndPcmSframesT(hwPtr) - SndPcmSframesT(applPtr)
		if availS < 0 {
			availS += SndPcmSframesT(p.boundary)
		}
	} else {
		// Playback: used frames are appl_ptr - hw_ptr
		used := SndPcmSframesT(applPtr) - SndPcmSframesT(hwPtr)
		if used < 0 {
			used += SndPcmSframesT(p.boundary)
		}
		// Available frames are buffer_size - used
		availS = SndPcmSframesT(p.bufferSize) - used
	}

	avail = SndPcmUframesT(availS)
	if wantFrames > uint32(avail) {
		wantFrames = uint32(avail)
	}

	offsetFrames = uint32(applPtr % SndPcmUframesT(p.bufferSize))
	continuousFrames := p.bufferSize - offsetFrames
	framesToCopy := wantFrames

	if framesToCopy > continuousFrames {
		framesToCopy = continuousFrames
	}

	actualFrames = framesToCopy

	frameSize := uint64(p.FrameSize())
	byteOffset := uint64(offsetFrames) * frameSize
	byteCount := uint64(actualFrames) * frameSize

	if byteOffset+byteCount > uint64(len(p.mmapBuffer)) {
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
		return fmt.Errorf("method MmapCommit() is only available for MMAP streams")
	}

	var applPtr SndPcmUframesT
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

	if ptrSize == 8 {
		atomic.StoreUint64((*uint64)(unsafe.Pointer(&p.mmapControl.ApplPtr)), uint64(newApplPtr))
	} else {
		atomic.StoreUint32((*uint32)(unsafe.Pointer(&p.mmapControl.ApplPtr)), uint32(newApplPtr))
	}

	// After updating the application pointer, we must notify the kernel and get the latest state back.
	return p.syncPtr(SNDRV_PCM_SYNC_PTR_APPL | SNDRV_PCM_SYNC_PTR_HWSYNC)
}

// Timestamp returns available frames and the corresponding timestamp.
// The clock source is CLOCK_MONOTONIC if the PCM_MONOTONIC flag was used, otherwise it is CLOCK_REALTIME.
// This method is only available for MMAP streams.
func (p *PCM) Timestamp() (availFrames uint32, t time.Time, err error) {
	if (p.flags & PCM_MMAP) == 0 {
		err = fmt.Errorf("method Timestamp() is only available for MMAP streams")

		return
	}

	tmp, err := p.AvailUpdate()
	if err != nil {
		return
	}

	availFrames = uint32(tmp)

	ts := p.mmapStatus.Tstamp
	t = time.Unix(int64(ts.Sec), int64(ts.Nsec))

	return
}

// HWPtr returns the current hardware pointer position and the corresponding timestamp.
// This method is only available for MMAP streams.
func (p *PCM) HWPtr() (hwPtr SndPcmUframesT, t time.Time, err error) {
	if !p.IsReady() {
		err = fmt.Errorf("PCM is not ready")

		return
	}

	if (p.flags & PCM_MMAP) == 0 {
		err = fmt.Errorf("method HWPtr() is only available for MMAP streams")

		return
	}

	if ioctlErr := p.syncPtr(SNDRV_PCM_SYNC_PTR_HWSYNC); ioctlErr != nil {
		err = fmt.Errorf("ioctl HWSYNC failed: %w", ioctlErr)
	}

	if unsafe.Sizeof(hwPtr) == 8 {
		hwPtr = SndPcmUframesT(atomic.LoadUint64((*uint64)(unsafe.Pointer(&p.mmapStatus.HwPtr))))
	} else {
		hwPtr = SndPcmUframesT(atomic.LoadUint32((*uint32)(unsafe.Pointer(&p.mmapStatus.HwPtr))))
	}

	ts := p.mmapStatus.Tstamp
	t = time.Unix(int64(ts.Sec), int64(ts.Nsec))

	return
}
