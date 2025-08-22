package alsa

import (
	"errors"
	"fmt"
	"os"
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
	SilenceThreshold uint32
	SilenceSize      uint32
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
	syncPointer        *sndPcmSyncPtr // Used for non-mmap streams or if mmap for status/control fails
	isMmapped          bool
	boundary           sndPcmUframesT
	xruns              int    // Counter for overruns/underruns
	noirqFramesPerMsec uint32 // For PCM_NOIRQ calculation
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
	}

	if err := pcm.SetConfig(config); err != nil {
		_ = pcm.Close()

		return nil, fmt.Errorf("failed to set PCM config: %w", err)
	}

	// This is critical: status/control structures (or the syncPointer fallback)
	// must be initialized for ALL streams to allow state checking.
	if err := pcm.mapStatusAndControl(); err != nil {
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

	p.unmapStatusAndControl()

	if (p.flags & PCM_MMAP) != 0 {
		_ = p.Stop()

		if p.mmapBuffer != nil {
			_ = unix.Munmap(p.mmapBuffer)
			p.mmapBuffer = nil
		}
	}

	err := p.file.Close()
	p.bufferSize = 0
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
// This function should be called before the stream is started.
func (p *PCM) SetConfig(config *Config) error {
	if config == nil {
		config = &Config{}
		config.Channels = 2
		config.Rate = 48000
		config.PeriodSize = 1024
		config.PeriodCount = 4
		config.Format = SNDRV_PCM_FORMAT_S16_LE
		config.StartThreshold = config.PeriodCount * config.PeriodSize
		config.StopThreshold = config.PeriodCount * config.PeriodSize
		config.SilenceThreshold = 0
		config.SilenceSize = 0
	} else {
		p.config = *config
	}

	hwParams := &sndPcmHwParams{}
	paramInit(hwParams)

	paramSetMask(hwParams, SNDRV_PCM_HW_PARAM_FORMAT, uint32(config.Format))
	paramSetMin(hwParams, SNDRV_PCM_HW_PARAM_PERIOD_SIZE, config.PeriodSize)
	paramSetInt(hwParams, SNDRV_PCM_HW_PARAM_CHANNELS, config.Channels)
	paramSetInt(hwParams, SNDRV_PCM_HW_PARAM_PERIODS, config.PeriodCount)
	paramSetInt(hwParams, SNDRV_PCM_HW_PARAM_RATE, config.Rate)

	if (p.flags & PCM_NOIRQ) != 0 {
		if (p.flags & PCM_MMAP) == 0 {
			return fmt.Errorf("flag PCM_NOIRQ is only supported with PCM_MMAP")
		}

		hwParams.Flags |= uint32(SNDRV_PCM_HW_PARAMS_NO_PERIOD_WAKEUP)
		if config.Rate > 0 {
			p.noirqFramesPerMsec = config.Rate / 1000
		}
	}

	if (p.flags & PCM_MMAP) != 0 {
		paramSetMask(hwParams, SNDRV_PCM_HW_PARAM_ACCESS, SNDRV_PCM_ACCESS_MMAP_INTERLEAVED)
	} else {
		paramSetMask(hwParams, SNDRV_PCM_HW_PARAM_ACCESS, SNDRV_PCM_ACCESS_RW_INTERLEAVED)
	}

	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_HW_PARAMS, uintptr(unsafe.Pointer(hwParams))); err != nil {
		return fmt.Errorf("ioctl HW_PARAMS failed: %w", err)
	}

	// Update our config with the refined parameters from the driver.
	p.config.PeriodSize = paramGetInt(hwParams, SNDRV_PCM_HW_PARAM_PERIOD_SIZE)
	p.config.PeriodCount = paramGetInt(hwParams, SNDRV_PCM_HW_PARAM_PERIODS)
	p.bufferSize = p.config.PeriodSize * p.config.PeriodCount
	p.config.Channels = paramGetInt(hwParams, SNDRV_PCM_HW_PARAM_CHANNELS)
	p.config.Rate = paramGetInt(hwParams, SNDRV_PCM_HW_PARAM_RATE)

	// Mmap the data buffer after setting HW/SW_PARAMS.
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

	// Sanity check for essential parameters
	if p.config.Channels == 0 || p.config.Rate == 0 || p.config.PeriodSize == 0 || p.config.PeriodCount == 0 {
		return fmt.Errorf("driver finalized invalid PCM configuration (Channels=%d, Rate=%d, PeriodSize=%d, PeriodCount=%d)",
			p.config.Channels, p.config.Rate, p.config.PeriodSize, p.config.PeriodCount)
	}

	swParams := &sndPcmSwParams{}
	swParams.TstampMode = 1 // SNDRV_PCM_TSTAMP_ENABLE
	swParams.PeriodStep = 1

	if config.AvailMin == 0 {
		p.config.AvailMin = p.config.PeriodSize
	}
	swParams.AvailMin = sndPcmUframesT(p.config.AvailMin)

	if config.StartThreshold == 0 {
		if (p.flags & PCM_IN) != 0 {
			swParams.StartThreshold = 1
		} else {
			swParams.StartThreshold = sndPcmUframesT(config.PeriodCount * config.PeriodSize / 2)
		}
		p.config.StartThreshold = uint32(swParams.StartThreshold)
	} else {
		swParams.StartThreshold = sndPcmUframesT(config.StartThreshold)
	}

	if config.StopThreshold == 0 {
		if (p.flags & PCM_IN) != 0 {
			swParams.StopThreshold = sndPcmUframesT(config.PeriodCount * config.PeriodSize * 10)
		} else {
			swParams.StopThreshold = sndPcmUframesT(config.PeriodCount * config.PeriodSize)
		}
		p.config.StopThreshold = uint32(swParams.StopThreshold)
	} else {
		swParams.StopThreshold = sndPcmUframesT(config.StopThreshold)
	}

	swParams.XferAlign = sndPcmUframesT(config.PeriodSize / 2) // Needed for old kernels
	swParams.SilenceSize = sndPcmUframesT(config.SilenceSize)
	swParams.SilenceThreshold = sndPcmUframesT(config.SilenceThreshold)

	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_SW_PARAMS, uintptr(unsafe.Pointer(swParams))); err != nil {
		return fmt.Errorf("ioctl SW_PARAMS (write) failed: %w", err)
	}

	p.boundary = swParams.Boundary

	return nil
}

