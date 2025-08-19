package alsa

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Config encapsulates the hardware and software parameters of a PCM stream.
type Config struct {
	Channels         uint32
	Rate             uint32
	PeriodSize       uint32
	PeriodCount      uint32
	Format           PcmFormat
	StartThreshold   uint32
	StopThreshold    uint32
	SilenceThreshold SndPcmUframesT
	SilenceSize      SndPcmUframesT
	AvailMin         uint32
}

// PCM represents an open ALSA PCM device handle.
type PCM struct {
	file               *os.File
	config             Config
	flags              PcmFlag
	bufferSize         uint32 // In frames
	subdevice          uint32
	mmapBuffer         []byte
	mmapStatus         *sndPcmMmapStatus
	mmapControl        *sndPcmMmapControl
	syncPtr            *sndPcmSyncPtr // Used for non-mmap streams or if mmap for status/control fails
	syncPtrIsMmapped   bool
	boundary           SndPcmUframesT
	xruns              int    // Counter for overruns/underruns
	noirqFramesPerMsec uint32 // For PCM_NOIRQ calculation
	state              int32  // PcmState: Use getState/setState for atomic access.
	lastError          string // For pcm_get_error() equivalent
	noSyncPtr          bool   // Flag to indicate that SYNC_PTR is not supported.
}

// PcmParams represents the hardware capabilities of a PCM device.
type PcmParams struct {
	params *sndPcmHwParams
}

// PcmOpenByName opens a PCM by its name, in the format "hw:C,D".
func PcmOpenByName(name string, flags PcmFlag, config *Config) (*PCM, error) {
	if !strings.HasPrefix(name, "hw:") {
		return nil, fmt.Errorf("invalid PCM name format: missing 'hw:' prefix")
	}

	parts := strings.Split(strings.TrimPrefix(name, "hw:"), ",")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid PCM name format: expected 'hw:card,device'")
	}

	card, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid card number '%s': %w", parts[0], err)
	}

	device, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid device number '%s': %w", parts[1], err)
	}

	return PcmOpen(uint(card), uint(device), flags, config)
}

// PcmOpen opens an ALSA PCM device and configures it according to the provided parameters.
// Note: This implementation does not support the ALSA plugin architecture and will only open direct hardware PCM devices (e.g., /dev/snd/pcmC0D0p).
func PcmOpen(card, device uint, flags PcmFlag, config *Config) (*PCM, error) {
	var streamChar byte
	if (flags & PCM_IN) != 0 {
		streamChar = 'c' // Capture
	} else {
		streamChar = 'p' // Playback
	}

	path := fmt.Sprintf("/dev/snd/pcmC%dD%d%c", card, device, streamChar)

	// Always open non-blocking to avoid getting stuck
	// if the device is in use, then clear the flag if blocking I/O was requested.
	file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open PCM device %s: %w", path, err)
	}

	// If blocking mode is requested, clear the O_NONBLOCK flag using fcntl.
	if (flags & PCM_NONBLOCK) == 0 {
		currentFlags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
		if err != nil {
			file.Close()
			return nil, fmt.Errorf("fcntl F_GETFL for %s failed: %w", path, err)
		}
		if _, err = unix.FcntlInt(file.Fd(), unix.F_SETFL, currentFlags&^syscall.O_NONBLOCK); err != nil {
			file.Close()
			return nil, fmt.Errorf("failed to set blocking mode on %s: %w", path, err)
		}
	}

	var info sndPcmInfo
	if err := ioctl(file.Fd(), SNDRV_PCM_IOCTL_INFO, uintptr(unsafe.Pointer(&info))); err != nil {
		file.Close()

		return nil, fmt.Errorf("ioctl INFO failed: %w", err)
	}

	pcm := &PCM{
		file:      file,
		flags:     flags,
		subdevice: info.Subdevice,
		noSyncPtr: false,
		state:     int32(PCM_STATE_OPEN),
	}

	// If no config is provided, use a default configuration.
	// The start/stop thresholds are left at 0 to be calculated in SetConfig.
	var finalConfig Config
	if config == nil {
		finalConfig = Config{
			Channels:    2,
			Rate:        48000,
			PeriodSize:  1024,
			PeriodCount: 4,
			Format:      PCM_FORMAT_S16_LE,
		}
	} else {
		finalConfig = *config
	}

	if err := pcm.SetConfig(&finalConfig); err != nil {
		pcm.Close()

		return nil, fmt.Errorf("failed to set PCM config: %w", err)
	}

	// This is critical: status/control structures (or the syncPtr fallback)
	// must be initialized for ALL streams to allow state checking.
	if err := pcm.setupStatusAndControl(); err != nil {
		pcm.Close()

		return nil, fmt.Errorf("failed to set up status and control: %w", err)
	}

	// Set timestamp type if requested.
	if (flags & PCM_MONOTONIC) != 0 {
		// SNDRV_PCM_TSTAMP_TYPE_MONOTONIC = 1
		var arg int32 = 1
		if err := ioctl(pcm.file.Fd(), SNDRV_PCM_IOCTL_TTSTAMP, uintptr(unsafe.Pointer(&arg))); err != nil {
			pcm.Close()

			return nil, fmt.Errorf("ioctl TTSTAMP failed: %w", err)
		}
	}

	return pcm, nil
}

// IsReady checks if the PCM handle is valid.
func (p *PCM) IsReady() bool {
	return p != nil && p.file != nil
}

// Close closes the PCM device handle and releases all associated resources.
func (p *PCM) Close() error {
	if !p.IsReady() {
		return nil
	}

	// The kernel driver is responsible for stopping the stream and cleaning up
	// resources when the file descriptor is closed. Explicitly calling DROP
	// or another state-changing ioctl here can interfere with subsequent
	// operations if the stream is in an unexpected state.

	p.unmapStatusAndControl()

	if p.mmapBuffer != nil {
		_ = unix.Munmap(p.mmapBuffer)
		p.mmapBuffer = nil
	}

	err := p.file.Close()
	p.file = nil

	return err
}

// Config returns a copy of the PCM's current configuration.
func (p *PCM) Config() Config {
	return p.config
}

// BufferSize returns the PCM's total buffer size in frames.
func (p *PCM) BufferSize() uint32 {
	return p.bufferSize
}

// Flags returns the sample flags of the PCM stream.
func (p *PCM) Flags() PcmFlag {
	return p.flags
}

// PeriodSize returns the number of frames per period.
func (p *PCM) PeriodSize() uint32 {
	return p.config.PeriodSize
}

// PeriodCount returns the number of periods in the buffer.
func (p *PCM) PeriodCount() uint32 {
	return p.config.PeriodCount
}

// Channels returns the number of channels for the PCM stream.
func (p *PCM) Channels() uint32 {
	return p.config.Channels
}

// Rate returns the sample rate of the PCM stream in Hz.
func (p *PCM) Rate() uint32 {
	return p.config.Rate
}

// Format returns the sample format of the PCM stream.
func (p *PCM) Format() PcmFormat {
	return p.config.Format
}

// Fd returns the underlying file descriptor for the PCM device.
func (p *PCM) Fd() uintptr {
	if !p.IsReady() {
		return ^uintptr(0) // Invalid FD
	}

	return p.file.Fd()
}

// Subdevice returns the subdevice number of the PCM stream.
func (p *PCM) Subdevice() uint32 {
	return p.subdevice
}

// Xruns returns the number of buffer underruns (for playback) or overruns (for capture) that have occurred.
// An xrun is a serious error that indicates the hardware buffer was not serviced in time, leading to an audible glitch.
func (p *PCM) Xruns() int {
	return p.xruns
}

// FrameSize returns the size of a single frame in bytes.
// A frame contains one sample for each channel.
func (p *PCM) FrameSize() uint32 {
	bitsPerSample := PcmFormatToBits(p.config.Format)
	if bitsPerSample == 0 {
		return 0
	}

	return p.config.Channels * (bitsPerSample / 8)
}

