package alsa

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// MmapWrite writes interleaved audio data to a playback MMAP PCM device.
// The provided `data` must be a slice of a supported numeric type (e.g., []int16, []float32).
// Returns the number of frames actually written.
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

	defer runtime.KeepAlive(data)

	s := p.State()
	if s == SNDRV_PCM_STATE_SETUP {
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

		avail, availErr := p.AvailUpdate()
		if availErr != nil {
			return offset, fmt.Errorf("AvailUpdate failed: %w", availErr)
		}

		buffer, _, framesToCopy, _, err := p.mmapBegin(wantFrames)
		if err != nil {
			return offset, err
		}

		if framesToCopy == 0 {
			// If the stream is running but has no data, we must wait.
			timeout := -1

			if (p.flags & PCM_NONBLOCK) != 0 {
				return offset, syscall.EAGAIN
			}

			if (p.flags & PCM_NOIRQ) != 0 {
				if uint32(avail) < p.config.AvailMin {
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
				return offset, fmt.Errorf("pcm wait failed: %w", waitErr)
			}

			if !ready { // Timeout or stream not ready
				continue
			}

			// After a successful wait, continue the loop to try mmapBegin again.
			continue
		}

		bytesToCopy := int(PcmFramesToBytes(p, framesToCopy))
		srcPtr := unsafe.Pointer(uintptr(dataPtr) + uintptr(offset))
		srcSlice := unsafe.Slice((*byte)(srcPtr), bytesToCopy)
		copy(buffer, srcSlice)

		// Update offset and remainingBytes immediately after successful copy, before mmapCommit.
		offset += bytesToCopy
		remainingBytes -= bytesToCopy

		if err := p.mmapCommit(framesToCopy); err != nil {
			return offset, err
		}

		if p.State() == SNDRV_PCM_STATE_PREPARED && p.bufferSize-uint32(avail) >= p.config.StartThreshold {
			if err := p.Start(); err != nil {
				return offset, err
			}
		}
	}

	return offset, nil
}

// MmapRead reads interleaved audio data from a capture MMAP PCM device.
// The provided `data` must be a slice of a supported numeric type (e.g., []int16, []float32) that will receive the data.
// Returns the number of frames actually read.
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

	defer runtime.KeepAlive(data)

	s := p.State()
	if s == SNDRV_PCM_STATE_SETUP {
		if err := p.Prepare(); err != nil {
			return 0, err
		}
	}

	offset := 0
	remainingBytes := int(dataByteLen)

	if p.State() == SNDRV_PCM_STATE_PREPARED && PcmBytesToFrames(p, dataByteLen) >= p.config.StartThreshold {
		if err := p.Start(); err != nil {
			return offset, err
		}
	}

	for remainingBytes > 0 {
		avail, availErr := p.AvailUpdate()
		if availErr != nil {
			return offset, fmt.Errorf("AvailUpdate failed: %w", availErr)
		}

		wantFrames := PcmBytesToFrames(p, uint32(remainingBytes))
		if wantFrames == 0 {
			break
		}

		buffer, _, framesToCopy, _, err := p.mmapBegin(wantFrames)
		if err != nil {
			return offset, err
		}

		if framesToCopy == 0 {
			// If the stream is running but has no data, we must wait.
			timeout := -1

			if (p.flags & PCM_NONBLOCK) != 0 {
				return offset, syscall.EAGAIN
			}

			if (p.flags & PCM_NOIRQ) != 0 {
				if uint32(avail) < p.config.AvailMin {
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
				return offset, fmt.Errorf("pcm wait failed: %w", waitErr)
			}

			if !ready { // Timeout or stream not ready
				continue
			}

			continue
		}

		bytesToCopy := int(PcmFramesToBytes(p, framesToCopy))
		dstPtr := unsafe.Pointer(uintptr(dataPtr) + uintptr(offset))
		dstSlice := unsafe.Slice((*byte)(dstPtr), bytesToCopy)
		copy(dstSlice, buffer)

		// Update offset and remainingBytes immediately after successful copy, before mmapCommit.
		offset += bytesToCopy
		remainingBytes -= bytesToCopy

		if err := p.mmapCommit(framesToCopy); err != nil {
			return offset, err
		}
	}

	return offset, nil
}

// mmapBegin prepares for a memory-mapped transfer. It returns a slice of the main buffer corresponding to the available contiguous
// space for writing or reading, the offset in frames from the start of the buffer, and the number of frames available in that slice.
func (p *PCM) mmapBegin(wantFrames uint32) (buffer []byte, offsetFrames, actualFrames uint32, avail sndPcmUframesT, err error) {
	var applPtr sndPcmUframesT
	if unsafe.Sizeof(applPtr) == 8 {
		applPtr = sndPcmUframesT(atomic.LoadUint64((*uint64)(unsafe.Pointer(&p.mmapControl.ApplPtr))))
	} else {
		applPtr = sndPcmUframesT(atomic.LoadUint32((*uint32)(unsafe.Pointer(&p.mmapControl.ApplPtr))))
	}

	tmp, availErr := p.AvailUpdate()
	if availErr != nil {
		err = availErr

		return
	}

	avail = sndPcmUframesT(tmp)
	if wantFrames > uint32(avail) {
		wantFrames = uint32(avail)
	}

	offsetFrames = uint32(applPtr % sndPcmUframesT(p.bufferSize))
	continuousFrames := p.bufferSize - offsetFrames
	framesToCopy := wantFrames

	if framesToCopy > continuousFrames {
		framesToCopy = continuousFrames
	}

	actualFrames = framesToCopy

	frameSize := uint64(p.FrameSize())
	byteOffset := uint64(offsetFrames) * frameSize
	byteCount := uint64(actualFrames) * frameSize

	if byteCount > 0 {
		buffer = p.mmapBuffer[byteOffset : byteOffset+byteCount]
	}

	return
}

// mmapCommit commits the number of frames transferred after a mmapBegin call.
func (p *PCM) mmapCommit(frames uint32) error {
	var applPtr sndPcmUframesT
	ptrSize := unsafe.Sizeof(applPtr)

	var currentApplPtr sndPcmUframesT
	if ptrSize == 8 {
		currentApplPtr = sndPcmUframesT(atomic.LoadUint64((*uint64)(unsafe.Pointer(&p.mmapControl.ApplPtr))))
	} else {
		currentApplPtr = sndPcmUframesT(atomic.LoadUint32((*uint32)(unsafe.Pointer(&p.mmapControl.ApplPtr))))
	}

	newApplPtr := currentApplPtr + sndPcmUframesT(frames)
	if p.boundary > 0 && newApplPtr >= p.boundary {
		newApplPtr -= p.boundary
	}

	if ptrSize == 8 {
		atomic.StoreUint64((*uint64)(unsafe.Pointer(&p.mmapControl.ApplPtr)), uint64(newApplPtr))
	} else {
		atomic.StoreUint32((*uint32)(unsafe.Pointer(&p.mmapControl.ApplPtr)), uint32(newApplPtr))
	}

	// After updating the application pointer, we must notify the kernel.
	return p.syncPtr(0)
}
