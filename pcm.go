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
	xruns              int      // Counter for overruns/underruns
	noirqFramesPerMsec uint32   // For PCM_NOIRQ calculation
	state              PcmState // Tracks the stream state, replacing the 'running' boolean.
	lastError          string   // For pcm_get_error() equivalent
	noSyncPtr          bool     // Flag to indicate that SYNC_PTR is not supported.
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

	// Match the C implementation: always open non-blocking to avoid getting stuck
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
		state:     PCM_STATE_OPEN,
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

	// This is critical: status/control structures (or the sync_ptr fallback)
	// must be initialized for ALL streams to allow state checking.
	if err := pcm.setupStatusAndControl(); err != nil {
		pcm.Close()

		return nil, fmt.Errorf("failed to set up status and control: %w", err)
	}

	// Set timestamp type if requested, matching tinyalsa's pcm_open logic.
	if (flags & PCM_MONOTONIC) != 0 {
		// SNDRV_PCM_TSTAMP_TYPE_MONOTONIC = 1
		var arg int32 = 1
		if err := ioctl(pcm.file.Fd(), SNDRV_PCM_IOCTL_TTSTAMP, uintptr(unsafe.Pointer(&arg))); err != nil {
			pcm.Close()
			return nil, pcm.setError(err, "ioctl TTSTAMP failed")
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
		unix.Munmap(p.mmapBuffer)
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

// Error returns the last error message as a string. This is useful for debugging
// setup or I/O failures. It is the Go equivalent of pcm_get_error().
func (p *PCM) Error() string {
	return p.lastError
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
// This function should be called before the stream is started. If called on an
// already configured stream, it will attempt to reconfigure it, which may only succeed if the stream is stopped.
func (p *PCM) SetConfig(config *Config) error {
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
		paramSetMask(hwParams, PCM_PARAM_ACCESS, 0) // SNDRV_PCM_ACCESS_MMAP_INTERLEAVED
	} else {
		paramSetMask(hwParams, PCM_PARAM_ACCESS, 3) // SNDRV_PCM_ACCESS_RW_INTERLEAVED
	}

	paramSetMask(hwParams, PCM_PARAM_FORMAT, uint32(config.Format))
	paramSetMask(hwParams, PCM_PARAM_SUBFORMAT, 0) // SNDRV_PCM_SUBFORMAT_STD
	paramSetMin(hwParams, PCM_PARAM_SAMPLE_BITS, pcmFormatToRealBits(config.Format))
	paramSetInt(hwParams, PCM_PARAM_CHANNELS, config.Channels)
	paramSetInt(hwParams, PCM_PARAM_RATE, config.Rate)
	paramSetInt(hwParams, PCM_PARAM_PERIODS, config.PeriodCount)
	paramSetMin(hwParams, PCM_PARAM_PERIOD_SIZE, config.PeriodSize)

	if (p.flags & PCM_NOIRQ) != 0 {
		if (p.flags & PCM_MMAP) == 0 {
			return p.setError(nil, "PCM_NOIRQ is only supported with PCM_MMAP")
		}

		// SNDRV_PCM_HW_PARAMS_NO_PERIOD_WAKEUP = (1<<2)
		hwParams.Flags |= 1 << 2
		if config.Rate > 0 {
			p.noirqFramesPerMsec = config.Rate / 1000
		}
	}

	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_HW_PARAMS, uintptr(unsafe.Pointer(hwParams))); err != nil {
		return p.setError(err, "ioctl HW_PARAMS failed")
	}

	// Update our config with the refined parameters from the driver.
	p.config.PeriodSize = paramGetInt(hwParams, PCM_PARAM_PERIOD_SIZE)
	p.config.PeriodCount = paramGetInt(hwParams, PCM_PARAM_PERIODS)
	p.bufferSize = p.config.PeriodSize * p.config.PeriodCount

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
			// For playback, the C default is one period.
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
	swParams.SilenceThreshold = SndPcmUframesT(config.SilenceThreshold)
	swParams.SilenceSize = SndPcmUframesT(config.SilenceSize)

	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_SW_PARAMS, uintptr(unsafe.Pointer(swParams))); err != nil {
		return p.setError(err, "ioctl SW_PARAMS (write) failed")
	}

	p.boundary = swParams.Boundary

	// 3. MMAP Buffer (if applicable)
	// Mmap the data buffer after setting HW/SW_PARAMS, matching the standard ALSA flow (and C tinyalsa).
	if (p.flags & PCM_MMAP) != 0 {
		frameSize := PcmFormatToBits(p.config.Format) / 8 * p.config.Channels
		mmapLen := int(p.bufferSize * uint32(frameSize))

		buf, err := unix.Mmap(int(p.file.Fd()), 0, mmapLen, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return p.setError(err, "mmap data buffer failed")
		}
		p.mmapBuffer = buf
	}

	// After HW_PARAMS and SW_PARAMS, the stream is in the SETUP state.
	// It must be explicitly prepared before starting.
	p.state = PCM_STATE_SETUP

	return nil
}