// PeriodTime returns the duration of a single period.
func (p *PCM) PeriodTime() time.Duration {
	rate := p.Rate()
	if rate == 0 {
		return 0
	}

	// Duration in nanoseconds = (frames_per_period * 1,000,000,000) / frames_per_second
	ns := (1e9 * float64(p.PeriodSize())) / float64(rate)

	return time.Duration(ns)
}

// SetConfig sets the hardware and software parameters for the PCM device.
// This function should be called before the stream is started. If called on an already configured stream,
// it will attempt to reconfigure it, which requires freeing existing hardware parameters.
func (p *PCM) SetConfig(config *Config) error {
	// If re-configuring, we must free the existing hardware parameters first.
	// This is required if the state is SETUP or later (PREPARED, RUNNING, etc.).
	currentState := p.getState()
	if currentState != PCM_STATE_OPEN {
		if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_HW_FREE, 0); err != nil {
			// If HW_FREE fails (e.g., EBUSY if running), we cannot proceed with HW_PARAMS safely.
			// The user should ideally call Stop() before SetConfig() if the stream is running.
			return fmt.Errorf("ioctl HW_FREE failed during reconfiguration (stream might be busy): %w", err)
		}

		// Successfully freed HW params, reset state to OPEN conceptually.
		p.setState(PCM_STATE_OPEN)
	}

	// If re-configuring, unmap the old data buffer to avoid resource leaks.
	if p.mmapBuffer != nil {
		_ = unix.Munmap(p.mmapBuffer)
		p.mmapBuffer = nil
	}

	p.config = *config

	// 1. HW_PARAMS
	hwParams := &sndPcmHwParams{}
	paramInit(hwParams)

	if (p.flags & PCM_MMAP) != 0 {
		paramSetMask(hwParams, PCM_PARAM_ACCESS, SNDRV_PCM_ACCESS_MMAP_INTERLEAVED)
	} else {
		if (p.flags & PCM_NONINTERLEAVED) != 0 {
			paramSetMask(hwParams, PCM_PARAM_ACCESS, SNDRV_PCM_ACCESS_RW_NONINTERLEAVED)
		} else {
			paramSetMask(hwParams, PCM_PARAM_ACCESS, SNDRV_PCM_ACCESS_RW_INTERLEAVED)
		}
	}

	paramSetMask(hwParams, PCM_PARAM_FORMAT, uint32(config.Format))
	paramSetMask(hwParams, PCM_PARAM_SUBFORMAT, 0) // SNDRV_PCM_SUBFORMAT_STD
	paramSetInt(hwParams, PCM_PARAM_CHANNELS, config.Channels)

	// Use paramSetMin for Rate to be more flexible. This asks the driver for a rate
	// of *at least* the requested value, allowing it to choose the nearest supported rate.
	paramSetMin(hwParams, PCM_PARAM_RATE, config.Rate)

	paramSetInt(hwParams, PCM_PARAM_PERIODS, config.PeriodCount)
	paramSetInt(hwParams, PCM_PARAM_PERIOD_SIZE, config.PeriodSize)

	if (p.flags & PCM_NOIRQ) != 0 {
		if (p.flags & PCM_MMAP) == 0 {
			return fmt.Errorf("PCM_NOIRQ is only supported with PCM_MMAP")
		}

		// SNDRV_PCM_HW_PARAMS_NO_PERIOD_WAKEUP = (1<<2)
		hwParams.Flags |= 1 << 2
		if config.Rate > 0 {
			p.noirqFramesPerMsec = config.Rate / 1000
		}
	}

	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_HW_PARAMS, uintptr(unsafe.Pointer(hwParams))); err != nil {
		return fmt.Errorf("ioctl HW_PARAMS failed: %w", err)
	}

	// Update our config with the refined parameters from the driver.
	p.config.Channels = paramGetInt(hwParams, PCM_PARAM_CHANNELS)
	p.config.Rate = paramGetInt(hwParams, PCM_PARAM_RATE)
	p.config.PeriodSize = paramGetInt(hwParams, PCM_PARAM_PERIOD_SIZE)
	p.config.PeriodCount = paramGetInt(hwParams, PCM_PARAM_PERIODS)
	p.bufferSize = p.config.PeriodSize * p.config.PeriodCount

	// Sanity check for essential parameters
	if p.config.Channels == 0 || p.config.Rate == 0 || p.config.PeriodSize == 0 || p.config.PeriodCount == 0 {
		return fmt.Errorf("driver finalized invalid PCM configuration (Channels=%d, Rate=%d, PeriodSize=%d, PeriodCount=%d)",
			p.config.Channels, p.config.Rate, p.config.PeriodSize, p.config.PeriodCount)
	}

	// 2. SW_PARAMS
	swParams := &sndPcmSwParams{}
	swParams.TstampMode = 1 // SNDRV_PCM_TSTAMP_ENABLE
	swParams.PeriodStep = 1

	if config.AvailMin == 0 {
		p.config.AvailMin = p.config.PeriodSize
	} else {
		p.config.AvailMin = config.AvailMin
	}
	swParams.AvailMin = SndPcmUframesT(p.config.AvailMin)

	if config.StartThreshold == 0 {
		if (p.flags & PCM_IN) != 0 {
			p.config.StartThreshold = 1 // For capture, start immediately.
		} else {
			// For playback, the default is one period.
			p.config.StartThreshold = p.config.PeriodSize
		}
	} else {
		p.config.StartThreshold = config.StartThreshold
	}
	swParams.StartThreshold = SndPcmUframesT(p.config.StartThreshold)

	if config.StopThreshold == 0 {
		p.config.StopThreshold = p.bufferSize
	} else {
		p.config.StopThreshold = config.StopThreshold
	}
	swParams.StopThreshold = SndPcmUframesT(p.config.StopThreshold)

	// Pass through silence settings from the original config.
	swParams.SilenceThreshold = config.SilenceThreshold
	swParams.SilenceSize = config.SilenceSize

	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_SW_PARAMS, uintptr(unsafe.Pointer(swParams))); err != nil {
		return fmt.Errorf("ioctl SW_PARAMS (write) failed: %w", err)
	}

	p.boundary = swParams.Boundary

	// 3. MMAP Buffer (if applicable)
	// Mmap the data buffer after setting HW/SW_PARAMS, matching the standard ALSA flow (and tinyalsa).
	if (p.flags & PCM_MMAP) != 0 {
		frameSize := PcmFormatToBits(p.config.Format) / 8 * p.config.Channels
		mmapLen := int(p.bufferSize * uint32(frameSize))

		// Set the mmap protection flags based on the stream direction.
		var mmapProt int
		if (p.flags & PCM_IN) != 0 {
			// Capture requires PROT_READ.
			mmapProt = unix.PROT_READ
		} else {
			// Playback requires PROT_WRITE. Using PROT_READ|PROT_WRITE allows inspecting the buffer.
			mmapProt = unix.PROT_READ | unix.PROT_WRITE
		}

		buf, err := unix.Mmap(int(p.file.Fd()), 0, mmapLen, mmapProt, unix.MAP_SHARED)
		if err != nil {
			return fmt.Errorf("mmap data buffer failed: %w", err)
		}
		p.mmapBuffer = buf
	}

	// After HW_PARAMS and SW_PARAMS, the stream is in the SETUP state.
	// It must be explicitly prepared before starting.
	p.setState(PCM_STATE_SETUP)

	return nil
}

