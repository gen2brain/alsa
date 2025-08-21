package alsa

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"syscall"
	"unsafe"
)

// WriteI writes interleaved audio data to a playback PCM device using an ioctl call.
// The provided `data` argument must be a slice of a supported numeric type (e.g., []int16, []float32).
// Returns the number of frames actually written.
func (p *PCM) WriteI(data any, frames uint32) (int, error) {
	if (p.flags & PCM_IN) != 0 {
		return 0, fmt.Errorf("cannot write to a capture device")
	}

	byteLen, err := checkSlice(data)
	if err != nil {
		return 0, fmt.Errorf("invalid data type for WriteI: %w", err)
	}

	requiredBytes := PcmFramesToBytes(p, frames)
	if byteLen < requiredBytes {
		return 0, fmt.Errorf("data buffer too small: needs %d bytes, got %d", requiredBytes, byteLen)
	}

	if frames == 0 || requiredBytes == 0 {
		return 0, nil
	}

	defer runtime.KeepAlive(data)

	s := p.State()
	if s == SNDRV_PCM_STATE_SETUP {
		if err := p.Prepare(); err != nil {
			return 0, err
		}
	}

	dataPtr := uintptr(0)
	if reflect.ValueOf(data).Len() > 0 {
		dataPtr = reflect.ValueOf(data).Index(0).Addr().Pointer()
	}

	framesWritten := uint32(0)
	for framesWritten < frames {
		remainingFrames := frames - framesWritten
		offsetBytes := PcmFramesToBytes(p, framesWritten)

		xfer := sndXferi{
			Frames: SndPcmUframesT(remainingFrames),
			Buf:    dataPtr + uintptr(offsetBytes),
		}

		err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_WRITEI_FRAMES, uintptr(unsafe.Pointer(&xfer)))

		if xfer.Result > 0 {
			framesWritten += uint32(xfer.Result)
		}

		if err != nil {
			// For ESTRPIPE, try to recover if not disabled. EPIPE will just be counted.
			if (p.flags&PCM_NORESTART) == 0 && (errors.Is(err, syscall.ESTRPIPE) || errors.Is(err, syscall.EPIPE)) {
				if errRec := p.xrunRecover(err); errRec != nil {
					return int(framesWritten), errRec
				}

				// Recovery succeeded, continue the loop to retry writing.
				continue
			}

			// For non-blocking mode, EAGAIN means the buffer is full.
			if errors.Is(err, syscall.EAGAIN) && (p.flags&PCM_NONBLOCK) != 0 {
				break
			}

			return int(framesWritten), fmt.Errorf("ioctl WRITEI_FRAMES failed: %w", err)
		}
	}

	return int(framesWritten), nil
}

// ReadI reads interleaved audio data from a capture PCM device using an ioctl call.
// The provided `buffer` must be a slice of a supported numeric type (e.g., []int16, []float32).
// Returns the number of frames actually read.
func (p *PCM) ReadI(data any, frames uint32) (int, error) {
	if (p.flags & PCM_IN) == 0 {
		return 0, fmt.Errorf("cannot read from a playback device")
	}

	if (p.flags & PCM_MMAP) != 0 {
		return 0, fmt.Errorf("use MmapRead for mmap devices")
	}

	byteLen, err := checkSlice(data)
	if err != nil {
		return 0, fmt.Errorf("invalid buffer type for ReadI: %w", err)
	}

	requiredBytes := PcmFramesToBytes(p, frames)
	if byteLen < requiredBytes {
		return 0, fmt.Errorf("buffer too small: needs %d bytes, got %d", requiredBytes, byteLen)
	}

	if frames == 0 || requiredBytes == 0 {
		return 0, nil
	}

	defer runtime.KeepAlive(data)

	s := p.State()
	if s == SNDRV_PCM_STATE_SETUP {
		if err := p.Prepare(); err != nil {
			return 0, err
		}
	}

	bufferPtr := uintptr(0)
	if reflect.ValueOf(data).Len() > 0 {
		bufferPtr = reflect.ValueOf(data).Index(0).Addr().Pointer()
	}

	framesRead := uint32(0)
	for framesRead < frames {
		remainingFrames := frames - framesRead
		offsetBytes := PcmFramesToBytes(p, framesRead)

		xfer := sndXferi{
			Frames: SndPcmUframesT(remainingFrames),
			Buf:    bufferPtr + uintptr(offsetBytes),
		}

		err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_READI_FRAMES, uintptr(unsafe.Pointer(&xfer)))

		if xfer.Result > 0 {
			framesRead += uint32(xfer.Result)
		}

		if err != nil {
			// For ESTRPIPE, try to recover if not disabled. EPIPE will just be counted.
			if (p.flags&PCM_NORESTART) == 0 && (errors.Is(err, syscall.ESTRPIPE) || errors.Is(err, syscall.EPIPE)) {
				if errRec := p.xrunRecover(err); errRec != nil {
					return int(framesRead), errRec
				}

				continue
			}

			// For non-blocking mode, EAGAIN means no data is available.
			if (p.flags&PCM_NONBLOCK) != 0 && errors.Is(err, syscall.EAGAIN) {
				return int(framesRead), syscall.EAGAIN
			}

			return int(framesRead), fmt.Errorf("ioctl READI_FRAMES failed: %w", err)
		}
	}

	return int(framesRead), nil
}