// xrunRecover is an internal helper to recover from an XRUN.
func (p *PCM) xrunRecover(err error) error {
	if !errors.Is(err, syscall.EPIPE) {
		return err // Not an XRUN
	}

	p.xruns++
	if (p.flags & PCM_NORESTART) != 0 {
		p.state = PCM_STATE_XRUN
		return p.setError(err, "xrun occurred with PCM_NORESTART")
	}

	// To recover, we must stop the stream, then re-prepare it.
	// Stop() calls DROP and sets state to SETUP.
	if stopErr := p.Stop(); stopErr != nil {
		p.state = PCM_STATE_XRUN // Can't recover
		return p.setError(stopErr, "xrun recovery failed: could not stop stream")
	}

	// After Stop(), state is SETUP. Now re-prepare.
	// Prepare() sets state to PREPARED.
	if prepErr := p.Prepare(); prepErr != nil {
		p.state = PCM_STATE_XRUN // Can't recover
		return p.setError(prepErr, "xrun recovery failed: could not prepare stream")
	}

	// For capture streams, tinyalsa also restarts them immediately.
	// For playback, the next write will start it.
	if (p.flags & PCM_IN) != 0 {
		return p.startUnconditional()
	}

	return nil
}

// WriteI writes interleaved audio data to a playback PCM device using an ioctl call.
// data must be a slice of a supported numeric type (e.g., []int16, []float32).
// This is the idiomatic way to perform blocking I/O with ALSA.
// It automatically prepares and starts the stream and recovers from underruns (EPIPE) by retrying once.
// The data buffer should contain data for the number of 'frames' specified.
// Returns the number of frames actually written.
func (p *PCM) WriteI(data any, frames uint32) (int, error) {
	if (p.flags & PCM_IN) != 0 {
		return 0, p.setError(nil, "cannot write to a capture device")
	}
	if (p.flags & PCM_MMAP) != 0 {
		return 0, p.setError(nil, "use MmapWrite for mmap devices")
	}

	byteLen, err := checkSlice(data)
	if err != nil {
		return 0, p.setError(err, "invalid data type for WriteI")
	}
	requiredBytes := PcmFramesToBytes(p, frames)
	if byteLen < requiredBytes {
		return 0, p.setError(nil, "data buffer too small: needs %d bytes, got %d", requiredBytes, byteLen)
	}

	if frames == 0 || requiredBytes == 0 {
		return 0, nil
	}

	xfer := sndXferi{Frames: SndPcmUframesT(frames)}
	if reflect.ValueOf(data).Len() > 0 {
		xfer.Buf = reflect.ValueOf(data).Index(0).Addr().Pointer()
	}
	defer runtime.KeepAlive(data)

	if p.state != PCM_STATE_RUNNING && p.state != PCM_STATE_PREPARED {
		if err := p.Prepare(); err != nil {
			return 0, err
		}
	}

	err = ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_WRITEI_FRAMES, uintptr(unsafe.Pointer(&xfer)))

	if err != nil && (p.flags&PCM_NORESTART) == 0 && errors.Is(err, syscall.EPIPE) {
		if errRec := p.xrunRecover(err); errRec != nil {
			return 0, errRec
		}
		err = ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_WRITEI_FRAMES, uintptr(unsafe.Pointer(&xfer)))
	}

	if err != nil {
		p.state = PCM_STATE_XRUN
		return 0, p.setError(err, "ioctl WRITEI_FRAMES failed")
	}

	if p.state != PCM_STATE_RUNNING {
		p.state = PCM_STATE_RUNNING
	}

	return int(xfer.Result), nil
}