// WriteI writes interleaved audio data to a playback PCM device using an ioctl call.
// The provided `data` argument must be a slice of a supported numeric type (e.g., []int16, []float32).
// This is the idiomatic way to perform blocking I/O with ALSA.
// It automatically prepares and starts the stream, recovers from underruns (EPIPE),
// and loops until all requested frames have been written or an unrecoverable error occurs.
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

	// Keep the data slice alive for the duration of the system calls.
	defer runtime.KeepAlive(data)

	// Handle stream preparation and recovery proactively.
	s := p.getState()
	if s == PCM_STATE_XRUN {
		if (p.flags & PCM_NORESTART) != 0 {
			// If NORESTART is set, we must return EPIPE immediately.
			return 0, syscall.EPIPE
		}

		// If auto-restart is enabled, attempt recovery now. We use EPIPE as the triggering error.
		if err := p.xrunRecover(syscall.EPIPE); err != nil {
			return 0, err
		}

		// Recovery successful, state is now PREPARED.
	} else if s != PCM_STATE_RUNNING && s != PCM_STATE_PREPARED {
		// Handle initial preparation if state is OPEN or SETUP.
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
			// For non-blocking mode, EAGAIN means the buffer is full.
			// Stop and return the frames written so far.
			if (p.flags&PCM_NONBLOCK) != 0 && errors.Is(err, syscall.EAGAIN) {
				break
			}

			// EINTR is a temporary interruption; just retry the operation.
			if errors.Is(err, syscall.EINTR) {
				continue
			}

			// EBADF is a terminal condition for this I/O operation, often indicating
			// the stream was closed by another thread. Propagate it to the caller.
			if errors.Is(err, syscall.EBADF) {
				return int(framesWritten), err
			}

			// For underruns (EPIPE), try to recover if not disabled and if the stream
			// was not intentionally stopped (which would put it in the SETUP state).
			if (p.flags&PCM_NORESTART) == 0 && errors.Is(err, syscall.EPIPE) && p.KernelState() != PCM_STATE_SETUP {
				if errRec := p.xrunRecover(err); errRec != nil {
					// Recovery failed, return what we've written and the error.
					return int(framesWritten), errRec
				}

				// Recovery succeeded, continue the loop to retry writing.
				continue
			}

			// For any other error, it's unrecoverable.
			p.setState(PCM_STATE_XRUN)

			return int(framesWritten), fmt.Errorf("ioctl WRITEI_FRAMES failed: %w", err)
		}

		if p.getState() != PCM_STATE_RUNNING {
			p.setState(PCM_STATE_RUNNING)
		}
	}

	return int(framesWritten), nil
}

// WriteN writes non-interleaved audio data to a playback PCM device. This function is for non-MMAP, non-interleaved access.
// It is less common than interleaved I/O. The provided `data` argument must be a slice of slices (e.g., `[][]int16`),
// with each inner slice representing a channel and containing 'frames' worth of sample data.
// Like WriteI, it handles underruns by preparing and retrying the write.
// Returns the number of frames actually written.
func (p *PCM) WriteN(data any, frames uint32) (int, error) {
	if (p.flags & PCM_IN) != 0 {
		return 0, fmt.Errorf("cannot write to a capture device")
	}

	if (p.flags & PCM_MMAP) != 0 {
		return 0, fmt.Errorf("WriteN is not for mmap devices")
	}

	pointers, err := checkSliceOfSlices(p, data, frames)
	if err != nil {
		return 0, fmt.Errorf("invalid data type for WriteN: %w", err)
	}

	if frames == 0 {
		return 0, nil
	}

	defer runtime.KeepAlive(data)

	// Handle stream preparation and recovery proactively.
	s := p.getState()
	if s == PCM_STATE_XRUN {
		if (p.flags & PCM_NORESTART) != 0 {
			return 0, syscall.EPIPE
		}
		// Attempt recovery if auto-restart is enabled.
		if err := p.xrunRecover(syscall.EPIPE); err != nil {
			return 0, err
		}
	} else if s != PCM_STATE_RUNNING && s != PCM_STATE_PREPARED {
		if err := p.Prepare(); err != nil {
			return 0, err
		}
	}

	defer runtime.KeepAlive(pointers)

	bytesPerFramePerChannel := p.FrameSize() / p.config.Channels
	framesWritten := uint32(0)
	for framesWritten < frames {
		remainingFrames := frames - framesWritten

		xfer := sndXfern{
			Frames: SndPcmUframesT(remainingFrames),
			Bufs:   uintptr(unsafe.Pointer(&pointers[0])),
		}

		err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_WRITEN_FRAMES, uintptr(unsafe.Pointer(&xfer)))

		if xfer.Result > 0 {
			framesAdvanced := uint32(xfer.Result)
			framesWritten += framesAdvanced

			// Update pointers for the next iteration
			if framesWritten < frames {
				offsetBytes := framesAdvanced * bytesPerFramePerChannel
				for i := range pointers {
					pointers[i] += uintptr(offsetBytes)
				}
			}
		}

		if err != nil {
			if (p.flags&PCM_NONBLOCK) != 0 && errors.Is(err, syscall.EAGAIN) {
				break
			}

			if errors.Is(err, syscall.EINTR) {
				continue
			}

			if errors.Is(err, syscall.EBADF) {
				return int(framesWritten), err
			}

			if (p.flags&PCM_NORESTART) == 0 && errors.Is(err, syscall.EPIPE) && p.KernelState() != PCM_STATE_SETUP {
				if errRec := p.xrunRecover(err); errRec != nil {
					return int(framesWritten), errRec
				}

				continue
			}

			p.setState(PCM_STATE_XRUN)

			return int(framesWritten), fmt.Errorf("ioctl WRITEN_FRAMES failed: %w", err)
		}

		if p.getState() != PCM_STATE_RUNNING {
			p.setState(PCM_STATE_RUNNING)
		}
	}

	return int(framesWritten), nil
}

// ReadI reads interleaved audio data from a capture PCM device using an ioctl call.
// The provided `buffer` must be a slice of a supported numeric type (e.g., []int16, []float32).
// This is the idiomatic way to perform blocking I/O with ALSA.
// It automatically starts the stream, recovers from overruns (EPIPE),
// and loops until the buffer is filled with the requested number of frames or an unrecoverable error occurs.
// Returns the number of frames actually read.
func (p *PCM) ReadI(buffer any, frames uint32) (int, error) {
	if (p.flags & PCM_IN) == 0 {
		return 0, fmt.Errorf("cannot read from a playback device")
	}

	if (p.flags & PCM_MMAP) != 0 {
		return 0, fmt.Errorf("use MmapRead for mmap devices")
	}

	byteLen, err := checkSlice(buffer)
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

	defer runtime.KeepAlive(buffer)

	// Handle stream preparation and recovery proactively.
	s := p.getState()
	if s == PCM_STATE_XRUN {
		if (p.flags & PCM_NORESTART) != 0 {
			return 0, syscall.EPIPE
		}

		// Attempt recovery if auto-restart is enabled. For capture, xrunRecover will call Start() if non-MMAP.
		if err := p.xrunRecover(syscall.EPIPE); err != nil {
			return 0, err
		}
	} else if s != PCM_STATE_RUNNING && s != PCM_STATE_PREPARED {
		if err := p.Prepare(); err != nil {
			return 0, err
		}
	}

	bufferPtr := uintptr(0)
	if reflect.ValueOf(buffer).Len() > 0 {
		bufferPtr = reflect.ValueOf(buffer).Index(0).Addr().Pointer()
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
			// For non-blocking mode, EAGAIN means no data is available.
			// Stop and return the frames read so far.
			if (p.flags&PCM_NONBLOCK) != 0 && errors.Is(err, syscall.EAGAIN) {
				break
			}

			// EINTR is a temporary interruption; just retry the operation.
			if errors.Is(err, syscall.EINTR) {
				continue
			}

			// EBADF is a terminal condition for this I/O operation, often indicating
			// the stream was closed by another thread. Propagate it to the caller.
			if errors.Is(err, syscall.EBADF) {
				return int(framesRead), err
			}

			// For overruns (EPIPE), try to recover if not disabled and if the stream
			// was not intentionally stopped (which would put it in the SETUP state).
			if (p.flags&PCM_NORESTART) == 0 && errors.Is(err, syscall.EPIPE) && p.KernelState() != PCM_STATE_SETUP {
				if errRec := p.xrunRecover(err); errRec != nil {
					return int(framesRead), errRec
				}

				continue
			}

			// For any other error, it's unrecoverable.
			p.setState(PCM_STATE_XRUN)

			return int(framesRead), fmt.Errorf("ioctl READI_FRAMES failed: %w", err)
		}
	}

	if p.getState() != PCM_STATE_RUNNING {
		p.setState(PCM_STATE_RUNNING)
	}

	return int(framesRead), nil
}

