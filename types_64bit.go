//go:build linux && (amd64 || arm64)

package alsa

import (
	// Use unix.Timespec for consistency, although syscall.Timespec is identical on 64-bit linux.
	"golang.org/x/sys/unix"
)

// SndPcmUframesT is an unsigned long in the ALSA headers.
// On 64-bit architectures, this is a 64-bit unsigned integer.
type SndPcmUframesT = uint64

// clong is a type alias for the C `long` type on 64-bit systems.
type clong = int64

// sndXferi is for interleaved read/write operations.
type sndXferi struct {
	Result int     // Corresponds to C ssize_t
	Buf    uintptr // void*
	Frames SndPcmUframesT
}

// sndXfern is for non-interleaved read/write operations.
type sndXfern struct {
	Result int     // Corresponds to C ssize_t
	Bufs   uintptr // void**
	Frames SndPcmUframesT
}

// sndPcmHwParams contains hardware parameters for a PCM device.
type sndPcmHwParams struct {
	Flags     uint32
	Masks     [3]sndMask
	Mres      [5]sndMask // reserved for future use
	Intervals [12]sndInterval
	Ires      [9]sndInterval // reserved for future use
	Rmask     uint32
	Cmask     uint32
	Info      uint32
	Msbits    uint32
	RateNum   uint32
	RateDen   uint32
	FifoSize  SndPcmUframesT
	Reserved  [64]byte
}

// sndPcmMmapStatus contains the status of an MMAP PCM stream.
// On 64-bit systems, padding is required before AudioTstamp for alignment.
type sndPcmMmapStatus struct {
	State          int32 // PcmState
	Pad1           int32
	HwPtr          SndPcmUframesT
	Tstamp         unix.Timespec
	SuspendedState int32 // PcmState
	_              [4]byte
	AudioTstamp    unix.Timespec
}

// sndPcmMmapControl contains control parameters for an MMAP PCM stream.
type sndPcmMmapControl struct {
	ApplPtr  SndPcmUframesT
	AvailMin SndPcmUframesT
}

// sndCtlElemValue holds the value of a control element.
type sndCtlElemValue struct {
	Id sndCtlElemId
	_  [8]byte
	// The value union on 64-bit systems is 1024 bytes (long value[128] = 8*128 = 1024)
	Value    [1024]byte
	Reserved [128]byte
}

// sndCtlElemList is used to enumerate control elements.
type sndCtlElemList struct {
	Offset   uint32
	Space    uint32
	Used     uint32
	Count    uint32
	Pids     uintptr // *sndCtlElemId
	Reserved [50]byte
}

// sndPcmSyncPtr is used to synchronize hardware and application pointers via ioctl.
// The field order must match the C struct exactly. This definition is for 64-bit systems.
type sndPcmSyncPtr struct {
	Flags uint32
	_     [4]byte // Padding to align the unions
	S     struct {
		sndPcmMmapStatus
		_ [8]byte // Padding to make the union 64 bytes
	}
	C struct {
		sndPcmMmapControl
		_ [48]byte // Padding to make the union 64 bytes
	}
}

// sndPcmSwParams contains software parameters for a PCM device for 64-bit systems.
// This struct has 4 bytes of padding after SleepMin to align the following uint64 fields.
type sndPcmSwParams struct {
	TstampMode       uint32
	PeriodStep       uint32
	SleepMin         uint32
	_                [4]byte // Padding for 64-bit alignment
	AvailMin         SndPcmUframesT
	XferAlign        SndPcmUframesT
	StartThreshold   SndPcmUframesT
	StopThreshold    SndPcmUframesT
	SilenceThreshold SndPcmUframesT
	SilenceSize      SndPcmUframesT
	Boundary         SndPcmUframesT
	Reserved         [64]byte
}