// WriteN writes non-interleaved audio data to a playback PCM device.
// This function is for non-MMAP, non-interleaved access. It is less common than
// interleaved I/O. The `data` slice should contain one slice per channel (e.g., `[][]byte`),
// each containing 'frames' worth of sample data. It uses the `SNDRV_PCM_IOCTL_WRITEN_FRAMES` ioctl.
// Like WriteI, it handles underruns by preparing and retrying the write.
// Returns the number of frames actually written.
func (p *PCM) WriteN(data [][]byte, frames uint32) (int, error) {
	if (p.flags & PCM_IN) != 0 {
		return 0, p.setError(nil, "cannot write to a capture device")
	}
	if (p.flags & PCM_MMAP) != 0 {
		return 0, p.setError(nil, "WriteN is not for mmap devices")
	}
	if uint32(len(data)) != p.config.Channels {
		return 0, p.setError(nil, "incorrect number of channels in data: expected %d, got %d", p.config.Channels, len(data))
	}

	bytesPerFramePerChannel := PcmFormatToBits(p.config.Format) / 8
	requiredBytes := frames * bytesPerFramePerChannel
	for i, channelData := range data {
		if uint32(len(channelData)) < requiredBytes {
			return 0, p.setError(nil, "channel %d buffer too small: needs %d, got %d", i, requiredBytes, len(channelData))
		}
	}

	pointers := make([]unsafe.Pointer, p.config.Channels)
	for i, channelData := range data {
		if len(channelData) > 0 {
			pointers[i] = unsafe.Pointer(&channelData[0])
		}
	}
	defer runtime.KeepAlive(data)

	xfer := sndXfern{Frames: SndPcmUframesT(frames)}
	if len(pointers) > 0 {
		xfer.Bufs = uintptr(unsafe.Pointer(&pointers[0]))
	}
	defer runtime.KeepAlive(pointers)

	if p.state != PCM_STATE_RUNNING && p.state != PCM_STATE_PREPARED {
		if err := p.Prepare(); err != nil {
			return 0, err
		}
	}

	err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_WRITEN_FRAMES, uintptr(unsafe.Pointer(&xfer)))
	if err != nil && (p.flags&PCM_NORESTART) == 0 && errors.Is(err, syscall.EPIPE) {
		if errRec := p.xrunRecover(err); errRec != nil {
			return 0, errRec
		}
		err = ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_WRITEN_FRAMES, uintptr(unsafe.Pointer(&xfer)))
	}

	if err != nil {
		p.state = PCM_STATE_XRUN
		return 0, p.setError(err, "ioctl WRITEN_FRAMES failed")
	}

	if p.state != PCM_STATE_RUNNING {
		p.state = PCM_STATE_RUNNING
	}

	return int(xfer.Result), nil
}

// ReadI reads interleaved audio data from a capture PCM device using an ioctl call.
// buffer must be a slice of a supported numeric type (e.g., []int16, []float32).
// This is the idiomatic way to perform blocking I/O with ALSA.
// It automatically starts the stream and recovers from overruns (EPIPE) by retrying once.
// The provided buffer must be large enough to hold 'frames' of audio data.
// Returns the number of frames actually read.
func (p *PCM) ReadI(buffer any, frames uint32) (int, error) {
	if (p.flags & PCM_IN) == 0 {
		return 0, p.setError(nil, "cannot read from a playback device")
	}
	if (p.flags & PCM_MMAP) != 0 {
		return 0, p.setError(nil, "use MmapRead for mmap devices")
	}

	byteLen, err := checkSlice(buffer)
	if err != nil {
		return 0, p.setError(err, "invalid buffer type for ReadI")
	}
	requiredBytes := PcmFramesToBytes(p, frames)
	if byteLen < requiredBytes {
		return 0, p.setError(nil, "buffer too small: needs %d bytes, got %d", requiredBytes, byteLen)
	}

	if frames == 0 || requiredBytes == 0 {
		return 0, nil
	}

	xfer := sndXferi{Frames: SndPcmUframesT(frames)}
	if reflect.ValueOf(buffer).Len() > 0 {
		xfer.Buf = reflect.ValueOf(buffer).Index(0).Addr().Pointer()
	}
	defer runtime.KeepAlive(buffer)

	if p.state != PCM_STATE_RUNNING {
		if err := p.Start(); err != nil {
			return 0, err
		}
	}

	err = ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_READI_FRAMES, uintptr(unsafe.Pointer(&xfer)))

	if err != nil && (p.flags&PCM_NORESTART) == 0 && errors.Is(err, syscall.EPIPE) {
		if errRec := p.xrunRecover(err); errRec != nil {
			return 0, errRec
		}
		err = ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_READI_FRAMES, uintptr(unsafe.Pointer(&xfer)))
	}

	if err != nil {
		p.state = PCM_STATE_XRUN
		return 0, p.setError(err, "ioctl READI_FRAMES failed")
	}

	return xfer.Result, nil
}

// ReadN reads non-interleaved audio data from a capture PCM device.
// This function is for non-MMAP, non-interleaved access. The provided `buffers`
// slice must contain one slice per channel (e.g., `[][]byte`), each large enough to
// hold 'frames' of audio data. It uses the `SNDRV_PCM_IOCTL_READN_FRAMES` ioctl.
// Like ReadI, it handles overruns by preparing and retrying the read.
// Returns the number of frames actually read.
func (p *PCM) ReadN(buffers [][]byte, frames uint32) (int, error) {
	if (p.flags & PCM_IN) == 0 {
		return 0, p.setError(nil, "cannot read from a playback device")
	}
	if (p.flags & PCM_MMAP) != 0 {
		return 0, p.setError(nil, "ReadN is not for mmap devices")
	}
	if uint32(len(buffers)) != p.config.Channels {
		return 0, p.setError(nil, "incorrect number of channels in buffers: expected %d, got %d", p.config.Channels, len(buffers))
	}

	bytesPerFramePerChannel := PcmFormatToBits(p.config.Format) / 8
	requiredBytes := frames * bytesPerFramePerChannel
	for i, channelBuffer := range buffers {
		if uint32(len(channelBuffer)) < requiredBytes {
			return 0, p.setError(nil, "channel %d buffer too small: needs %d, got %d", i, requiredBytes, len(channelBuffer))
		}
	}

	pointers := make([]unsafe.Pointer, p.config.Channels)
	for i, channelBuffer := range buffers {
		if len(channelBuffer) == 0 {
			return 0, p.setError(nil, "channel buffer %d is empty or nil", i)
		}
		pointers[i] = unsafe.Pointer(&channelBuffer[0])
	}
	defer runtime.KeepAlive(buffers)

	xfer := sndXfern{Frames: SndPcmUframesT(frames)}
	if len(pointers) > 0 {
		xfer.Bufs = uintptr(unsafe.Pointer(&pointers[0]))
	}
	defer runtime.KeepAlive(pointers)

	if p.state != PCM_STATE_RUNNING {
		if err := p.Start(); err != nil {
			return 0, err
		}
	}

	err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_READN_FRAMES, uintptr(unsafe.Pointer(&xfer)))
	if err != nil && (p.flags&PCM_NORESTART) == 0 && errors.Is(err, syscall.EPIPE) {
		if errRec := p.xrunRecover(err); errRec != nil {
			return 0, errRec
		}
		err = ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_READN_FRAMES, uintptr(unsafe.Pointer(&xfer)))
	}

	if err != nil {
		p.state = PCM_STATE_XRUN
		return 0, p.setError(err, "ioctl READN_FRAMES failed")
	}

	return xfer.Result, nil
}