// ReadN reads non-interleaved audio data from a capture PCM device. This function is for non-MMAP, non-interleaved access.
// The provided `buffers` argument must be a slice of slices (e.g., `[][]int16`), with each inner slice being large enough
// to hold 'frames' of audio data for one channel. Like ReadI, it handles overruns by preparing and retrying the read.
// Returns the number of frames actually read.
func (p *PCM) ReadN(buffers any, frames uint32) (int, error) {
	if (p.flags & PCM_IN) == 0 {
		return 0, fmt.Errorf("cannot read from a playback device")
	}

	if (p.flags & PCM_MMAP) != 0 {
		return 0, fmt.Errorf("ReadN is not for mmap devices")
	}

	pointers, err := checkSliceOfSlices(p, buffers, frames)
	if err != nil {
		return 0, fmt.Errorf("invalid buffer type for ReadN: %w", err)
	}

	if frames == 0 {
		return 0, nil
	}

	defer runtime.KeepAlive(buffers)

	// Handle stream preparation and recovery proactively.
	s := p.getState()
	if s == PCM_STATE_XRUN {
		if (p.flags & PCM_NORESTART) != 0 {
			return 0, syscall.EPIPE
		}

		// Attempt recovery if auto-restart is enabled. For capture, xrunRecover will call Start() if non-MMAP.
		if err := p.xrunRecover(syscall.EPIPE); err != nil {
			return 0, err
		}
	} else if s != PCM_STATE_RUNNING && s != PCM_STATE_PREPARED {
		if err := p.Prepare(); err != nil {
			return 0, err
		}
	}

	defer runtime.KeepAlive(pointers)

	bytesPerFramePerChannel := p.FrameSize() / p.config.Channels
	framesRead := uint32(0)
	for framesRead < frames {
		remainingFrames := frames - framesRead

		xfer := sndXfern{
			Frames: SndPcmUframesT(remainingFrames),
			Bufs:   uintptr(unsafe.Pointer(&pointers[0])),
		}

		err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_READN_FRAMES, uintptr(unsafe.Pointer(&xfer)))

		if xfer.Result > 0 {
			framesAdvanced := uint32(xfer.Result)
			framesRead += framesAdvanced
			if framesRead < frames {
				offsetBytes := framesAdvanced * bytesPerFramePerChannel
				for i := range pointers {
					pointers[i] += uintptr(offsetBytes)
				}
			}
		}

		if err != nil {
			if (p.flags&PCM_NONBLOCK) != 0 && errors.Is(err, syscall.EAGAIN) {
				break
			}

			if errors.Is(err, syscall.EINTR) {
				continue
			}

			if errors.Is(err, syscall.EBADF) {
				return int(framesRead), err
			}

			if (p.flags&PCM_NORESTART) == 0 && errors.Is(err, syscall.EPIPE) && p.KernelState() != PCM_STATE_SETUP {
				if errRec := p.xrunRecover(err); errRec != nil {
					return int(framesRead), errRec
				}

				continue
			}

			p.setState(PCM_STATE_XRUN)

			return int(framesRead), fmt.Errorf("ioctl READN_FRAMES failed: %w", err)
		}
	}

	if p.getState() != PCM_STATE_RUNNING {
		p.setState(PCM_STATE_RUNNING)
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

// Prepare readies the PCM device for I/O operations.
// This is typically used to recover from an XRUN.
func (p *PCM) Prepare() error {
	s := p.getState()
	if s == PCM_STATE_PREPARED || s == PCM_STATE_RUNNING {
		// Calling prepare on an already prepared/running stream is an error in ALSA,
		// but we can treat it as a no-op to be robust, as the stream is already usable.
		return nil
	}

	// When recovering from an XRUN (EPIPE) or Bad State (EBADFD), the ioctl(PREPARE)
	// might fail transiently with EPIPE/EBADFD again, especially with linked streams
	// where the counterpart might still be active or in a transitional state.
	// We retry a few times with exponential backoff to allow the system to stabilize.
	const maxRetries = 8     // Increased retries for more robustness in concurrent scenarios.
	const initialBackoff = 1 // ms
	const maxBackoff = 50    // ms
	backoff := time.Duration(initialBackoff) * time.Millisecond

	var lastErr error

	for i := 0; i < maxRetries; i++ {
		// Optimization: Check the kernel state before attempting PREPARE.
		// It might have been prepared concurrently (e.g., by a linked stream).
		currentState := p.KernelState()
		if currentState == PCM_STATE_PREPARED || currentState == PCM_STATE_RUNNING {
			p.setState(currentState)

			return nil
		}

		err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_PREPARE, 0)
		if err == nil {
			p.setState(PCM_STATE_PREPARED)

			return nil
		}

		lastErr = err

		// For linked streams, one stream might be prepared by an operation on the other, causing this call to return EBUSY.
		// We must check the actual kernel state to synchronize our internal state correctly.
		if errors.Is(err, syscall.EBUSY) {
			// If EBUSY is returned, the stream is likely PREPARED or RUNNING.
			// Query the kernel state to confirm and update our internal state.
			kernelState := p.KernelState()
			if kernelState == PCM_STATE_RUNNING || kernelState == PCM_STATE_PREPARED {
				p.setState(kernelState)

				return nil
			}

			// If KernelState reports something else (e.g., XRUN, SETUP), it might be a transient state.
			// We treat EBUSY as a transient error and retry, hoping the state stabilizes.
			// Fall through to the retry logic below.
		}

		// Retry on transient errors (EPIPE, EBADFD) and EBUSY if the state wasn't confirmed as ready.
		if errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) || errors.Is(err, syscall.EBUSY) {
			// Wait briefly to allow the kernel and other threads (especially the linked stream handler) to stabilize the state.

			// A small initial delay for allowing linked streams to settle.
			if i == 0 {
				time.Sleep(time.Duration(initialBackoff) * time.Millisecond)
			} else {
				time.Sleep(backoff)
			}

			// Exponential backoff (doubling the wait time)
			backoff *= 2
			if backoff > time.Duration(maxBackoff)*time.Millisecond {
				backoff = time.Duration(maxBackoff) * time.Millisecond
			}

			continue
		}

		// If prepare fails with a non-transient error, break immediately.
		break
	}

	// If prepare fails after retries or with a fatal error, the stream remains in its previous state
	// (e.g., XRUN if recovering). We do NOT reset the state to SETUP, as that might mask an XRUN.

	// Update the error message to reflect that retries occurred.
	return fmt.Errorf("ioctl PREPARE failed after multiple retries: %w", lastErr)
}

// Start explicitly starts the PCM stream.
// It ensures the stream is prepared before starting.
func (p *PCM) Start() error {
	if p.getState() == PCM_STATE_RUNNING {
		return nil
	}

	if p.getState() != PCM_STATE_PREPARED {
		if err := p.Prepare(); err != nil {
			return err
		}
	}

	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_START, 0); err != nil {
		// If the stream is already running (e.g., started by a linked stream),
		// the kernel will return EBUSY. We treat this as a success and update our cached state to RUNNING.
		if errors.Is(err, syscall.EBUSY) {
			p.setState(PCM_STATE_RUNNING)

			return nil
		}

		if errors.Is(err, syscall.EPIPE) {
			p.xruns++
			p.setState(PCM_STATE_XRUN)

			return fmt.Errorf("ioctl START failed with xrun: %w", err)
		}

		// If START fails, the stream remains in the PREPARED state.
		// Do not reset to SETUP as the hardware is still configured.
		return fmt.Errorf("ioctl START failed: %w", err)
	}

	p.setState(PCM_STATE_RUNNING)

	return nil
}

