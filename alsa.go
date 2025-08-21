// Package alsa provides a Go interface to the Linux ALSA subsystem, modeled after the tinyalsa library.
package alsa

// PcmFormat defines the sample format for a PCM stream.
// These values correspond to the SNDRV_PCM_FORMAT_* constants in the ALSA kernel headers.
type PcmFormat int32

const (
	SNDRV_PCM_FORMAT_INVALID            PcmFormat = -1
	SNDRV_PCM_FORMAT_S8                 PcmFormat = 0
	SNDRV_PCM_FORMAT_U8                 PcmFormat = 1
	SNDRV_PCM_FORMAT_S16_LE             PcmFormat = 2
	SNDRV_PCM_FORMAT_S16_BE             PcmFormat = 3
	SNDRV_PCM_FORMAT_U16_LE             PcmFormat = 4
	SNDRV_PCM_FORMAT_U16_BE             PcmFormat = 5
	SNDRV_PCM_FORMAT_S24_LE             PcmFormat = 6
	SNDRV_PCM_FORMAT_S24_BE             PcmFormat = 7
	SNDRV_PCM_FORMAT_U24_LE             PcmFormat = 8
	SNDRV_PCM_FORMAT_U24_BE             PcmFormat = 9
	SNDRV_PCM_FORMAT_S32_LE             PcmFormat = 10
	SNDRV_PCM_FORMAT_S32_BE             PcmFormat = 11
	SNDRV_PCM_FORMAT_U32_LE             PcmFormat = 12
	SNDRV_PCM_FORMAT_U32_BE             PcmFormat = 13
	SNDRV_PCM_FORMAT_FLOAT_LE           PcmFormat = 14
	SNDRV_PCM_FORMAT_FLOAT_BE           PcmFormat = 15
	SNDRV_PCM_FORMAT_FLOAT64_LE         PcmFormat = 16
	SNDRV_PCM_FORMAT_FLOAT64_BE         PcmFormat = 17
	SNDRV_PCM_FORMAT_IEC958_SUBFRAME_LE PcmFormat = 18
	SNDRV_PCM_FORMAT_IEC958_SUBFRAME_BE PcmFormat = 19
	SNDRV_PCM_FORMAT_MU_LAW             PcmFormat = 20
	SNDRV_PCM_FORMAT_A_LAW              PcmFormat = 21
	SNDRV_PCM_FORMAT_IMA_ADPCM          PcmFormat = 22
	SNDRV_PCM_FORMAT_MPEG               PcmFormat = 23
	SNDRV_PCM_FORMAT_GSM                PcmFormat = 24
	SNDRV_PCM_FORMAT_SPECIAL            PcmFormat = 31
	SNDRV_PCM_FORMAT_S24_3LE            PcmFormat = 32
	SNDRV_PCM_FORMAT_S24_3BE            PcmFormat = 33
	SNDRV_PCM_FORMAT_U24_3LE            PcmFormat = 34
	SNDRV_PCM_FORMAT_U24_3BE            PcmFormat = 35
	SNDRV_PCM_FORMAT_S20_3LE            PcmFormat = 36
	SNDRV_PCM_FORMAT_S20_3BE            PcmFormat = 37
	SNDRV_PCM_FORMAT_U20_3LE            PcmFormat = 38
	SNDRV_PCM_FORMAT_U20_3BE            PcmFormat = 39
	SNDRV_PCM_FORMAT_S18_3LE            PcmFormat = 40
	SNDRV_PCM_FORMAT_S18_3BE            PcmFormat = 41
	SNDRV_PCM_FORMAT_U18_3LE            PcmFormat = 42
	SNDRV_PCM_FORMAT_U18_3BE            PcmFormat = 43
)

// PcmState defines the current state of a PCM stream.
// These values correspond to the SNDRV_PCM_STATE_* constants.
type PcmState int32