// Write writes audio samples to a PCM device. It calculates the number of frames based on the input slice size and calls WriteI.
// The provided `data` must be a slice of a supported numeric type (e.g., []int16, []float32).
func (p *PCM) Write(data any) error {
	byteLen, err := checkSlice(data)
	if err != nil {
		return fmt.Errorf("invalid data type for Write: %w", err)
	}

	frames := PcmBytesToFrames(p, byteLen)

	ret, err := p.WriteI(data, frames)
	if err != nil {
		return err
	}

	if uint32(ret) != frames {
		return fmt.Errorf("failed to write all frames: %w", syscall.EIO)
	}

	return nil
}

// Read reads audio samples from a PCM device. It calculates the number of frames based on the buffer slice size and calls ReadI.
// The provided `data` must be a slice of a supported numeric type (e.g., []int16, []float32).
func (p *PCM) Read(data any) error {
	byteLen, err := checkSlice(data)
	if err != nil {
		return fmt.Errorf("invalid data type for Read: %w", err)
	}

	frames := PcmBytesToFrames(p, byteLen)

	ret, err := p.ReadI(data, frames)
	if err != nil {
		return err
	}

	if uint32(ret) != frames {
		return fmt.Errorf("failed to read all frames: %w", syscall.EIO)
	}

	return nil
}

// checkSlice validates that the input is a slice of a supported numeric type.
// It returns the total length of the slice data in bytes.
func checkSlice(data any) (byteLen uint32, err error) {
	if data == nil {
		return 0, errors.New("data cannot be nil")
	}

	rv := reflect.ValueOf(data)
	if rv.Kind() != reflect.Slice {
		return 0, fmt.Errorf("expected a slice, got %T", data)
	}

	if rv.Len() == 0 {
		return 0, nil
	}

	switch rv.Type().Elem().Kind() {
	case reflect.Int8, reflect.Uint8,
		reflect.Int16, reflect.Uint16,
		reflect.Int32, reflect.Uint32,
		reflect.Float32, reflect.Float64:
	default:
		return 0, fmt.Errorf("unsupported slice element type: %s", rv.Type().Elem().Kind())
	}

	return uint32(rv.Len()) * uint32(rv.Type().Elem().Size()), nil
}

// checkSliceAndGetData is a helper that combines slice validation and getting the data pointer.
func checkSliceAndGetData(data any) (ptr unsafe.Pointer, byteLen uint32, err error) {
	byteLen, err = checkSlice(data)
	if err != nil {
		return nil, 0, err
	}

	if byteLen > 0 {
		ptr = unsafe.Pointer(reflect.ValueOf(data).Index(0).Addr().Pointer())
	}

	return ptr, byteLen, nil
}