// Stop abruptly stops the PCM stream, dropping any pending frames.
func (p *PCM) Stop() error {
	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_DROP, 0); err != nil {
		return fmt.Errorf("ioctl DROP failed: %w", err)
	}

	p.setState(PCM_STATE_SETUP)

	return nil
}

// Pause pauses or resumes the PCM stream.
// Calling Pause(true) on a running stream will pause it.
// Calling Pause(false) on a paused stream will resume it.
func (p *PCM) Pause(enable bool) error {
	var arg int32
	if enable {
		arg = 1 // Pause
	} else {
		arg = 0 // Resume
	}

	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_PAUSE, uintptr(arg)); err != nil {
		return fmt.Errorf("ioctl PAUSE failed: %w", err)
	}

	if enable {
		p.setState(PCM_STATE_PAUSED)
	} else {
		p.setState(PCM_STATE_RUNNING)
	}

	return nil
}

// Resume resumes a suspended PCM stream (system suspend, when state is SND_PCM_STATE_SUSPENDED).
func (p *PCM) Resume() error {
	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_RESUME, 0); err != nil {
		// EINTR is a possible outcome and should be retried by the caller if needed.
		return fmt.Errorf("ioctl RESUME failed: %w", err)
	}

	p.setState(PCM_STATE_PREPARED) // After resume, stream is PREPARED, not RUNNING

	return nil
}

// Drain waits for all pending frames in the buffer to be played.
// This is a blocking call and only applies to playback streams.
func (p *PCM) Drain() error {
	if (p.flags & PCM_IN) != 0 {
		return fmt.Errorf("drain cannot be called on a capture device")
	}

	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_DRAIN, 0); err != nil {
		p.setState(PCM_STATE_XRUN)
		return fmt.Errorf("ioctl DRAIN failed: %w", err)
	}

	p.setState(PCM_STATE_SETUP)

	return nil
}

// Link establishes a start/stop synchronization between two PCMs.
// After this function is called, the two PCMs will prepare, start and stop in sync.
func (p *PCM) Link(other *PCM) error {
	if !p.IsReady() || !other.IsReady() {
		return fmt.Errorf("both PCM handles must be valid")
	}

	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_LINK, other.file.Fd()); err != nil {
		return fmt.Errorf("ioctl LINK failed: %w", err)
	}

	return nil
}

// Unlink removes the synchronization between this PCM and others.
func (p *PCM) Unlink() error {
	if !p.IsReady() {
		return fmt.Errorf("PCM handle is not valid")
	}

	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_UNLINK, 0); err != nil {
		return fmt.Errorf("ioctl UNLINK failed: %w", err)
	}

	return nil
}

// Delay returns the current delay for the PCM stream in frames.
func (p *PCM) Delay() (int, error) {
	if !p.IsReady() {
		return 0, fmt.Errorf("PCM handle is not valid")
	}

	var delay int
	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_DELAY, uintptr(unsafe.Pointer(&delay))); err != nil {
		return 0, fmt.Errorf("ioctl DELAY failed: %w", err)
	}

	return delay, nil
}

// Wait waits for the PCM to become ready for I/O or until a timeout occurs.
// Returns true if the device is ready, false on timeout.
// On error, it may return a specific error code like EPIPE, ESTRPIPE or ENODEV.
func (p *PCM) Wait(timeoutMs int) (bool, error) {
	if !p.IsReady() {
		return false, fmt.Errorf("PCM handle not ready")
	}

	// Fast-path: if we already know we're in an error state, propagate that immediately.
	st := p.getState()
	if st == PCM_STATE_XRUN {
		// An XRUN has already been detected. The stream is stopped and requires recovery.
		// Return EPIPE to signal this, consistent with how I/O calls behave.
		return false, syscall.EPIPE
	}

	if st == PCM_STATE_SUSPENDED {
		return false, syscall.ESTRPIPE
	}

	if st == PCM_STATE_DISCONNECTED {
		return false, syscall.ENODEV
	}

	var events int16
	if (p.flags & PCM_IN) != 0 {
		events = unix.POLLIN
	} else {
		events = unix.POLLOUT
	}

	// Always poll for errors.
	events |= unix.POLLERR | unix.POLLNVAL | unix.POLLHUP

	pfd := []unix.PollFd{
		{Fd: int32(p.file.Fd()), Events: events},
	}

	if timeoutMs < 0 {
		timeoutMs = -1 // Block indefinitely
	}

	for {
		_, err := unix.Poll(pfd, timeoutMs)
		if err == nil {
			break // Success or timeout
		}
		if !errors.Is(err, syscall.EINTR) {
			return false, fmt.Errorf("poll failed: %w", err)
		}
		// Loop on EINTR
	}

	revents := pfd[0].Revents

	// Check for errors first. If an error is signaled by poll, we must
	// query the authoritative stream state to return the correct error code.
	if (revents & (unix.POLLERR | unix.POLLHUP | unix.POLLNVAL)) != 0 {
		return p.checkState()
	}

	// If poll indicates readiness for I/O, we can return true.
	// The subsequent I/O call (e.g., MmapBegin, WriteI) is responsible
	// for handling the stream's state at the moment of the operation.
	// This avoids a race condition where the state could change between
	// Wait() and the I/O call, which was the source of a deadlock.
	if (revents & (unix.POLLIN | unix.POLLOUT)) != 0 {
		return true, nil
	}

	// Poll returned, but no readiness or error flags are set. This means timeout.
	return false, nil
}

// KernelState returns the current state of the PCM stream by querying the kernel.
func (p *PCM) KernelState() PcmState {
	// If the status page is available (mmapped or fallback), read the state from it.
	if p.mmapStatus != nil {
		// Use atomic load to ensure synchronization (acquire barrier), especially for mmapped memory.
		return PcmState(atomic.LoadInt32((*int32)(unsafe.Pointer(&p.mmapStatus.State))))
	}

	// If status/control setup failed entirely (should not happen if PcmOpen succeeded),
	// we must use the STATUS ioctl as a last resort.
	var status sndPcmStatus
	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_STATUS, uintptr(unsafe.Pointer(&status))); err != nil {
		// If ioctl fails, assume disconnected or bad state.
		return PCM_STATE_DISCONNECTED
	}

	return status.State
}

// State returns the current cached state of the PCM stream.
func (p *PCM) State() (PcmState, error) {
	// This function returns the internally tracked state, which is more reliable
	// than querying the kernel, as some ioctls can be unsupported.
	// For kernel-level state, see KernelState().
	if p == nil {
		return PCM_STATE_DISCONNECTED, errors.New("pcm is nil")
	}

	return p.getState(), nil
}

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

	// After updating the application pointer, we must notify the kernel via SYNC_PTR.
	// This tells the kernel to re-read the appl_ptr from the control structure and update its internal state.
	if err := p.ioctlSync(SNDRV_PCM_SYNC_PTR_APPL); err != nil {
		// The ioctl failed, which often indicates an XRUN. Update our state and propagate the error.
		if errors.Is(err, syscall.EPIPE) {
			p.setState(PCM_STATE_XRUN)
		}

		return err
	}

	return nil
}