// Write writes audio samples to a PCM device. This function is for compatibility with tinyalsa's
// deprecated pcm_write. It calculates the number of frames based on the input slice size and calls WriteI.
// data must be a slice of a supported numeric type (e.g., []int16, []float32).
// Returns nil on success or an error on failure.
func (p *PCM) Write(data any) error {
	byteLen, err := checkSlice(data)
	if err != nil {
		return p.setError(err, "invalid data type for Write")
	}
	frames := PcmBytesToFrames(p, byteLen)

	ret, err := p.WriteI(data, frames)
	if err != nil {
		return err
	}
	if uint32(ret) != frames {
		return p.setError(syscall.EIO, "failed to write all frames")
	}

	return nil
}

// Read reads audio samples from a PCM device. This function is for compatibility with tinyalsa's
// deprecated pcm_read. It calculates the number of frames based on the buffer slice size and calls ReadI.
// data must be a slice of a supported numeric type (e.g., []int16, []float32).
// Returns nil on success or an error on failure.
func (p *PCM) Read(data any) error {
	byteLen, err := checkSlice(data)
	if err != nil {
		return p.setError(err, "invalid data type for Read")
	}
	frames := PcmBytesToFrames(p, byteLen)

	ret, err := p.ReadI(data, frames)
	if err != nil {
		return err
	}
	if uint32(ret) != frames {
		return p.setError(syscall.EIO, "failed to read all frames")
	}

	return nil
}

// Prepare readies the PCM device for I/O operations.
// This is typically used to recover from an XRUN.
func (p *PCM) Prepare() error {
	if p.state == PCM_STATE_PREPARED || p.state == PCM_STATE_RUNNING {
		// Calling prepare on an already prepared/running stream is an error in ALSA,
		// but we can treat it as a no-op to be robust, as the stream is already usable.
		return nil
	}

	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_PREPARE, 0); err != nil {
		// If prepare fails, the stream is in an undefined state.
		// Mark it as SETUP so the next operation can try again from a known state.
		p.state = PCM_STATE_SETUP
		return p.setError(err, "ioctl PREPARE failed")
	}

	p.state = PCM_STATE_PREPARED

	return nil
}

// startUnconditional starts the stream without preparing it first.
// This is used after a write to a prepared stream.
func (p *PCM) startUnconditional() error {
	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_START, 0); err != nil {
		if errors.Is(err, syscall.EPIPE) {
			p.state = PCM_STATE_XRUN
			p.xruns++
			return p.setError(err, "ioctl START failed with xrun")
		}

		// If START fails, the stream remains in the PREPARED state.
		// Do not reset to SETUP as the hardware is still configured.
		return p.setError(err, "ioctl START failed")
	}

	p.state = PCM_STATE_RUNNING

	return nil
}

// Start explicitly starts the PCM stream.
// It ensures the stream is prepared before starting.
func (p *PCM) Start() error {
	if p.state == PCM_STATE_RUNNING {
		return nil
	}

	if p.state != PCM_STATE_PREPARED {
		if err := p.Prepare(); err != nil {
			return err
		}
	}

	return p.startUnconditional()
}

// Stop abruptly stops the PCM stream, dropping any pending frames.
func (p *PCM) Stop() error {
	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_DROP, 0); err != nil {
		return p.setError(err, "ioctl DROP failed")
	}

	p.state = PCM_STATE_SETUP

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

	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_PAUSE, uintptr(unsafe.Pointer(&arg))); err != nil {
		return p.setError(err, "ioctl PAUSE failed")
	}

	if enable {
		p.state = PCM_STATE_PAUSED
	} else if p.state == PCM_STATE_PAUSED {
		p.state = PCM_STATE_RUNNING // Resuming should put it back in RUNNING
	}

	return nil
}

