// Package alsa provides a Go interface to the Linux ALSA subsystem, modeled after the tinyalsa library.
package alsa

// PcmFormat defines the sample format for a PCM stream.
// These values correspond to the SNDRV_PCM_FORMAT_* constants in the ALSA kernel headers.
type PcmFormat int32

const (
	PCM_FORMAT_INVALID            PcmFormat = -1
	PCM_FORMAT_S8                 PcmFormat = 0
	PCM_FORMAT_U8                 PcmFormat = 1
	PCM_FORMAT_S16_LE             PcmFormat = 2
	PCM_FORMAT_S16_BE             PcmFormat = 3
	PCM_FORMAT_U16_LE             PcmFormat = 4
	PCM_FORMAT_U16_BE             PcmFormat = 5
	PCM_FORMAT_S24_LE             PcmFormat = 6
	PCM_FORMAT_S24_BE             PcmFormat = 7
	PCM_FORMAT_U24_LE             PcmFormat = 8
	PCM_FORMAT_U24_BE             PcmFormat = 9
	PCM_FORMAT_S32_LE             PcmFormat = 10
	PCM_FORMAT_S32_BE             PcmFormat = 11
	PCM_FORMAT_U32_LE             PcmFormat = 12
	PCM_FORMAT_U32_BE             PcmFormat = 13
	PCM_FORMAT_FLOAT_LE           PcmFormat = 14
	PCM_FORMAT_FLOAT_BE           PcmFormat = 15
	PCM_FORMAT_FLOAT64_LE         PcmFormat = 16
	PCM_FORMAT_FLOAT64_BE         PcmFormat = 17
	PCM_FORMAT_IEC958_SUBFRAME_LE PcmFormat = 18
	PCM_FORMAT_IEC958_SUBFRAME_BE PcmFormat = 19
	PCM_FORMAT_MU_LAW             PcmFormat = 20
	PCM_FORMAT_A_LAW              PcmFormat = 21
	PCM_FORMAT_IMA_ADPCM          PcmFormat = 22
	PCM_FORMAT_MPEG               PcmFormat = 23
	PCM_FORMAT_GSM                PcmFormat = 24
	PCM_FORMAT_SPECIAL            PcmFormat = 31
	PCM_FORMAT_S24_3LE            PcmFormat = 32
	PCM_FORMAT_S24_3BE            PcmFormat = 33
	PCM_FORMAT_U24_3LE            PcmFormat = 34
	PCM_FORMAT_U24_3BE            PcmFormat = 35
	PCM_FORMAT_S20_3LE            PcmFormat = 36
	PCM_FORMAT_S20_3BE            PcmFormat = 37
	PCM_FORMAT_U20_3LE            PcmFormat = 38
	PCM_FORMAT_U20_3BE            PcmFormat = 39
	PCM_FORMAT_S18_3LE            PcmFormat = 40
	PCM_FORMAT_S18_3BE            PcmFormat = 41
	PCM_FORMAT_U18_3LE            PcmFormat = 42
	PCM_FORMAT_U18_3BE            PcmFormat = 43
)

// PcmState defines the current state of a PCM stream.
// These values correspond to the SNDRV_PCM_STATE_* constants.
type PcmState int

const (
	PCM_STATE_OPEN         PcmState = 0 // Stream is open.
	PCM_STATE_SETUP        PcmState = 1 // Stream has a setup.
	PCM_STATE_PREPARED     PcmState = 2 // Stream is ready to start.
	PCM_STATE_RUNNING      PcmState = 3 // Stream is running.
	PCM_STATE_XRUN         PcmState = 4 // Stream reached an underrun or overrun.
	PCM_STATE_DRAINING     PcmState = 5 // Stream is draining.
	PCM_STATE_PAUSED       PcmState = 6 // Stream is paused.
	PCM_STATE_SUSPENDED    PcmState = 7 // Hardware is suspended.
	PCM_STATE_DISCONNECTED PcmState = 8 // Hardware is disconnected.
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
)

// MixerCtlType defines the value type of mixer control.
type MixerCtlType int32

