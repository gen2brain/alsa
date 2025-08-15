package alsa

// sndMask is a bitmask for hardware parameters.
type sndMask struct {
	Bits [8]uint32
}

// sndInterval represents a range of values for a hardware parameter.
type sndInterval struct {
	MinVal uint32
	MaxVal uint32
	Flags  uint32
}

// sndPcmInfo contains general information about a PCM device.
type sndPcmInfo struct {
	Device          uint32
	Subdevice       uint32
	Stream          int32
	Card            int32
	Id              [64]byte
	Name            [80]byte
	Subname         [32]byte
	DevClass        int32
	DevSubclass     int32
	SubdevicesCount uint32
	SubdevicesAvail uint32
	Sync            [16]byte // snd_sync_id_t
	Reserved        [64]byte
}

// sndCtlCardInfo contains general information about a sound card.
type sndCtlCardInfo struct {
	Card       int32
	Pad        int32
	Id         [16]byte
	Driver     [16]byte
	Name       [32]byte
	Longname   [80]byte
	Reserved_  [16]byte
	Mixername  [80]byte
	Components [128]byte
}

// sndCtlElemId identifies a single control element.
type sndCtlElemId struct {
	Numid     uint32
	Iface     int32 // snd_ctl_elem_iface_t
	Device    uint32
	Subdevice uint32
	Name      [44]byte
	Index     uint32
}

// sndCtlElemInfo contains metadata about a control element.
type sndCtlElemInfo struct {
	Id     sndCtlElemId
	Typ    int32 // snd_ctl_elem_type_t
	Access uint32
	Count  uint32
	Owner  int32
	// This represents the C union, sized to the largest member.
	// The largest member is `unsigned char reserved[128]` for TLV.
	Value [128]byte
	// Reserved field size to match modern kernel expectations
	Reserved [64]byte
}

// sndCtlEvent represents a notification from the control interface.
type sndCtlEvent struct {
	Typ  int32
	Elem sndCtlEventElement
}

// sndCtlEventElement mirrors the C union member for element-related events.
type sndCtlEventElement struct {
	Mask uint32
	Id   sndCtlElemId
}

type integer struct {
	Min  clong
	Max  clong
	Step clong
}
type integer64 struct {
	Min  int64
	Max  int64
	Step int64
}

// sndCtlEnum represents the `enumerated` member of the `snd_ctl_elem_info.value` union.
// It is used for accessing enumerated control metadata.
type sndCtlEnum struct {
	Items       uint32
	Item        uint32
	Name        [64]byte
	NamesPtr    uint64
	NamesLength uint32
}

// sndCtlTlv represents the header for a Type-Length-Value data structure.
// The variable-length data follows this header in memory.
type sndCtlTlv struct {
	Numid  uint32
	Length uint32
	Tlv    [0]byte // Variable data part
}

// SndAesEbu is the Go representation of the C struct for IEC958 (S/PDIF) data.
// It is part of the sndCtlElemValue union.
type SndAesEbu struct {
	Status      [24]byte
	Subcode     [147]byte
	Pad         byte
	DigSubframe [4]byte
}