// Resume resumes a suspended PCM stream.
func (p *PCM) Resume() error {
	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_RESUME, 0); err != nil {
		// EINTR is a possible outcome and should be retried by the caller if needed.
		return p.setError(err, "ioctl RESUME failed")
	}

	p.state = PCM_STATE_PREPARED // After resume, stream is PREPARED, not running

	return nil
}

// Drain waits for all pending frames in the buffer to be played.
// This is a blocking call and only applies to playback streams.
func (p *PCM) Drain() error {
	if (p.flags & PCM_IN) != 0 {
		return p.setError(nil, "drain cannot be called on a capture device")
	}

	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_DRAIN, 0); err != nil {
		p.state = PCM_STATE_XRUN
		return p.setError(err, "ioctl DRAIN failed")
	}

	p.state = PCM_STATE_SETUP

	return nil
}

// Link establishes a start/stop synchronization between two PCMs.
// After this function is called, the two PCMs will prepare, start and stop in sync.
func (p *PCM) Link(other *PCM) error {
	if !p.IsReady() || !other.IsReady() {
		return fmt.Errorf("both PCM handles must be valid")
	}

	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_LINK, other.file.Fd()); err != nil {
		return p.setError(err, "ioctl LINK failed")
	}

	return nil
}

// Unlink removes the synchronization between this PCM and others.
func (p *PCM) Unlink() error {
	if !p.IsReady() {
		return fmt.Errorf("PCM handle is not valid")
	}

	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_UNLINK, 0); err != nil {
		return p.setError(err, "ioctl UNLINK failed")
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
		return 0, p.setError(err, "ioctl DELAY failed")
	}

	return delay, nil
}

// Wait waits for the PCM to become ready for I/O or until a timeout occurs.
// Returns true if the device is ready, false on timeout.
// On error, it may return a specific error code like EPIPE, ESTRPIPE, ENODEV, or EIO.
func (p *PCM) Wait(timeoutMs int) (bool, error) {
	if !p.IsReady() {
		return false, fmt.Errorf("PCM handle not ready")
	}

	pfd := []unix.PollFd{
		{Fd: int32(p.file.Fd()), Events: unix.POLLIN | unix.POLLOUT | unix.POLLERR | unix.POLLNVAL},
	}

	n, err := unix.Poll(pfd, timeoutMs)
	if err != nil {
		if errors.Is(err, syscall.EINTR) {
			return p.Wait(timeoutMs) // Retry on interrupt
		}

		return false, p.setError(err, "poll failed")
	}

	if n == 0 {
		return false, nil // Timeout
	}

	revents := pfd[0].Revents

	if (revents & (unix.POLLERR | unix.POLLNVAL)) != 0 {
		p.state = p.KernelState()
		if p.state == PCM_STATE_XRUN || p.state == PCM_STATE_SUSPENDED {
			if (p.flags & PCM_NORESTART) != 0 {
				return false, p.setError(syscall.EPIPE, "XRUN/suspend detected with PCM_NORESTART")
			}

			if err := p.xrunRecover(syscall.EPIPE); err != nil {
				return false, err
			}

			// After successful recovery, we are not ready for I/O, so return false.
			return false, nil
		}

		// For other poll errors, tinyalsa returns -EIO. We must return a corresponding error
		// to prevent infinite loops in the caller.
		return false, p.setError(syscall.EIO, "unrecoverable poll error")
	}

	// If Poll() returned a positive value, and it was not an error handled above,
	// it means the file descriptor is ready for I/O in some way (POLLIN, POLLOUT, POLLHUP, etc.).
	// This matches the behavior of the C tinyalsa `pcm_wait`, which would return 1.
	// The subsequent I/O call will then reveal the true nature of the event (e.g., fail with ENODEV on HUP).
	return true, nil
}

// KernelState returns the current state of the PCM stream by querying the kernel.
func (p *PCM) KernelState() PcmState {
	if p.mmapStatus == nil {
		// Cannot query kernel without status struct, return cached state.
		return p.state
	}

	// Use flags=0 just to read the current status from the kernel, matching tinyalsa's pcm_state().
	if err := p.ioctlSync(0); err != nil {
		if errors.Is(err, syscall.EPIPE) {
			// If the sync returns EPIPE, the stream is in XRUN.
			return PCM_STATE_XRUN
		}
		// For other errors (e.g., device unplugged), report as disconnected.
		return PCM_STATE_DISCONNECTED
	}

	// The state is updated in mmapStatus by ioctlSync.
	return PcmState(p.mmapStatus.State)
}

