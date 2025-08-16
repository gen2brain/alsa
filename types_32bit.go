//go:build linux && (386 || arm)

package alsa

// SndPcmUframesT is an unsigned long in the ALSA headers.
// On 32-bit architectures, this is a 32-bit unsigned integer.
type SndPcmUframesT = uint32

// SndPcmSframesT is a signed long in the ALSA headers.
// On 32-bit architectures, this is a 32-bit signed integer.
type SndPcmSframesT = int32

// clong is a type alias for the C `long` type on 32-bit systems.
type clong = int32

// sndPcmMmapStatus contains the status of an MMAP PCM stream.
type sndPcmMmapStatus struct {
	State          int32 // PcmState
	Pad1           int32
	HwPtr          SndPcmUframesT
	_              [4]byte
	Tstamp         kernelTimespec
	SuspendedState int32 // PcmState
	_              [4]byte
	AudioTstamp    kernelTimespec
}

// sndPcmStatus contains the current status of a PCM stream.
type sndPcmStatus struct {
	State          PcmState
	_              [4]byte // Padding
	TriggerTstamp  kernelTimespec
	Tstamp         kernelTimespec
	ApplPtr        SndPcmUframesT
	HwPtr          SndPcmUframesT
	Delay          sndPcmSframesT
	Avail          SndPcmUframesT
	AvailMax       SndPcmUframesT
	Overrange      SndPcmUframesT
	SuspendedState PcmState
	_              [28]byte // Reserved
}

// sndCtlElemValue holds the value of a control element.
type sndCtlElemValue struct {
	Id sndCtlElemId
	// This represents the `unsigned int indirect:1;` field from the C struct.
	// On 32-bit architectures, the following `Value` union is only 4-byte aligned,
	// so no extra padding is needed after this 4-byte field.
	_ [4]byte
	// Represents a C union. The largest member on 32-bit is 'long long Value[64]' (512 bytes).
	Value    [512]byte
	Reserved [128]byte
}

// sndPcmSyncPtr is used to synchronize hardware and application pointers via ioctl.
// The field order must match the C struct exactly. This definition is for 32-bit systems.
type sndPcmSyncPtr struct {
	Flags uint32
	// Padding (4 bytes) required to align the unions to 8 bytes (due to Timespec inside status).
	_ [4]byte
	S struct {
		sndPcmMmapStatus
		_ [8]byte // Padding to make the union 64 bytes
	}
	C struct {
		sndPcmMmapControl
		_ [56]byte // Padding to make the union 64 bytes
	}
}

// sndPcmSwParams contains software parameters for a PCM device for 32-bit systems.
// The layout must match the C struct exactly. This version matches older kernel ABIs
// for broader compatibility.
type sndPcmSwParams struct {
	TstampMode       uint32
	PeriodStep       uint32
	SleepMin         uint32
	AvailMin         SndPcmUframesT
	XferAlign        SndPcmUframesT
	StartThreshold   SndPcmUframesT
	StopThreshold    SndPcmUframesT
	SilenceThreshold SndPcmUframesT
	SilenceSize      SndPcmUframesT
	Boundary         SndPcmUframesT
	Reserved         [64]byte
}