// Prepare readies the PCM device for I/O operations.
// This is typically used to recover from an XRUN.
func (p *PCM) Prepare() error {
	err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_PREPARE, 0)
	if err != nil {
		return fmt.Errorf("ioctl PREPARE failed: %w", err)
	}

	if err := p.syncPtr(SNDRV_PCM_SYNC_PTR_APPL | SNDRV_PCM_SYNC_PTR_AVAIL_MIN); err != nil {
		return err
	}

	return nil
}

// Start explicitly starts the PCM stream.
// It ensures the stream is prepared before starting.
func (p *PCM) Start() error {
	if p.State() == SNDRV_PCM_STATE_SETUP {
		if err := p.Prepare(); err != nil {
			return err
		}
	}

	if err := p.syncPtr(0); err != nil {
		return err
	}

	if p.mmapStatus.State != SNDRV_PCM_STATE_RUNNING {
		if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_START, 0); err != nil {
			return fmt.Errorf("ioctl START failed: %w", err)
		}
	}

	return nil
}

// Stop abruptly stops the PCM stream, dropping any pending frames.
func (p *PCM) Stop() error {
	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_DROP, 0); err != nil {
		return fmt.Errorf("ioctl DROP failed: %w", err)
	}

	return nil
}

// Pause pauses or resumes the PCM stream.
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

	return nil
}

// Resume resumes a suspended PCM stream (system suspend, when state is SND_PCM_STATE_SUSPENDED).
func (p *PCM) Resume() error {
	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_RESUME, 0); err != nil {
		// EINTR is a possible outcome and should be retried by the caller if needed.
		return fmt.Errorf("ioctl RESUME failed: %w", err)
	}

	return nil
}