// State returns the current cached state of the PCM stream.
func (p *PCM) State() (PcmState, error) {
	// This function returns the internally tracked state, which is more reliable
	// than querying the kernel, as some ioctls can be unsupported.
	// For kernel-level state, see KernelState().
	if p == nil {
		return PCM_STATE_DISCONNECTED, errors.New("pcm is nil")
	}

	return p.state, nil
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
		// If SYNC_PTR returns EPIPE (xrun), we should update the xrun count and return the error.
		// Use errors.Is for robust checking.
		if errors.Is(err, syscall.EPIPE) {
			p.state = PCM_STATE_XRUN
			p.xruns++
		}

		return 0, err
	}

	p.state = PcmState(p.mmapStatus.State)
	if p.state == PCM_STATE_XRUN {
		p.xruns++
	}

	var avail SndPcmUframesT
	if (p.flags & PCM_IN) != 0 {
		avail = p.mmapStatus.HwPtr - p.mmapControl.ApplPtr
	} else {
		avail = p.mmapStatus.HwPtr + SndPcmUframesT(p.bufferSize) - p.mmapControl.ApplPtr
	}

	if p.boundary > 0 && avail >= p.boundary {
		avail -= p.boundary
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
// data must be a slice of a supported numeric type (e.g., []int16, []float32).
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
		return 0, p.setError(err, "invalid data type for MmapWrite")
	}

	offset := 0
	remainingBytes := int(dataByteLen)

	for remainingBytes > 0 {
		wantFrames := PcmBytesToFrames(p, uint32(remainingBytes))
		if wantFrames == 0 {
			break
		}

		buffer, _, framesToCopy, err := p.MmapBegin(wantFrames)
		if err != nil {
			if errors.Is(err, syscall.EPIPE) {
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
				return offset, p.setError(waitErr, "pcm wait failed")
			}
			if !ready { // Timeout or recovered
				if (p.flags & PCM_NONBLOCK) != 0 {
					return offset, syscall.EAGAIN
				}
				continue
			}
			continue
		}

		bytesToCopy := int(PcmFramesToBytes(p, framesToCopy))
		srcPtr := unsafe.Pointer(uintptr(dataPtr) + uintptr(offset))
		srcSlice := unsafe.Slice((*byte)(srcPtr), bytesToCopy)
		copy(buffer, srcSlice)

		if err := p.MmapCommit(framesToCopy); err != nil {
			return offset, err
		}

		offset += bytesToCopy
		remainingBytes -= bytesToCopy

		// This logic now strictly matches tinyalsa: the stream is only auto-started
		// if it's in the PREPARED state. The user must call pcm.Prepare() before
		// the first MmapWrite call.
		if p.state == PCM_STATE_PREPARED {
			avail, availErr := p.AvailUpdate()
			if availErr == nil {
				framesInBuf := p.bufferSize - uint32(avail)
				if framesInBuf >= p.config.StartThreshold {
					if startErr := p.Start(); startErr != nil {
						return offset, startErr
					}
				}
			}
		}
	}

	return offset, nil
}

// MmapRead reads interleaved audio data from a capture MMAP PCM device.
// data must be a slice of a supported numeric type (e.g., []int16, []float32) that will receive the data.
// It automatically handles waiting for data and starting the stream on the first read.
func (p *PCM) MmapRead(data any) (int, error) {
	if (p.flags & PCM_MMAP) == 0 {
		return 0, fmt.Errorf("MmapRead can only be used with MMAP streams")
	}
	if (p.flags & PCM_IN) == 0 {
		return 0, fmt.Errorf("cannot read from a playback device")
	}

	dataPtr, dataByteLen, err := checkSliceAndGetData(data)
	if err != nil {
		return 0, p.setError(err, "invalid data type for MmapRead")
	}

	offset := 0
	remainingBytes := int(dataByteLen)

	if p.state != PCM_STATE_RUNNING {
		if err := p.Start(); err != nil {
			return 0, err
		}
	}

	for remainingBytes > 0 {
		wantFrames := PcmBytesToFrames(p, uint32(remainingBytes))
		if wantFrames == 0 {
			break
		}

		buffer, _, framesToCopy, err := p.MmapBegin(wantFrames)
		if err != nil {
			if errors.Is(err, syscall.EPIPE) {
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
				return offset, p.setError(waitErr, "pcm wait failed")
			}
			if !ready { // Timeout or recovered
				if (p.flags & PCM_NONBLOCK) != 0 {
					return offset, syscall.EAGAIN
				}
				continue
			}
			continue
		}

		bytesToCopy := int(PcmFramesToBytes(p, framesToCopy))
		dstPtr := unsafe.Pointer(uintptr(dataPtr) + uintptr(offset))
		dstSlice := unsafe.Slice((*byte)(dstPtr), bytesToCopy)
		copy(dstSlice, buffer)

		if err := p.MmapCommit(framesToCopy); err != nil {
			return offset, err
		}

		offset += bytesToCopy
		remainingBytes -= bytesToCopy
	}

	return offset, nil
}

