//go:build linux && (386 || arm || mips || mipsle)

package alsa

// sndPcmUframesT is an unsigned long in the ALSA headers.
// On 32-bit architectures, this is a 32-bit unsigned integer.
type sndPcmUframesT = uint32

// sndPcmSframesT is a signed long in the ALSA headers.
// On 32-bit architectures, this is a 32-bit signed integer.
type sndPcmSframesT = int32

// clong is a type alias for the C `long` type on 32-bit systems.
type clong = int32

// timespec matches the struct timespec.
type timespec struct {
	Sec  int32
	Nsec int32
}

// sndPcmMmapStatus contains the status of an MMAP PCM stream.
type sndPcmMmapStatus struct {
	State          PcmState
	Pad1           int32
	HwPtr          sndPcmUframesT
	Tstamp         timespec
	SuspendedState PcmState
	AudioTstamp    timespec
}

// sndPcmStatus contains the current status of a PCM stream.
type sndPcmStatus struct {
	State               PcmState
	TriggerTstamp       timespec
	Tstamp              timespec
	ApplPtr             sndPcmUframesT
	HwPtr               sndPcmUframesT
	Delay               sndPcmSframesT
	Avail               sndPcmUframesT
	AvailMax            sndPcmUframesT
	Overrange           sndPcmUframesT
	SuspendedState      PcmState
	AudioTstampData     uint32
	AudioTstamp         timespec
	DriverTstamp        timespec
	AudioTstampAccuracy uint32
	Reserved            [36]byte
}

// sndPcmSyncPtr is used to synchronize hardware and application pointers via ioctl.
// The field order must match the C struct exactly. This definition is for 32-bit systems.
type sndPcmSyncPtr struct {
	Flags uint32
	S     struct {
		sndPcmMmapStatus          // sndPcmMmapStatus is 32 bytes
		_                [32]byte // Padding to make the union 64 bytes
	}
	C struct {
		sndPcmMmapControl          // 8 bytes
		_                 [56]byte // Padding to make the union 64 bytes
	}
}

// sndPcmSwParams contains software parameters for a PCM device for 32-bit systems.
// The layout must match the C struct exactly. This version matches older kernel ABIs
// for broader compatibility.
type sndPcmSwParams struct {
	TstampMode       uint32
	PeriodStep       uint32
	SleepMin         uint32
	AvailMin         sndPcmUframesT
	XferAlign        sndPcmUframesT
	StartThreshold   sndPcmUframesT
	StopThreshold    sndPcmUframesT
	SilenceThreshold sndPcmUframesT
	SilenceSize      sndPcmUframesT
	Boundary         sndPcmUframesT
	Reserved         [64]byte
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