// Timestamp returns available frames and the corresponding timestamp.
// The clock source is CLOCK_MONOTONIC if the PCM_MONOTONIC flag was used, otherwise it is CLOCK_REALTIME.
// This function is only available for MMAP streams.
func (p *PCM) Timestamp() (availFrames uint32, t time.Time, err error) {
	if (p.flags & PCM_MMAP) == 0 {
		err = fmt.Errorf("Timestamp() is only available for MMAP streams")

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
// This function is only available for MMAP streams.
func (p *PCM) HWPtr() (hwPtr SndPcmUframesT, tstamp time.Time, err error) {
	if !p.IsReady() {
		err = fmt.Errorf("PCM is not ready")

		return
	}

	if (p.flags & PCM_MMAP) == 0 {
		err = fmt.Errorf("HWPtr() is only available for MMAP streams")

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

// xrunRecover is an internal helper to recover from an XRUN.
func (p *PCM) xrunRecover(err error) error {
	// Check for EPIPE (XRUN) or EBADFD (Bad state, e.g., stream stopped unexpectedly).
	isEPIPE := errors.Is(err, syscall.EPIPE)
	isEBADFD := errors.Is(err, unix.EBADFD)

	if !isEPIPE && !isEBADFD {
		return err // Not an XRUN or recoverable bad state
	}

	// Only an EPIPE is a true XRUN. Increment the counter and set the state accordingly.
	if isEPIPE {
		p.xruns++
		p.setState(PCM_STATE_XRUN)
	}

	if (p.flags & PCM_NORESTART) != 0 {
		return fmt.Errorf("xrun or bad state occurred with PCM_NORESTART: %w", err)
	}

	// According to ALSA documentation, after an XRUN or if the stream is stopped,
	// a call to prepare() is sufficient to recover to the PREPARED state.
	if prepErr := p.Prepare(); prepErr != nil {
		return fmt.Errorf("recovery failed: could not prepare stream: %w", prepErr)
	}

	// For capture streams (both MMAP and standard), we must explicitly restart the stream to resume data flow.
	if (p.flags & PCM_IN) != 0 {
		return p.Start()
	}

	return nil
}

// setupStatusAndControl maps the kernel's status and control structures into memory.
// It always attempts this, as some drivers support it even for non-MMAP streams.
// If mapping fails, it sets up a fallback structure to be used with the SYNC_PTR ioctl.
func (p *PCM) setupStatusAndControl() error {
	pageSize := os.Getpagesize()
	var statusBuf, controlBuf []byte
	var err error

	// Always allocate the sync_ptr struct, which is needed for the SYNC_PTR ioctl.
	p.syncPtr = &sndPcmSyncPtr{}

	// Attempt to mmap status (read-only) and control (read-write) pages.
	statusBuf, err = unix.Mmap(int(p.file.Fd()), SNDRV_PCM_MMAP_OFFSET_STATUS, pageSize, unix.PROT_READ, unix.MAP_SHARED)
	if err == nil {
		controlBuf, err = unix.Mmap(int(p.file.Fd()), SNDRV_PCM_MMAP_OFFSET_CONTROL, pageSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			_ = unix.Munmap(statusBuf) // Clean up statusBuf if controlBuf fails
		}
	}

	if err != nil {
		// Fallback mechanism: Use local structs and SYNC_PTR ioctl.
		// The mmapStatus and mmapControl pointers will point to the fields inside our local syncPtr.
		p.mmapStatus = &p.syncPtr.S.sndPcmMmapStatus
		p.mmapControl = &p.syncPtr.C.sndPcmMmapControl
		p.syncPtrIsMmapped = false
	} else {
		// Success: Use the mmapped memory directly.
		p.mmapStatus = (*sndPcmMmapStatus)(unsafe.Pointer(&statusBuf[0]))
		p.mmapControl = (*sndPcmMmapControl)(unsafe.Pointer(&controlBuf[0]))
		p.syncPtrIsMmapped = true
	}

	// Initialize AvailMin in the control structure (shared or fallback).
	// Use atomic store for consistency and safety, ensuring initialization is visible (release barrier).
	var availMin SndPcmUframesT = SndPcmUframesT(p.config.AvailMin)
	if unsafe.Sizeof(availMin) == 8 {
		atomic.StoreUint64((*uint64)(unsafe.Pointer(&p.mmapControl.AvailMin)), uint64(availMin))
	} else {
		atomic.StoreUint32((*uint32)(unsafe.Pointer(&p.mmapControl.AvailMin)), uint32(availMin))
	}

	return nil
}

func (p *PCM) unmapStatusAndControl() {
	if p.mmapStatus == nil {
		return
	}

	if p.syncPtrIsMmapped {
		pageSize := os.Getpagesize()
		if p.mmapStatus != nil {
			buf := unsafe.Slice((*byte)(unsafe.Pointer(p.mmapStatus)), pageSize)
			_ = unix.Munmap(buf)
		}

		if p.mmapControl != nil {
			buf := unsafe.Slice((*byte)(unsafe.Pointer(p.mmapControl)), pageSize)
			_ = unix.Munmap(buf)
		}
	}

	p.mmapStatus = nil
	p.mmapControl = nil
	p.syncPtr = nil
}

// ioctlSync synchronizes the application and hardware pointers.
// This is used for all stream types to query state and manage pointers.
func (p *PCM) ioctlSync(flags uint32) error {
	if p.noSyncPtr {
		return nil
	}

	if p.syncPtr == nil {
		// This should not happen if setupStatusAndControl succeeded.
		return fmt.Errorf("internal error: sync_ptr not initialized")
	}

	handleIoctlError := func(err error) error {
		if errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.EINVAL) {
			// Handle cases where SYNC_PTR is not supported.
			p.noSyncPtr = true
			return nil
		}

		// The caller will handle state updates and XRUN counting based on the error.
		return err
	}

	// When using MMAP, the kernel reads appl_ptr from the user-provided struct.
	// We must ensure our syncPtr struct contains the latest appl_ptr from the mmap'd control page.
	if p.syncPtrIsMmapped {
		// Atomically read the application pointer from the shared memory control page.
		var applPtr SndPcmUframesT
		if unsafe.Sizeof(applPtr) == 8 {
			applPtr = SndPcmUframesT(atomic.LoadUint64((*uint64)(unsafe.Pointer(&p.mmapControl.ApplPtr))))
		} else {
			applPtr = SndPcmUframesT(atomic.LoadUint32((*uint32)(unsafe.Pointer(&p.mmapControl.ApplPtr))))
		}
		// Update the control part of our sync_ptr struct for the ioctl.
		p.syncPtr.C.sndPcmMmapControl.ApplPtr = applPtr
	}

	p.syncPtr.Flags = flags
	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_SYNC_PTR, uintptr(unsafe.Pointer(p.syncPtr))); err != nil {
		return handleIoctlError(err)
	}

	// After the ioctl, the kernel has updated the status fields in our syncPtr struct.
	// If not in mmap mode, our mmapStatus pointer already points to the struct's status field,
	// so it's automatically updated. If in mmap mode, the kernel updates the mmap'd status
	// page directly, so reads from p.mmapStatus will see the new values.

	return nil
}

func (p *PCM) getState() PcmState {
	return PcmState(atomic.LoadInt32(&p.state))
}

func (p *PCM) setState(s PcmState) {
	atomic.StoreInt32(&p.state, int32(s))
}

// checkState queries the PCM status and returns an error if the stream is in an error state.
// Returns true if the stream is ready for I/O, false otherwise (with an accompanying error if applicable).
func (p *PCM) checkState() (bool, error) {
	// First, check the state from the mmap'd page, which is non-blocking.
	// This handles the common XRUN case without a potentially blocking syscall.
	s := p.KernelState()
	p.setState(s) // Update cached state

	// If the non-blocking check reveals a clear error state, handle it.
	switch s {
	case PCM_STATE_XRUN:
		// Do not increment xruns here. The caller will receive EPIPE and call xrunRecover,
		// which is the canonical place to increment the counter.
		return false, fmt.Errorf("stream xrun: %w", syscall.EPIPE)
	case PCM_STATE_SUSPENDED:
		return false, fmt.Errorf("stream suspended: %w", syscall.ESTRPIPE)
	case PCM_STATE_DISCONNECTED:
		return false, fmt.Errorf("device disconnected: %w", syscall.ENODEV)
	}

	// If the mmap'd state seems okay, but poll reported an error (POLLERR/POLLHUP/POLLNVAL), we must
	// use the ioctl to get the definitive status from the kernel. This is the
	// only way to be 100% sure, and is only done on an error path.
	var status sndPcmStatus
	if ioctlErr := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_STATUS, uintptr(unsafe.Pointer(&status))); ioctlErr != nil {
		// If we can't get status, it's a critical failure.
		return false, fmt.Errorf("ioctl STATUS failed: %w", ioctlErr)
	}

	p.setState(status.State)

	switch status.State {
	case PCM_STATE_XRUN:
		// Return EPIPE for XRUN, and false for readiness.
		// The caller will call xrunRecover, which increments the counter.
		return false, fmt.Errorf("stream xrun: %w", syscall.EPIPE)
	case PCM_STATE_SUSPENDED:
		return false, fmt.Errorf("stream suspended: %w", syscall.ESTRPIPE)
	case PCM_STATE_DISCONNECTED:
		return false, fmt.Errorf("device disconnected: %w", syscall.ENODEV)
	case PCM_STATE_OPEN, PCM_STATE_SETUP:
		// These states are not ready for I/O. Return EBADFD to signal a bad state
		// that the caller should recover from, rather than treating it as a timeout.
		return false, unix.EBADFD
	case PCM_STATE_PREPARED:
		// For playback, PREPARED means ready to accept data.
		if (p.flags & PCM_IN) == 0 {
			return true, nil
		}

		// For capture, PREPARED means healthy, but not running yet (e.g., waiting for linked start).
		// We should not treat this as EBADFD, as that triggers recovery which might break linked starts.
		// We return false (not ready) and nil error, effectively treating it like a timeout
		// or spurious wakeup in the Wait() caller.
		return false, nil
	default:
		// All other states (RUNNING, DRAINING, PAUSED) are considered ready for I/O by Wait().
		return true, nil
	}
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

// checkSliceOfSlices validates that the input is a slice of slices of a supported numeric type.
// It checks channel count, type consistency, and buffer length for all inner slices.
// It returns a slice of pointers to the data of each inner slice.
func checkSliceOfSlices(p *PCM, data any, frames uint32) ([]uintptr, error) {
	if data == nil {
		return nil, errors.New("data cannot be nil")
	}

	rv := reflect.ValueOf(data)
	if rv.Kind() != reflect.Slice {
		return nil, fmt.Errorf("expected a slice of slices, got %T", data)
	}

	if uint32(rv.Len()) != p.config.Channels {
		return nil, fmt.Errorf("incorrect number of channels in data: expected %d, got %d", p.config.Channels, rv.Len())
	}

	if rv.Len() == 0 {
		return nil, nil // No channels, nothing to do.
	}

	// Determine an expected element type and size from the first channel
	firstInnerSlice := rv.Index(0)
	if firstInnerSlice.Kind() != reflect.Slice {
		return nil, fmt.Errorf("expected a slice of slices, but first element is %T", firstInnerSlice.Interface())
	}
	elemType := firstInnerSlice.Type().Elem()

	// Validate element type is a supported numeric type
	switch elemType.Kind() {
	case reflect.Int8, reflect.Uint8,
		reflect.Int16, reflect.Uint16,
		reflect.Int32, reflect.Uint32,
		reflect.Float32, reflect.Float64:
	default:
		return nil, fmt.Errorf("unsupported slice element type: %s", elemType.Kind())
	}

	// The number of bytes per frame for a single channel's data
	bytesPerFramePerChannel := p.FrameSize() / p.config.Channels
	requiredBytes := frames * bytesPerFramePerChannel
	pointers := make([]uintptr, p.config.Channels)

	for i := 0; i < rv.Len(); i++ {
		innerSlice := rv.Index(i)

		// Check that all inner elements are slices of the same type
		if innerSlice.Kind() != reflect.Slice || innerSlice.Type().Elem() != elemType {
			return nil, fmt.Errorf("channel %d is not a slice of the expected type", i)
		}

		// Check if the buffer for this channel is large enough
		byteLen := uint32(innerSlice.Len()) * uint32(elemType.Size())
		if byteLen < requiredBytes {
			return nil, fmt.Errorf("channel %d buffer too small: needs %d bytes, got %d", i, requiredBytes, byteLen)
		}

		// Get the pointer to the slice data
		if innerSlice.Len() > 0 {
			pointers[i] = innerSlice.Index(0).Addr().Pointer()
		}
	}

	return pointers, nil
}

// PcmFormatToBits returns the number of bits per sample for a given format.
// This reflects the space occupied in memory, so 24-bit formats in 32-bit containers return 32.
func PcmFormatToBits(f PcmFormat) uint32 {
	switch f {
	case PCM_FORMAT_FLOAT64_LE, PCM_FORMAT_FLOAT64_BE:
		return 64
	case PCM_FORMAT_S32_LE, PCM_FORMAT_S32_BE, PCM_FORMAT_U32_LE, PCM_FORMAT_U32_BE,
		PCM_FORMAT_FLOAT_LE, PCM_FORMAT_FLOAT_BE,
		PCM_FORMAT_S24_LE, PCM_FORMAT_S24_BE, PCM_FORMAT_U24_LE, PCM_FORMAT_U24_BE:
		return 32
	case PCM_FORMAT_S24_3LE, PCM_FORMAT_S24_3BE, PCM_FORMAT_U24_3LE, PCM_FORMAT_U24_3BE:
		return 24
	case PCM_FORMAT_S16_LE, PCM_FORMAT_S16_BE, PCM_FORMAT_U16_LE, PCM_FORMAT_U16_BE:
		return 16
	case PCM_FORMAT_S8, PCM_FORMAT_U8:
		return 8
	default:
		return 0
	}
}

// PcmFramesToBytes converts a number of frames to the corresponding number of bytes.
func PcmFramesToBytes(p *PCM, frames uint32) uint32 {
	if p == nil {
		return 0
	}

	frameSize := p.FrameSize()
	if frameSize == 0 {
		return 0
	}

	return frames * frameSize
}

// PcmBytesToFrames converts a number of bytes to the corresponding number of frames.
func PcmBytesToFrames(p *PCM, bytes uint32) uint32 {
	if p == nil {
		return 0
	}

	frameSize := p.FrameSize()
	if frameSize == 0 {
		return 0
	}

	return bytes / frameSize
}

// PcmParamsGet queries the hardware parameters for a given PCM device to get its default settings.
// This function initializes the parameters and then uses the SNDRV_PCM_IOCTL_HW_PARAMS ioctl.
// The kernel then fills the structure with the hardware's default or current settings.
func PcmParamsGet(card, device uint, flags PcmFlag) (*PcmParams, error) {
	var streamChar byte
	if (flags & PCM_IN) != 0 {
		streamChar = 'c'
	} else {
		streamChar = 'p'
	}

	path := fmt.Sprintf("/dev/snd/pcmC%dD%d%c", card, device, streamChar)

	// Use O_NONBLOCK on open to avoid getting stuck
	file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open PCM device %s for query: %w", path, err)
	}
	defer file.Close()

	hwParams := &sndPcmHwParams{}
	paramInit(hwParams)

	// HW_PARAMS will fill the struct with default parameters.
	if err := ioctl(file.Fd(), SNDRV_PCM_IOCTL_HW_PARAMS, uintptr(unsafe.Pointer(hwParams))); err != nil {
		return nil, fmt.Errorf("ioctl HW_PARAMS failed: %w", err)
	}

	return &PcmParams{params: hwParams}, nil
}

// PcmParamsGetRefined queries the hardware parameters for a given PCM device to discover its full range of capabilities.
// This function initializes the parameters and then uses the SNDRV_PCM_IOCTL_HW_REFINE ioctl to ask the kernel to restrict
// the ranges to what the hardware actually supports.
func PcmParamsGetRefined(card, device uint, flags PcmFlag) (*PcmParams, error) {
	var streamChar byte
	if (flags & PCM_IN) != 0 {
		streamChar = 'c'
	} else {
		streamChar = 'p'
	}

	path := fmt.Sprintf("/dev/snd/pcmC%dD%d%c", card, device, streamChar)

	// Use O_NONBLOCK on open to avoid getting stuck
	file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open PCM device %s for query: %w", path, err)
	}
	defer file.Close()

	hwParams := &sndPcmHwParams{}
	paramInit(hwParams)

	if err := ioctl(file.Fd(), SNDRV_PCM_IOCTL_HW_REFINE, uintptr(unsafe.Pointer(hwParams))); err != nil {
		return nil, fmt.Errorf("ioctl HW_REFINE failed: %w", err)
	}

	return &PcmParams{params: hwParams}, nil
}