// MmapBegin prepares for a memory-mapped transfer. It returns a slice of the
// main buffer corresponding to the available contiguous space for writing or reading,
// the offset in frames from the start of the buffer, and the number of frames available in that slice.
func (p *PCM) MmapBegin(wantFrames uint32) (buffer []byte, offsetFrames, actualFrames uint32, err error) {
	if (p.flags & PCM_MMAP) == 0 {
		err = fmt.Errorf("MmapBegin() is only available for MMAP streams")

		return
	}

	if syncErr := p.ioctlSync(SNDRV_PCM_SYNC_PTR_HWSYNC); syncErr != nil {
		if p.xrunRecover(syncErr) != nil {
			// If recovery itself fails, that's a fatal error.
			return nil, 0, 0, p.setError(syncErr, "failed to recover from ioctl sync ptr")
		}

		// Even after successful recovery, return EPIPE to signal to the caller
		// that an XRUN happened and the state has changed, mirroring tinyalsa.
		return nil, 0, 0, syscall.EPIPE
	}

	p.state = PcmState(p.mmapStatus.State)

	offset := p.mmapControl.ApplPtr % SndPcmUframesT(p.bufferSize)
	offsetFrames = uint32(offset)

	var avail SndPcmUframesT
	if (p.flags & PCM_IN) != 0 {
		avail = p.mmapStatus.HwPtr - p.mmapControl.ApplPtr
	} else {
		avail = p.mmapStatus.HwPtr + SndPcmUframesT(p.bufferSize) - p.mmapControl.ApplPtr
	}

	if p.boundary > 0 && avail >= p.boundary {
		avail -= p.boundary
	}

	if avail > SndPcmUframesT(p.bufferSize) {
		avail = SndPcmUframesT(p.bufferSize)
	}

	continuousFrames := uint32(p.bufferSize) - offsetFrames

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
		err = fmt.Errorf("mmap begin calculation exceeds buffer bounds")
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

	p.mmapControl.ApplPtr += SndPcmUframesT(frames)
	if p.boundary > 0 && p.mmapControl.ApplPtr >= p.boundary {
		p.mmapControl.ApplPtr -= p.boundary
	}

	return p.ioctlSync(0)
}

// Timestamp returns available frames and the corresponding timestamp.
// The clock source is CLOCK_MONOTONIC if the PCM_MONOTONIC flag was used, otherwise it is CLOCK_REALTIME.
// This function is only available for MMAP streams.
func (p *PCM) Timestamp() (availFrames uint32, t time.Time, err error) {
	if (p.flags & PCM_MMAP) == 0 {
		err = fmt.Errorf("the Timestamp() is only available for MMAP streams")

		return
	}

	for i := 0; i < 2; i++ {
		if syncErr := p.ioctlSync(SNDRV_PCM_SYNC_PTR_HWSYNC); syncErr != nil {
			err = syncErr

			return
		}

		p.state = PcmState(p.mmapStatus.State)

		var currentAvail SndPcmUframesT
		if (p.flags & PCM_IN) != 0 {
			currentAvail = p.mmapStatus.HwPtr - p.mmapControl.ApplPtr
		} else {
			currentAvail = p.mmapStatus.HwPtr + SndPcmUframesT(p.bufferSize) - p.mmapControl.ApplPtr
		}

		if p.boundary > 0 && currentAvail >= p.boundary {
			currentAvail -= p.boundary
		}

		if i == 1 && uint32(currentAvail) == availFrames {
			return
		}

		availFrames = uint32(currentAvail)
		ts := p.mmapStatus.Tstamp
		t = time.Unix(int64(ts.Sec), int64(ts.Nsec))

		if !p.syncPtrIsMmapped {
			return
		}
	}

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

	if syncErr := p.ioctlSync(SNDRV_PCM_SYNC_PTR_HWSYNC); syncErr != nil {
		err = syncErr

		return
	}

	p.state = PcmState(p.mmapStatus.State)

	if p.state != PCM_STATE_RUNNING && p.state != PCM_STATE_DRAINING {
		err = fmt.Errorf("invalid stream state for HWPtr: %v", p.state)

		return
	}

	ts := p.mmapStatus.Tstamp
	if ts.Sec == 0 && ts.Nsec == 0 {
		err = fmt.Errorf("driver returned invalid timestamp")

		return
	}

	tstamp = time.Unix(int64(ts.Sec), int64(ts.Nsec))
	hwPtr = p.mmapStatus.HwPtr

	return
}

func (p *PCM) setError(err error, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if err != nil {
		p.lastError = fmt.Sprintf("%s: %v", msg, err)
	} else {
		p.lastError = msg
	}

	if err != nil {
		return fmt.Errorf("%s: %w", msg, err)
	}

	return fmt.Errorf("%s", msg)
}