const (
	MIXER_CTL_TYPE_BOOL    MixerCtlType = 0
	MIXER_CTL_TYPE_INT     MixerCtlType = 1
	MIXER_CTL_TYPE_ENUM    MixerCtlType = 2
	MIXER_CTL_TYPE_BYTE    MixerCtlType = 3
	MIXER_CTL_TYPE_IEC958  MixerCtlType = 4
	MIXER_CTL_TYPE_INT64   MixerCtlType = 5
	MIXER_CTL_TYPE_UNKNOWN MixerCtlType = -1
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
	SNDRV_PCM_SYNC_PTR_APPL      = 1 << 0
	SNDRV_PCM_SYNC_PTR_AVAIL_MIN = 1 << 2
	SNDRV_PCM_SYNC_PTR_HWSYNC    = 1 << 1
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
	PCM_PARAM_ACCESS       PcmParam = 0
	PCM_PARAM_FORMAT       PcmParam = 1
	PCM_PARAM_SUBFORMAT    PcmParam = 2
	PCM_PARAM_SAMPLE_BITS  PcmParam = 8
	PCM_PARAM_FRAME_BITS   PcmParam = 9
	PCM_PARAM_CHANNELS     PcmParam = 10
	PCM_PARAM_RATE         PcmParam = 11
	PCM_PARAM_PERIOD_TIME  PcmParam = 12
	PCM_PARAM_PERIOD_SIZE  PcmParam = 13
	PCM_PARAM_PERIOD_BYTES PcmParam = 14
	PCM_PARAM_PERIODS      PcmParam = 15
	PCM_PARAM_BUFFER_TIME  PcmParam = 16
	PCM_PARAM_BUFFER_SIZE  PcmParam = 17
	PCM_PARAM_BUFFER_BYTES PcmParam = 18
	PCM_PARAM_TICK_TIME    PcmParam = 19
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
	PCM_FORMAT_S8:                 "S8",
	PCM_FORMAT_U8:                 "U8",
	PCM_FORMAT_S16_LE:             "S16_LE",
	PCM_FORMAT_S16_BE:             "S16_BE",
	PCM_FORMAT_U16_LE:             "U16_LE",
	PCM_FORMAT_U16_BE:             "U16_BE",
	PCM_FORMAT_S24_LE:             "S24_LE",
	PCM_FORMAT_S24_BE:             "S24_BE",
	PCM_FORMAT_U24_LE:             "U24_LE",
	PCM_FORMAT_U24_BE:             "U24_BE",
	PCM_FORMAT_S32_LE:             "S32_LE",
	PCM_FORMAT_S32_BE:             "S32_BE",
	PCM_FORMAT_U32_LE:             "U32_LE",
	PCM_FORMAT_U32_BE:             "U32_BE",
	PCM_FORMAT_FLOAT_LE:           "FLOAT_LE",
	PCM_FORMAT_FLOAT_BE:           "FLOAT_BE",
	PCM_FORMAT_FLOAT64_LE:         "FLOAT64_LE",
	PCM_FORMAT_FLOAT64_BE:         "FLOAT64_BE",
	PCM_FORMAT_IEC958_SUBFRAME_LE: "IEC958_SUBFRAME_LE",
	PCM_FORMAT_IEC958_SUBFRAME_BE: "IEC958_SUBFRAME_BE",
	PCM_FORMAT_MU_LAW:             "MU_LAW",
	PCM_FORMAT_A_LAW:              "A_LAW",
	PCM_FORMAT_IMA_ADPCM:          "IMA_ADPCM",
	PCM_FORMAT_MPEG:               "MPEG",
	PCM_FORMAT_GSM:                "GSM",
	PCM_FORMAT_SPECIAL:            "SPECIAL",
	PCM_FORMAT_S24_3LE:            "S24_3LE",
	PCM_FORMAT_S24_3BE:            "S24_3BE",
	PCM_FORMAT_U24_3LE:            "U24_3LE",
	PCM_FORMAT_U24_3BE:            "U24_3BE",
	PCM_FORMAT_S20_3LE:            "S20_3LE",
	PCM_FORMAT_S20_3BE:            "S20_3BE",
	PCM_FORMAT_U20_3LE:            "U20_3LE",
	PCM_FORMAT_U20_3BE:            "U20_3BE",
	PCM_FORMAT_S18_3LE:            "S18_3LE",
	PCM_FORMAT_S18_3BE:            "S18_3BE",
	PCM_FORMAT_U18_3LE:            "U18_3LE",
	PCM_FORMAT_U18_3BE:            "U18_3BE",
}

// PcmParamSubformatNames provides human-readable names for PCM subformats.
// The index corresponds to the SNDRV_PCM_SUBFORMAT_* value.
var PcmParamSubformatNames = []string{
	"STD",
}