// RangeMin returns the minimum value for an interval parameter.
func (pp *PcmParams) RangeMin(param PcmParam) (uint32, error) {
	if pp == nil || pp.params == nil {
		return 0, fmt.Errorf("params not initialized")
	}

	if param < PCM_PARAM_SAMPLE_BITS || param > PCM_PARAM_TICK_TIME {
		return 0, fmt.Errorf("parameter %v is not an interval type", param)
	}

	return pp.params.Intervals[param-PCM_PARAM_SAMPLE_BITS].MinVal, nil
}

// RangeMax returns the maximum value for an interval parameter.
func (pp *PcmParams) RangeMax(param PcmParam) (uint32, error) {
	if pp == nil || pp.params == nil {
		return 0, fmt.Errorf("params not initialized")
	}

	if param < PCM_PARAM_SAMPLE_BITS || param > PCM_PARAM_TICK_TIME {
		return 0, fmt.Errorf("parameter %v is not an interval type", param)
	}

	return pp.params.Intervals[param-PCM_PARAM_SAMPLE_BITS].MaxVal, nil
}

// Mask returns the bitmask for a mask-type parameter.
func (pp *PcmParams) Mask(param PcmParam) (*PcmParamMask, error) {
	if pp == nil || pp.params == nil {
		return nil, fmt.Errorf("params not initialized")
	}

	if param < PCM_PARAM_ACCESS || param > PCM_PARAM_SUBFORMAT {
		return nil, fmt.Errorf("parameter %v is not a mask type", param)
	}

	maskPtr := &pp.params.Masks[param-PCM_PARAM_ACCESS]

	return (*PcmParamMask)(unsafe.Pointer(maskPtr)), nil
}