const (
	SNDRV_PCM_STATE_OPEN         PcmState = 0 // Stream is open.
	SNDRV_PCM_STATE_SETUP        PcmState = 1 // Stream has a setup.
	SNDRV_PCM_STATE_PREPARED     PcmState = 2 // Stream is ready to start.
	SNDRV_PCM_STATE_RUNNING      PcmState = 3 // Stream is running.
	SNDRV_PCM_STATE_XRUN         PcmState = 4 // Stream reached an underrun or overrun.
	SNDRV_PCM_STATE_DRAINING     PcmState = 5 // Stream is draining.
	SNDRV_PCM_STATE_PAUSED       PcmState = 6 // Stream is paused.
	SNDRV_PCM_STATE_SUSPENDED    PcmState = 7 // Hardware is suspended.
	SNDRV_PCM_STATE_DISCONNECTED PcmState = 8 // Hardware is disconnected.
)

// PcmFlag defines flags for opening a PCM stream.
type PcmFlag uint32

const (
	// PCM_OUT specifies a playback stream.
	PCM_OUT PcmFlag = 0
	// PCM_IN specifies a capture stream.
	PCM_IN PcmFlag = 0x10000000

	// PCM_MMAP specifies that the stream will use memory-mapped I/O.
	PCM_MMAP PcmFlag = 0x00000001
	// PCM_NONBLOCK specifies that I/O operations should not block.
	PCM_NONBLOCK PcmFlag = 0x00000010
	// PCM_NORESTART specifies that the driver should not automatically restart the stream on underrun.
	PCM_NORESTART PcmFlag = 0x00000002
	// PCM_MONOTONIC requests monotonic timestamps instead of wall clock time.
	PCM_MONOTONIC PcmFlag = 0x00000004
	// PCM_NOIRQ specifies that the driver should not generate IRQs for every period.
	// This is an optimization for MMAP streams that can reduce CPU usage.
	// When used, Wait() will use a calculated timeout instead of blocking indefinitely.
	PCM_NOIRQ PcmFlag = 0x00000008
	// PCM_NONINTERLEAVED specifies that the stream will use non-interleaved I/O.
	// This applies only to standard read/write, not MMAP.
	PCM_NONINTERLEAVED PcmFlag = 0x00000020
)

// MixerCtlType defines the value type of mixer control.
type MixerCtlType int32

const (
	SNDRV_CTL_ELEM_TYPE_NONE       MixerCtlType = 0
	SNDRV_CTL_ELEM_TYPE_BOOLEAN    MixerCtlType = 1
	SNDRV_CTL_ELEM_TYPE_INTEGER    MixerCtlType = 2
	SNDRV_CTL_ELEM_TYPE_ENUMERATED MixerCtlType = 3
	SNDRV_CTL_ELEM_TYPE_BYTES      MixerCtlType = 4
	SNDRV_CTL_ELEM_TYPE_IEC958     MixerCtlType = 5
	SNDRV_CTL_ELEM_TYPE_INTEGER64  MixerCtlType = 6
	SNDRV_CTL_ELEM_TYPE_UNKNOWN    MixerCtlType = -1
)

// CtlAccessFlag defines the access permissions for a mixer control.
type CtlAccessFlag uint32

const (
	// If set, the control is readable.
	SNDRV_CTL_ELEM_ACCESS_READ CtlAccessFlag = 1 << 0
	// If set, the control is writable.
	SNDRV_CTL_ELEM_ACCESS_WRITE CtlAccessFlag = 1 << 1
	// If set, the control uses the TLV mechanism for custom data structures.
	SNDRV_CTL_ELEM_ACCESS_TLV_READWRITE CtlAccessFlag = 1 << 13
)

// Constants for the bitfields within snd_interval.flags to match C enum.
const (
	SNDRV_PCM_INTERVAL_OPENMIN = 1 << 0
	SNDRV_PCM_INTERVAL_OPENMAX = 1 << 1
	SNDRV_PCM_INTERVAL_INTEGER = 1 << 2
	SNDRV_PCM_INTERVAL_EMPTY   = 1 << 3
)

// ALSA MMAP offsets for status and control pages.
const (
	SNDRV_PCM_MMAP_OFFSET_STATUS  = 0x80000000
	SNDRV_PCM_MMAP_OFFSET_CONTROL = 0x81000000
)

