package alsa

import (
	"errors"
	"fmt"
	"os"
	"reflect"
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
			_ = file.Close()

			return nil, fmt.Errorf("fcntl F_GETFL for %s failed: %w", path, err)
		}
		if _, err = unix.FcntlInt(file.Fd(), unix.F_SETFL, currentFlags&^syscall.O_NONBLOCK); err != nil {
			_ = file.Close()

			return nil, fmt.Errorf("failed to set blocking mode on %s: %w", path, err)
		}
	}

	var info sndPcmInfo
	if err := ioctl(file.Fd(), SNDRV_PCM_IOCTL_INFO, uintptr(unsafe.Pointer(&info))); err != nil {
		_ = file.Close()

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
		_ = pcm.Close()

		return nil, fmt.Errorf("failed to set PCM config: %w", err)
	}

	// This is critical: status/control structures (or the syncPtr fallback)
	// must be initialized for ALL streams to allow state checking.
	if err := pcm.setupStatusAndControl(); err != nil {
		_ = pcm.Close()

		return nil, fmt.Errorf("failed to set up status and control: %w", err)
	}

	// Set timestamp type if requested.
	if (flags & PCM_MONOTONIC) != 0 {
		// SNDRV_PCM_TSTAMP_TYPE_MONOTONIC = 1
		var arg int32 = 1
		if err := ioctl(pcm.file.Fd(), SNDRV_PCM_IOCTL_TTSTAMP, uintptr(unsafe.Pointer(&arg))); err != nil {
			_ = pcm.Close()

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
			return fmt.Errorf("flag PCM_NOIRQ is only supported with PCM_MMAP")
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
	// This function returns the internally tracked state.
	// For kernel-level state, see KernelState().
	if p == nil {
		return PCM_STATE_DISCONNECTED, errors.New("pcm is nil")
	}

	return p.getState(), nil
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

	// If the mmap'd state seems okay, but poll reported an error (POLLERR/POLLHUP/POLLNVAL), we must use the ioctl
	// to get the definitive status from the kernel. This is the only way to be 100% sure, and is only done on an error path.
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