// Drain waits for all pending frames in the buffer to be played.
// This is a blocking call and only applies to playback streams.
func (p *PCM) Drain() error {
	if !p.IsReady() {
		return fmt.Errorf("PCM handle is not valid")
	}

	if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_DRAIN, 0); err != nil {
		return fmt.Errorf("ioctl DRAIN failed: %w", err)
	}

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
func (p *PCM) Wait(timeoutMs int) (bool, error) {
	if !p.IsReady() {
		return false, fmt.Errorf("PCM handle not ready")
	}

	pfd := []unix.PollFd{
		{
			Fd:     int32(p.file.Fd()),
			Events: unix.POLLIN | unix.POLLOUT | unix.POLLERR | unix.POLLNVAL,
		},
	}

	var n int
	var err error

	// Loop to handle EINTR (interrupted system call)
	for {
		n, err = unix.Poll(pfd, timeoutMs)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}

	// Handle poll results
	if err != nil {
		// A real error from poll()
		return false, err
	}

	if n == 0 {
		// Timeout occurred
		return false, nil
	}

	revents := pfd[0].Revents
	if (revents & (unix.POLLERR | unix.POLLNVAL)) != 0 {
		s := p.State()

		switch s {
		case SNDRV_PCM_STATE_XRUN:
			return false, fmt.Errorf("stream xrun: %w", syscall.EPIPE)
		case SNDRV_PCM_STATE_SUSPENDED:
			return false, fmt.Errorf("stream suspended: %w", syscall.ESTRPIPE)
		case SNDRV_PCM_STATE_DISCONNECTED:
			return false, fmt.Errorf("device disconnected: %w", syscall.ENODEV)
		default:
			return false, fmt.Errorf("input/output error: %w", syscall.EIO)
		}
	}

	// If we are here, the device is ready for I/O.
	return true, nil
}

// State returns the current cached state of the PCM stream.
func (p *PCM) State() PcmState {
	// Try the fast path first: sync pointers and read the state from shared memory.
	// This is most effective for MMAP streams but is attempted for all.
	if err := p.syncPtr(SNDRV_PCM_SYNC_PTR_HWSYNC); err == nil {
		// On success, we can read the state from the (potentially mmapped) status struct.
		return PcmState(atomic.LoadInt32((*int32)(unsafe.Pointer(&p.mmapStatus.State))))
	}

	// If syncPtr fails (e.g., on non-mmap stream before it's running), fall back to the more robust but slower STATUS ioctl.
	var status sndPcmStatus
	if ioctlErr := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_STATUS, uintptr(unsafe.Pointer(&status))); ioctlErr != nil {
		// If both methods fail, the device is likely unusable or disconnected.
		return SNDRV_PCM_STATE_DISCONNECTED
	}

	return status.State
}

// xrunRecover is an internal helper to recover from an XRUN.
func (p *PCM) xrunRecover(err error) error {
	isEPIPE := errors.Is(err, syscall.EPIPE)
	isESTRPIPE := errors.Is(err, unix.ESTRPIPE)

	if !isEPIPE && !isESTRPIPE {
		return err // Not an XRUN or recoverable bad state
	}

	if isEPIPE {
		p.xruns++

		return nil
	}

	if (p.flags & PCM_NORESTART) != 0 {
		return fmt.Errorf("xrun or bad state occurred with PCM_NORESTART: %w", err)
	}

	if prepErr := p.Prepare(); prepErr != nil {
		return fmt.Errorf("recovery failed: could not prepare stream: %w", prepErr)
	}

	return nil
}