const (
	SNDRV_PCM_SYNC_PTR_HWSYNC    = 1 << 0
	SNDRV_PCM_SYNC_PTR_APPL      = 1 << 1
	SNDRV_PCM_SYNC_PTR_AVAIL_MIN = 1 << 2
)

// PcmAccess defines the type of PCM access.
type PcmAccess int32

const (
	SNDRV_PCM_ACCESS_MMAP_INTERLEAVED    = 0
	SNDRV_PCM_ACCESS_MMAP_NONINTERLEAVED = 1
	SNDRV_PCM_ACCESS_MMAP_COMPLEX        = 2
	SNDRV_PCM_ACCESS_RW_INTERLEAVED      = 3
	SNDRV_PCM_ACCESS_RW_NONINTERLEAVED   = 4
)

// MixerEventType defines the type of event generated by the mixer.
type MixerEventType uint32

const (
	SNDRV_CTL_EVENT_ELEM = 0

	// Indicates that a control element's value has changed.
	SNDRV_CTL_EVENT_MASK_VALUE MixerEventType = 1 << 0
	// Indicates that a control element's metadata (e.g., range) has changed.
	SNDRV_CTL_EVENT_MASK_INFO MixerEventType = 1 << 1
	// Indicates that a control element has been added.
	SNDRV_CTL_EVENT_MASK_ADD MixerEventType = 1 << 2
	// Indicates a control element has been removed.
	SNDRV_CTL_EVENT_MASK_REMOVE MixerEventType = 1 << 3
)

// MixerEvent represents a notification from the ALSA control interface.
type MixerEvent struct {
	Type      MixerEventType
	ControlID uint32 // The numid of the control that changed.
}

// PcmParam identifies a hardware parameter for a PCM device.
// These values correspond to the SNDRV_PCM_HW_PARAM_* constants.
type PcmParam int

const (
	SNDRV_PCM_HW_PARAM_ACCESS       PcmParam = 0
	SNDRV_PCM_HW_PARAM_FORMAT       PcmParam = 1
	SNDRV_PCM_HW_PARAM_SUBFORMAT    PcmParam = 2
	SNDRV_PCM_HW_PARAM_SAMPLE_BITS  PcmParam = 8
	SNDRV_PCM_HW_PARAM_FRAME_BITS   PcmParam = 9
	SNDRV_PCM_HW_PARAM_CHANNELS     PcmParam = 10
	SNDRV_PCM_HW_PARAM_RATE         PcmParam = 11
	SNDRV_PCM_HW_PARAM_PERIOD_TIME  PcmParam = 12
	SNDRV_PCM_HW_PARAM_PERIOD_SIZE  PcmParam = 13
	SNDRV_PCM_HW_PARAM_PERIOD_BYTES PcmParam = 14
	SNDRV_PCM_HW_PARAM_PERIODS      PcmParam = 15
	SNDRV_PCM_HW_PARAM_BUFFER_TIME  PcmParam = 16
	SNDRV_PCM_HW_PARAM_BUFFER_SIZE  PcmParam = 17
	SNDRV_PCM_HW_PARAM_BUFFER_BYTES PcmParam = 18
	SNDRV_PCM_HW_PARAM_TICK_TIME    PcmParam = 19

	SNDRV_PCM_HW_PARAMS_NO_RESAMPLE      PcmParam = 1 << 0
	SNDRV_PCM_HW_PARAMS_EXPORT_BUFFER    PcmParam = 1 << 1
	SNDRV_PCM_HW_PARAMS_NO_PERIOD_WAKEUP PcmParam = 1 << 2
	SNDRV_PCM_HW_PARAMS_NO_DRAIN_SILENCE PcmParam = 1 << 3
)

// PcmParamMask represents a bitmask for a PCM hardware parameter.
// It allows checking which specific capabilities (e.g., formats) are supported.
type PcmParamMask struct {
	bits [8]uint32 // Corresponds to sndMask->bits
}

// Test checks if a specific bit in the mask is set.
func (m *PcmParamMask) Test(bit uint) bool {
	if bit >= 256 { // SNDRV_MASK_MAX
		return false
	}

	element := bit >> 5             // bit / 32
	mask := uint32(1 << (bit & 31)) // bit % 32

	return (m.bits[element] & mask) != 0
}