// setupStatusAndControl maps the kernel's status and control structures into memory.
// It always attempts this, as some drivers support it even for non-MMAP streams.
// If mapping fails, it sets up a fallback structure to be used with the SYNC_PTR ioctl.
func (p *PCM) setupStatusAndControl() error {
	pageSize := os.Getpagesize()
	var statusBuf, controlBuf []byte
	var err error

	statusBuf, err = unix.Mmap(int(p.file.Fd()), SNDRV_PCM_MMAP_OFFSET_STATUS, pageSize, unix.PROT_READ, unix.MAP_SHARED)
	if err == nil {
		controlBuf, err = unix.Mmap(int(p.file.Fd()), SNDRV_PCM_MMAP_OFFSET_CONTROL, pageSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			_ = unix.Munmap(statusBuf)
		}
	}

	if err != nil {
		p.syncPtr = &sndPcmSyncPtr{}
		p.mmapStatus = &p.syncPtr.S.sndPcmMmapStatus
		p.mmapControl = &p.syncPtr.C.sndPcmMmapControl
		p.syncPtrIsMmapped = false
		p.mmapControl.AvailMin = SndPcmUframesT(p.config.AvailMin)

		return nil
	}

	p.mmapStatus = (*sndPcmMmapStatus)(unsafe.Pointer(&statusBuf[0]))
	p.mmapControl = (*sndPcmMmapControl)(unsafe.Pointer(&controlBuf[0]))
	p.syncPtrIsMmapped = true
	p.mmapControl.AvailMin = SndPcmUframesT(p.config.AvailMin)

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

	if p.mmapStatus == nil || p.mmapControl == nil {
		return fmt.Errorf("internal error: status/control pointers not initialized")
	}

	if p.syncPtrIsMmapped {
		if (flags & (1 << 1) /* HWSYNC */) != 0 {
			if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_HWSYNC, 0); err != nil {
				// Handle cases where HWSYNC is not supported even if status/control are mmapped.
				if errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.EINVAL) {
					p.noSyncPtr = true

					return nil
				}

				return err
			}
		}
	} else {
		if p.syncPtr == nil {
			return fmt.Errorf("sync pointers (fallback) not initialized")
		}

		p.syncPtr.Flags = flags
		if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_SYNC_PTR, uintptr(unsafe.Pointer(p.syncPtr))); err != nil {
			if errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.EINVAL) {
				p.noSyncPtr = true

				return nil
			}

			return err
		}
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
		// Valid types
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

// pcmFormatToRealBits returns the "real" number of bits for a format,
// which may differ from the container size (e.g., 24 for S24_LE).
// This is used for setting hardware parameters.
func pcmFormatToRealBits(f PcmFormat) uint32 {
	switch f {
	case PCM_FORMAT_S32_LE, PCM_FORMAT_S32_BE, PCM_FORMAT_U32_LE, PCM_FORMAT_U32_BE,
		PCM_FORMAT_FLOAT_LE, PCM_FORMAT_FLOAT_BE:
		return 32
	case PCM_FORMAT_S24_LE, PCM_FORMAT_S24_BE, PCM_FORMAT_U24_LE, PCM_FORMAT_U24_BE,
		PCM_FORMAT_S24_3LE, PCM_FORMAT_S24_3BE, PCM_FORMAT_U24_3LE, PCM_FORMAT_U24_3BE:
		return 24
	case PCM_FORMAT_S20_3LE, PCM_FORMAT_S20_3BE, PCM_FORMAT_U20_3LE, PCM_FORMAT_U20_3BE:
		return 20
	case PCM_FORMAT_S18_3LE, PCM_FORMAT_S18_3BE, PCM_FORMAT_U18_3LE, PCM_FORMAT_U18_3BE:
		return 18
	case PCM_FORMAT_S16_LE, PCM_FORMAT_S16_BE, PCM_FORMAT_U16_LE, PCM_FORMAT_U16_BE:
		return 16
	case PCM_FORMAT_S8, PCM_FORMAT_U8:
		return 8
	case PCM_FORMAT_FLOAT64_LE, PCM_FORMAT_FLOAT64_BE:
		return 64
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

// paramInit initializes a sndPcmHwParams struct to allow all possible values.
func paramInit(p *sndPcmHwParams) {
	for n := 0; n < len(p.Masks); n++ {
		for i := range p.Masks[n].Bits {
			p.Masks[n].Bits[i] = ^uint32(0)
		}
	}

	for n := 0; n < len(p.Intervals); n++ {
		p.Intervals[n].MinVal = 0
		p.Intervals[n].MaxVal = ^uint32(0)
		p.Intervals[n].Flags = 0
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
	if (interval.Flags & SNDRV_PCM_INTERVAL_INTEGER) != 0 {
		return interval.MaxVal
	}

	return 0
}

// PcmParamsGet queries the hardware parameters for a given PCM device without fully opening it.
// This is useful for discovering device capabilities.
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
	// Initialize params to full range before refining
	paramInit(hwParams)

	if err := ioctl(file.Fd(), SNDRV_PCM_IOCTL_HW_REFINE, uintptr(unsafe.Pointer(hwParams))); err != nil {
		return nil, fmt.Errorf("ioctl HW_REFINE failed: %w", err)
	}

	return &PcmParams{params: hwParams}, nil
}

// Free releases the resources associated with PcmParams.
func (pp *PcmParams) Free() {
	pp.params = nil
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