// mapStatusAndControl maps the kernel's status and control structures into memory.
// If mapping fails, it sets up a fallback structure to be used with the SYNC_PTR ioctl.
func (p *PCM) mapStatusAndControl() error {
	pageSize := os.Getpagesize()
	var statusBuf, controlBuf []byte
	var err error

	// Always allocate the sync_ptr struct, which is needed for the SYNC_PTR ioctl.
	p.syncPointer = &sndPcmSyncPtr{}

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
		// The mmapStatus and mmapControl pointers will point to the fields inside our local syncPointer.
		p.mmapStatus = &p.syncPointer.S.sndPcmMmapStatus
		p.mmapControl = &p.syncPointer.C.sndPcmMmapControl
		p.isMmapped = false
	} else {
		// Success: Use the mmapped memory directly.
		p.mmapStatus = (*sndPcmMmapStatus)(unsafe.Pointer(&statusBuf[0]))
		p.mmapControl = (*sndPcmMmapControl)(unsafe.Pointer(&controlBuf[0]))
		p.isMmapped = true
	}

	// Initialize AvailMin in the control structure (shared or fallback).
	var availMin = sndPcmUframesT(p.config.AvailMin)
	if unsafe.Sizeof(availMin) == 8 {
		atomic.StoreUint64((*uint64)(unsafe.Pointer(&p.mmapControl.AvailMin)), uint64(availMin))
	} else {
		atomic.StoreUint32((*uint32)(unsafe.Pointer(&p.mmapControl.AvailMin)), uint32(availMin))
	}

	return nil
}

func (p *PCM) unmapStatusAndControl() {
	if p.isMmapped {
		pageSize := os.Getpagesize()
		if p.mmapStatus != nil {
			buf := unsafe.Slice((*byte)(unsafe.Pointer(p.mmapStatus)), pageSize)
			_ = unix.Munmap(buf)
		}

		if p.mmapControl != nil {
			buf := unsafe.Slice((*byte)(unsafe.Pointer(p.mmapControl)), pageSize)
			_ = unix.Munmap(buf)
		}
	} else {
		p.syncPointer = nil
	}

	p.mmapStatus = nil
	p.mmapControl = nil
}

// syncPtr synchronizes the application and hardware pointers.
// This is used for all stream types to query state and manage pointers.
func (p *PCM) syncPtr(flags uint32) error {
	if p.syncPointer == nil {
		return fmt.Errorf("sync pointer not initialized")
	}

	if !p.isMmapped { // Fallback case, uses SYNC_PTR ioctl
		p.syncPointer.Flags = flags
		if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_SYNC_PTR, uintptr(unsafe.Pointer(p.syncPointer))); err != nil {
			return err
		}
	} else {
		if (flags & SNDRV_PCM_SYNC_PTR_APPL) != 0 {
			p.syncPointer.Flags = flags
			if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_SYNC_PTR, uintptr(unsafe.Pointer(p.syncPointer))); err != nil {
				return err
			}
		} else if (flags & SNDRV_PCM_SYNC_PTR_HWSYNC) != 0 {
			// If only HWSYNC is needed, the more lightweight ioctl is sufficient.
			if err := ioctl(p.file.Fd(), SNDRV_PCM_IOCTL_HWSYNC, 0); err != nil {
				return err
			}
		}
	}

	return nil
}

// PcmFormatToBits returns the number of bits per sample for a given format.
// This reflects the space occupied in memory, so 24-bit formats in 32-bit containers return 32.
func PcmFormatToBits(f PcmFormat) uint32 {
	switch f {
	case SNDRV_PCM_FORMAT_FLOAT64_LE, SNDRV_PCM_FORMAT_FLOAT64_BE:
		return 64
	case SNDRV_PCM_FORMAT_S32_LE, SNDRV_PCM_FORMAT_S32_BE, SNDRV_PCM_FORMAT_U32_LE, SNDRV_PCM_FORMAT_U32_BE,
		SNDRV_PCM_FORMAT_FLOAT_LE, SNDRV_PCM_FORMAT_FLOAT_BE,
		SNDRV_PCM_FORMAT_S24_LE, SNDRV_PCM_FORMAT_S24_BE, SNDRV_PCM_FORMAT_U24_LE, SNDRV_PCM_FORMAT_U24_BE:
		return 32
	case SNDRV_PCM_FORMAT_S24_3LE, SNDRV_PCM_FORMAT_S24_3BE, SNDRV_PCM_FORMAT_U24_3LE, SNDRV_PCM_FORMAT_U24_3BE:
		return 24
	case SNDRV_PCM_FORMAT_S16_LE, SNDRV_PCM_FORMAT_S16_BE, SNDRV_PCM_FORMAT_U16_LE, SNDRV_PCM_FORMAT_U16_BE:
		return 16
	case SNDRV_PCM_FORMAT_S8, SNDRV_PCM_FORMAT_U8:
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