// PcmParamAccessNames provides human-readable names for PCM access types.
// The index corresponds to the SNDRV_PCM_ACCESS_* value.
var PcmParamAccessNames = []string{
	"MMAP_INTERLEAVED",
	"MMAP_NONINTERLEAVED",
	"MMAP_COMPLEX",
	"RW_INTERLEAVED",
	"RW_NONINTERLEAVED",
}

// PcmParamFormatNames provides human-readable names for PCM formats.
// The index corresponds to the PcmFormat (SNDRV_PCM_FORMAT_*) value.
var PcmParamFormatNames = map[PcmFormat]string{
	SNDRV_PCM_FORMAT_S8:                 "S8",
	SNDRV_PCM_FORMAT_U8:                 "U8",
	SNDRV_PCM_FORMAT_S16_LE:             "S16_LE",
	SNDRV_PCM_FORMAT_S16_BE:             "S16_BE",
	SNDRV_PCM_FORMAT_U16_LE:             "U16_LE",
	SNDRV_PCM_FORMAT_U16_BE:             "U16_BE",
	SNDRV_PCM_FORMAT_S24_LE:             "S24_LE",
	SNDRV_PCM_FORMAT_S24_BE:             "S24_BE",
	SNDRV_PCM_FORMAT_U24_LE:             "U24_LE",
	SNDRV_PCM_FORMAT_U24_BE:             "U24_BE",
	SNDRV_PCM_FORMAT_S32_LE:             "S32_LE",
	SNDRV_PCM_FORMAT_S32_BE:             "S32_BE",
	SNDRV_PCM_FORMAT_U32_LE:             "U32_LE",
	SNDRV_PCM_FORMAT_U32_BE:             "U32_BE",
	SNDRV_PCM_FORMAT_FLOAT_LE:           "FLOAT_LE",
	SNDRV_PCM_FORMAT_FLOAT_BE:           "FLOAT_BE",
	SNDRV_PCM_FORMAT_FLOAT64_LE:         "FLOAT64_LE",
	SNDRV_PCM_FORMAT_FLOAT64_BE:         "FLOAT64_BE",
	SNDRV_PCM_FORMAT_IEC958_SUBFRAME_LE: "IEC958_SUBFRAME_LE",
	SNDRV_PCM_FORMAT_IEC958_SUBFRAME_BE: "IEC958_SUBFRAME_BE",
	SNDRV_PCM_FORMAT_MU_LAW:             "MU_LAW",
	SNDRV_PCM_FORMAT_A_LAW:              "A_LAW",
	SNDRV_PCM_FORMAT_IMA_ADPCM:          "IMA_ADPCM",
	SNDRV_PCM_FORMAT_MPEG:               "MPEG",
	SNDRV_PCM_FORMAT_GSM:                "GSM",
	SNDRV_PCM_FORMAT_SPECIAL:            "SPECIAL",
	SNDRV_PCM_FORMAT_S24_3LE:            "S24_3LE",
	SNDRV_PCM_FORMAT_S24_3BE:            "S24_3BE",
	SNDRV_PCM_FORMAT_U24_3LE:            "U24_3LE",
	SNDRV_PCM_FORMAT_U24_3BE:            "U24_3BE",
	SNDRV_PCM_FORMAT_S20_3LE:            "S20_3LE",
	SNDRV_PCM_FORMAT_S20_3BE:            "S20_3BE",
	SNDRV_PCM_FORMAT_U20_3LE:            "U20_3LE",
	SNDRV_PCM_FORMAT_U20_3BE:            "U20_3BE",
	SNDRV_PCM_FORMAT_S18_3LE:            "S18_3LE",
	SNDRV_PCM_FORMAT_S18_3BE:            "S18_3BE",
	SNDRV_PCM_FORMAT_U18_3LE:            "U18_3LE",
	SNDRV_PCM_FORMAT_U18_3BE:            "U18_3BE",
}

// PcmParamSubformatNames provides human-readable names for PCM subformats.
// The index corresponds to the SNDRV_PCM_SUBFORMAT_* value.
var PcmParamSubformatNames = []string{
	"STD",
}