// FormatIsSupported checks if a given PCM format is supported.
func (pp *PcmParams) FormatIsSupported(format PcmFormat) bool {
	mask, err := pp.Mask(PCM_PARAM_FORMAT)
	if err != nil {
		return false
	}

	return mask.Test(uint(format))
}

// String returns a human-readable representation of the PCM device's capabilities.
func (pp *PcmParams) String() string {
	if pp == nil || pp.params == nil {
		return "<nil>"
	}

	var b strings.Builder

	// Helper to print masks using a string slice for names
	printMaskSlice := func(name string, param PcmParam, names []string) {
		mask, err := pp.Mask(param)
		if err != nil {
			return
		}

		var supported []string
		for i, n := range names {
			if i < len(names) && len(n) > 0 && mask.Test(uint(i)) {
				supported = append(supported, n)
			}
		}

		if len(supported) > 0 {
			b.WriteString(fmt.Sprintf("%12s: %s\n", name, strings.Join(supported, ", ")))
		}
	}

	// Helper to print format masks using the map
	printFormatMask := func() {
		mask, err := pp.Mask(PCM_PARAM_FORMAT)
		if err != nil {
			return
		}

		var supported []string

		// Sort keys for consistent output
		var keys []int
		for k := range PcmParamFormatNames {
			keys = append(keys, int(k))
		}

		sort.Ints(keys)

		for _, k := range keys {
			f := PcmFormat(k)
			if name, ok := PcmParamFormatNames[f]; ok && mask.Test(uint(f)) {
				supported = append(supported, name)
			}
		}

		if len(supported) > 0 {
			b.WriteString(fmt.Sprintf("%12s: %s\n", "Format", strings.Join(supported, ", ")))
		}
	}

	// Helper to print interval parameters
	printInterval := func(name string, param PcmParam, unit string) {
		rangeMin, errMin := pp.RangeMin(param)
		rangeMax, errMax := pp.RangeMax(param)

		if errMin != nil || errMax != nil {
			return
		}

		if rangeMax == 0 || rangeMax == ^uint32(0) { // Don't print meaningless ranges
			return
		}

		b.WriteString(fmt.Sprintf("%12s: min=%-6d max=%-6d %s\n", name, rangeMin, rangeMax, unit))
	}

	b.WriteString("PCM device capabilities:\n")
	printMaskSlice("Access", PCM_PARAM_ACCESS, PcmParamAccessNames)
	printFormatMask()
	printMaskSlice("Subformat", PCM_PARAM_SUBFORMAT, PcmParamSubformatNames)
	printInterval("Rate", PCM_PARAM_RATE, "Hz")
	printInterval("Channels", PCM_PARAM_CHANNELS, "")
	printInterval("Sample bits", PCM_PARAM_SAMPLE_BITS, "")
	printInterval("Period size", PCM_PARAM_PERIOD_SIZE, "frames")
	printInterval("Periods", PCM_PARAM_PERIODS, "")

	return b.String()
}

// paramInit initializes a sndPcmHwParams struct to allow all possible values.
func paramInit(p *sndPcmHwParams) {
	// Initialize all masks (including reserved) to all-ones.
	for n := range p.Masks {
		for i := range p.Masks[n].Bits {
			p.Masks[n].Bits[i] = ^uint32(0)
		}
	}

	for n := range p.Mres {
		for i := range p.Mres[n].Bits {
			p.Mres[n].Bits[i] = ^uint32(0)
		}
	}

	// Initialize all intervals (including reserved) to the full range.
	for n := range p.Intervals {
		p.Intervals[n].MinVal = 0
		p.Intervals[n].MaxVal = ^uint32(0)
		p.Intervals[n].Flags = 0
	}

	for n := range p.Ires {
		p.Ires[n].MinVal = 0
		p.Ires[n].MaxVal = ^uint32(0)
		p.Ires[n].Flags = 0
	}

	p.Rmask = ^uint32(0)
	p.Info = ^uint32(0)
}

func paramSetMask(p *sndPcmHwParams, param PcmParam, bit uint32) {
	// The first 3 params are masks
	if param < PCM_PARAM_ACCESS || param > PCM_PARAM_SUBFORMAT {
		return
	}

	mask := &p.Masks[param-PCM_PARAM_ACCESS]
	for i := range mask.Bits {
		mask.Bits[i] = 0
	}

	if bit >= 256 { // SNDRV_MASK_MAX
		return
	}

	mask.Bits[bit>>5] |= 1 << (bit & 31)
}

func paramSetInt(p *sndPcmHwParams, param PcmParam, val uint32) {
	if param < PCM_PARAM_SAMPLE_BITS || param > PCM_PARAM_TICK_TIME {
		return
	}

	// The interval array index is the parameter value minus the value of the first interval param.
	interval := &p.Intervals[param-PCM_PARAM_SAMPLE_BITS]
	interval.MinVal = val
	interval.MaxVal = val
	interval.Flags = SNDRV_PCM_INTERVAL_INTEGER
}

func paramSetMin(p *sndPcmHwParams, param PcmParam, val uint32) {
	if param < PCM_PARAM_SAMPLE_BITS || param > PCM_PARAM_TICK_TIME {
		return
	}

	interval := &p.Intervals[param-PCM_PARAM_SAMPLE_BITS]
	interval.MinVal = val
}

func paramGetInt(p *sndPcmHwParams, param PcmParam) uint32 {
	if param < PCM_PARAM_SAMPLE_BITS || param > PCM_PARAM_TICK_TIME {
		return 0
	}

	// The interval array index is the parameter value minus the value of the first interval param.
	interval := &p.Intervals[param-PCM_PARAM_SAMPLE_BITS]

	// Read the MinVal of the interval.
	// The driver finalizes the configuration by narrowing the interval.
	return interval.MinVal
}
