package alsa

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// MixerCtl represents an individual mixer control handle.
type MixerCtl struct {
	mixer *Mixer
	info  sndCtlElemInfo
	ename []string // Cache for enumerated item names
}

// Mixer represents an open ALSA mixer device handle.
type Mixer struct {
	file     *os.File
	cardInfo sndCtlCardInfo
	Ctls     []*MixerCtl
	ctlMap   map[string][]*MixerCtl // Maps a name to one or more controls
	ctlIdMap map[uint32]*MixerCtl   // Maps a numid to its control for O(1) access
}

// MixerOpen opens the ALSA mixer for a given sound card and enumerates its controls.
// Note: This implementation does not support the ALSA plugin architecture and will only open direct hardware control devices (e.g., /dev/snd/controlC0).
func MixerOpen(card uint) (*Mixer, error) {
	path := fmt.Sprintf("/dev/snd/controlC%d", card)

	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open mixer device %s: %w", path, err)
	}

	mixer := &Mixer{
		file:     file,
		ctlMap:   make(map[string][]*MixerCtl),
		ctlIdMap: make(map[uint32]*MixerCtl),
	}

	// Get card info
	if err := ioctl(mixer.file.Fd(), SNDRV_CTL_IOCTL_CARD_INFO, uintptr(unsafe.Pointer(&mixer.cardInfo))); err != nil {
		_ = mixer.Close()

		return nil, fmt.Errorf("ioctl CARD_INFO failed: %w", err)
	}

	if err := mixer.enumerateAllControls(); err != nil {
		_ = mixer.Close()

		return nil, fmt.Errorf("failed to enumerate controls: %w", err)
	}

	return mixer, nil
}

// Close closes the mixer device handle.
func (m *Mixer) Close() error {
	if m == nil || m.file == nil {
		return nil
	}

	err := m.file.Close()
	m.file = nil

	return err
}

// Name returns the name of the sound card.
func (m *Mixer) Name() string {
	if m == nil {
		return ""
	}

	return cString(m.cardInfo.Name[:])
}

// NumCtls returns the total number of controls found on the mixer.
func (m *Mixer) NumCtls() int {
	if m == nil {
		return 0
	}

	return len(m.Ctls)
}

// NumCtlsByName returns the number of controls that match the given name.
func (m *Mixer) NumCtlsByName(name string) int {
	if m == nil {
		return 0
	}

	return len(m.ctlMap[name])
}

// Ctl returns a mixer control by its numeric ID (`numid`).
func (m *Mixer) Ctl(id uint32) (*MixerCtl, error) {
	if m == nil {
		return nil, fmt.Errorf("mixer is nil")
	}

	ctl, ok := m.ctlIdMap[id]
	if !ok {
		return nil, fmt.Errorf("control with id %d not found", id)
	}

	return ctl, nil
}

// CtlByIndex returns a mixer control by its 0-based index in the enumerated list.
// The index is valid from 0 to NumCtls() - 1.
func (m *Mixer) CtlByIndex(index uint) (*MixerCtl, error) {
	if m == nil {
		return nil, fmt.Errorf("mixer is nil")
	}

	if index >= uint(m.NumCtls()) {
		return nil, fmt.Errorf("index %d is out of bounds (number of controls: %d)", index, m.NumCtls())
	}

	return m.Ctls[index], nil
}

// CtlByName returns the first mixer control found with the given name.
func (m *Mixer) CtlByName(name string) (*MixerCtl, error) {
	if m == nil {
		return nil, fmt.Errorf("mixer is nil")
	}

	return m.CtlByNameAndIndex(name, 0)
}

// CtlByNameAndIndex returns a specific mixer control handle by name and index.
func (m *Mixer) CtlByNameAndIndex(name string, index uint) (*MixerCtl, error) {
	if m == nil {
		return nil, fmt.Errorf("mixer is nil")
	}

	ctls, ok := m.ctlMap[name]
	if !ok {
		return nil, fmt.Errorf("control not found: %s", name)
	}

	if index >= uint(len(ctls)) {
		return nil, fmt.Errorf("index %d out of bounds for control %s", index, name)
	}

	return ctls[index], nil
}

// CtlByNameAndDevice returns a mixer control handle by name and device number.
func (m *Mixer) CtlByNameAndDevice(name string, device uint32) (*MixerCtl, error) {
	if m == nil {
		return nil, fmt.Errorf("mixer is nil")
	}

	ctls, ok := m.ctlMap[name]
	if !ok {
		return nil, fmt.Errorf("control not found: %s", name)
	}

	for _, ctl := range ctls {
		if ctl.Device() == device {
			return ctl, nil
		}
	}

	return nil, fmt.Errorf("control %s with device %d not found", name, device)
}

// CtlByNameAndSubdevice returns a mixer control handle by name and subdevice number.
func (m *Mixer) CtlByNameAndSubdevice(name string, subdevice uint32) (*MixerCtl, error) {
	if m == nil {
		return nil, fmt.Errorf("mixer is nil")
	}

	ctls, ok := m.ctlMap[name]
	if !ok {
		return nil, fmt.Errorf("control not found: %s", name)
	}

	for _, ctl := range ctls {
		if ctl.Subdevice() == subdevice {
			return ctl, nil
		}
	}

	return nil, fmt.Errorf("control %s with subdevice %d not found", name, subdevice)
}

// AddNewCtls scans for and adds any new controls that have appeared since the mixer was opened.
func (m *Mixer) AddNewCtls() error {
	if m == nil {
		return fmt.Errorf("mixer is nil")
	}

	list := &sndCtlElemList{}
	if err := ioctl(m.file.Fd(), SNDRV_CTL_IOCTL_ELEM_LIST, uintptr(unsafe.Pointer(list))); err != nil {
		return fmt.Errorf("ioctl ELEM_LIST (get count) failed: %w", err)
	}

	currentCount := uint32(len(m.Ctls))
	kernelCount := list.Count

	if kernelCount <= currentCount {
		return nil // No new controls
	}

	numToAdd := kernelCount - currentCount
	ids := make([]sndCtlElemId, numToAdd)
	list.Space = numToAdd
	list.Offset = currentCount // Start enumerating from the first new control.
	list.Pids = uintptr(unsafe.Pointer(&ids[0]))

	if err := ioctl(m.file.Fd(), SNDRV_CTL_IOCTL_ELEM_LIST, uintptr(unsafe.Pointer(list))); err != nil {
		return fmt.Errorf("ioctl ELEM_LIST (get new ids) failed: %w", err)
	}

	for i := uint32(0); i < list.Used; i++ {
		info := sndCtlElemInfo{}
		info.Id = ids[i]

		if err := ioctl(m.file.Fd(), SNDRV_CTL_IOCTL_ELEM_INFO, uintptr(unsafe.Pointer(&info))); err != nil {
			continue
		}

		ctl := &MixerCtl{
			mixer: m,
			info:  info,
		}

		name := ctl.Name()
		m.Ctls = append(m.Ctls, ctl)
		m.ctlMap[name] = append(m.ctlMap[name], ctl)
		m.ctlIdMap[ctl.ID()] = ctl // Populate the ID map for new controls
	}

	return nil
}

// SubscribeEvents enables or disables event generation for this mixer handle.
func (m *Mixer) SubscribeEvents(enable bool) error {
	if m == nil {
		return fmt.Errorf("mixer is nil")
	}

	var val int32
	if enable {
		val = 1
	}

	if err := ioctl(m.file.Fd(), SNDRV_CTL_IOCTL_SUBSCRIBE_EVENTS, uintptr(unsafe.Pointer(&val))); err != nil {
		return fmt.Errorf("ioctl SUBSCRIBE_EVENTS failed: %w", err)
	}

	return nil
}

// WaitEvent waits for a mixer event to occur.
// It returns true if an event is pending, false on timeout.
func (m *Mixer) WaitEvent(timeoutMs int) (bool, error) {
	if m == nil {
		return false, fmt.Errorf("mixer is nil")
	}

	pfd := []unix.PollFd{
		{Fd: int32(m.file.Fd()), Events: unix.POLLIN},
	}

	n, err := unix.Poll(pfd, timeoutMs)
	if err != nil {
		return false, err
	}

	if n == 0 {
		return false, nil // Timeout
	}

	if (pfd[0].Revents & unix.POLLIN) != 0 {
		return true, nil
	}

	return false, fmt.Errorf("poll returned with unexpected revents: %d", pfd[0].Revents)
}

// ReadEvent reads a pending mixer event from the device.
func (m *Mixer) ReadEvent() (*MixerEvent, error) {
	if m == nil {
		return nil, fmt.Errorf("mixer is nil")
	}

	var ev sndCtlEvent
	evSize := unsafe.Sizeof(ev)
	buffer := make([]byte, evSize)

	n, err := unix.Read(int(m.file.Fd()), buffer)
	if err != nil {
		return nil, err
	}

	if n < int(evSize) {
		return nil, fmt.Errorf("short read for event: got %d bytes, want %d", n, evSize)
	}

	ev = *(*sndCtlEvent)(unsafe.Pointer(&buffer[0]))
	event := &MixerEvent{Type: MixerEventType(ev.Typ)}

	// The mask indicates what kind of event it is. For VALUE, INFO, ADD, and REMOVE,
	// the event data contains the element ID.
	const eventMaskWithID = SNDRV_CTL_EVENT_MASK_VALUE | SNDRV_CTL_EVENT_MASK_INFO | SNDRV_CTL_EVENT_MASK_ADD | SNDRV_CTL_EVENT_MASK_REMOVE

	if (event.Type & eventMaskWithID) != 0 {
		event.ControlID = ev.Elem.Id.Numid
	}

	return event, nil
}

// ConsumeEvent reads and discards a single pending mixer event.
func (m *Mixer) ConsumeEvent() error {
	if m == nil {
		return fmt.Errorf("mixer is nil")
	}

	_, err := m.ReadEvent()

	return err
}

// Fd returns the underlying file descriptor for the mixer device.
func (m *Mixer) Fd() uintptr {
	if m == nil {
		return ^uintptr(0) // Invalid FD
	}

	return m.file.Fd()
}

// Name returns the name of the control.
func (ctl *MixerCtl) Name() string {
	if ctl == nil {
		return ""
	}

	return cString(ctl.info.Id.Name[:])
}

// ID returns the numeric ID (`numid`) of the control.
func (ctl *MixerCtl) ID() uint32 {
	if ctl == nil {
		return ^uint32(0)
	}

	return ctl.info.Id.Numid
}

// Device returns the device number associated with the control.
func (ctl *MixerCtl) Device() uint32 {
	if ctl == nil {
		return 0
	}

	return ctl.info.Id.Device
}

// Subdevice returns the subdevice number associated with the control.
func (ctl *MixerCtl) Subdevice() uint32 {
	if ctl == nil {
		return 0
	}

	return ctl.info.Id.Subdevice
}

// NumValues returns the number of values for the control (e.g., 2 for stereo volume).
func (ctl *MixerCtl) NumValues() uint32 {
	if ctl == nil {
		return 0
	}

	return ctl.info.Count
}

// Access returns the control access.
func (ctl *MixerCtl) Access() uint32 {
	if ctl == nil {
		return 0
	}

	return ctl.info.Access
}

// Type returns the data type of the control's value.
func (ctl *MixerCtl) Type() MixerCtlType {
	if ctl == nil {
		return MIXER_CTL_TYPE_UNKNOWN
	}

	// This is a workaround for drivers that report a control as ENUMERATED
	// but provide no item names. These are often misreported INTEGER controls.
	// A non-zero `max` in the integer union is a strong indicator.
	if MixerCtlType(ctl.info.Typ) == MIXER_CTL_TYPE_ENUM {
		intInfo := (*integer)(unsafe.Pointer(&ctl.info.Value[0]))
		if intInfo.Max != 0 {
			return MIXER_CTL_TYPE_INT
		}
	}

	return MixerCtlType(ctl.info.Typ)
}

// TypeString returns a string representation of the control's data type.
func (ctl *MixerCtl) TypeString() string {
	if ctl == nil {
		return "UNKNOWN"
	}

	switch ctl.Type() {
	case MIXER_CTL_TYPE_BOOL:
		return "BOOL"
	case MIXER_CTL_TYPE_INT:
		return "INT"
	case MIXER_CTL_TYPE_ENUM:
		return "ENUM"
	case MIXER_CTL_TYPE_BYTE:
		return "BYTE"
	case MIXER_CTL_TYPE_IEC958:
		return "IEC958"
	case MIXER_CTL_TYPE_INT64:
		return "INT64"
	default:
		return "UNKNOWN"
	}
}

// Update refreshes the control's information from the kernel. This is useful
// for checking for changes to a control's properties (like value ranges)
// after a mixer event of type SNDRV_CTL_EVENT_MASK_INFO.
func (ctl *MixerCtl) Update() error {
	if ctl == nil {
		return fmt.Errorf("MixerCtl is nil")
	}

	return ioctl(ctl.mixer.file.Fd(), SNDRV_CTL_IOCTL_ELEM_INFO, uintptr(unsafe.Pointer(&ctl.info)))
}

// IsAccessTLVRw checks if the control uses the TLV mechanism for custom data.
func (ctl *MixerCtl) IsAccessTLVRw() bool {
	if ctl == nil {
		return false
	}

	return (ctl.info.Access & uint32(SNDRV_CTL_ELEM_ACCESS_TLV_READWRITE)) != 0
}

// Value reads a single value from a control at a given index.
// This is for controls with types BOOL, INT, ENUM, and BYTE.
// For INT64 controls, use Value64.
func (ctl *MixerCtl) Value(index uint) (int, error) {
	if ctl == nil {
		return 0, fmt.Errorf("MixerCtl is nil")
	}

	count := ctl.NumValues()
	if index >= uint(count) {
		return 0, fmt.Errorf("index out of bounds")
	}

	val := &sndCtlElemValue{}
	val.Id = ctl.info.Id

	if err := ioctl(ctl.mixer.file.Fd(), SNDRV_CTL_IOCTL_ELEM_READ, uintptr(unsafe.Pointer(val))); err != nil {
		return 0, err
	}

	valuePtr := unsafe.Pointer(&val.Value[0])
	switch ctl.Type() {
	case MIXER_CTL_TYPE_BOOL, MIXER_CTL_TYPE_INT, MIXER_CTL_TYPE_ENUM:
		intValues := unsafe.Slice((*int32)(valuePtr), count)

		return int(intValues[index]), nil
	case MIXER_CTL_TYPE_BYTE:
		byteValues := unsafe.Slice((*byte)(valuePtr), count)

		return int(byteValues[index]), nil
	default:
		return 0, fmt.Errorf("unsupported control type for Value: %v. Use Value64 or Array", ctl.Type())
	}
}

// SetValue writes a single value to a control at a given index.
func (ctl *MixerCtl) SetValue(index uint, value int) error {
	if ctl == nil {
		return fmt.Errorf("MixerCtl is nil")
	}

	count := ctl.NumValues()
	if index >= uint(count) {
		return fmt.Errorf("index out of bounds")
	}

	val := &sndCtlElemValue{}
	val.Id = ctl.info.Id

	// A read is necessary first to fill the struct with current values.
	// This prevents accidentally modifying other channels or properties.
	if err := ioctl(ctl.mixer.file.Fd(), SNDRV_CTL_IOCTL_ELEM_READ, uintptr(unsafe.Pointer(val))); err != nil {
		return err
	}

	valuePtr := unsafe.Pointer(&val.Value[0])
	switch ctl.Type() {
	case MIXER_CTL_TYPE_BOOL, MIXER_CTL_TYPE_INT, MIXER_CTL_TYPE_ENUM:
		intValues := unsafe.Slice((*int32)(valuePtr), count)
		intValues[index] = int32(value)
	case MIXER_CTL_TYPE_BYTE:
		byteValues := unsafe.Slice((*byte)(valuePtr), count)
		byteValues[index] = byte(value)
	default:
		return fmt.Errorf("unsupported control type for SetValue: %v. Use SetValue64 or SetArray", ctl.Type())
	}

	return ioctl(ctl.mixer.file.Fd(), SNDRV_CTL_IOCTL_ELEM_WRITE, uintptr(unsafe.Pointer(val)))
}

// Value64 reads a single 64-bit integer value from a control at a given index.
func (ctl *MixerCtl) Value64(index uint) (int64, error) {
	if ctl == nil {
		return 0, fmt.Errorf("MixerCtl is nil")
	}

	if ctl.Type() != MIXER_CTL_TYPE_INT64 {
		return 0, fmt.Errorf("not an INT64 control")
	}

	count := ctl.NumValues()
	if index >= uint(count) {
		return 0, fmt.Errorf("index out of bounds")
	}

	val := &sndCtlElemValue{}
	val.Id = ctl.info.Id

	if err := ioctl(ctl.mixer.file.Fd(), SNDRV_CTL_IOCTL_ELEM_READ, uintptr(unsafe.Pointer(val))); err != nil {
		return 0, err
	}

	int64Values := unsafe.Slice((*int64)(unsafe.Pointer(&val.Value[0])), count)

	return int64Values[index], nil
}

// SetValue64 writes a single 64-bit integer value to a control at a given index.
func (ctl *MixerCtl) SetValue64(index uint, value int64) error {
	if ctl == nil {
		return fmt.Errorf("MixerCtl is nil")
	}

	if ctl.Type() != MIXER_CTL_TYPE_INT64 {
		return fmt.Errorf("not an INT64 control")
	}

	count := ctl.NumValues()
	if index >= uint(count) {
		return fmt.Errorf("index out of bounds")
	}

	val := &sndCtlElemValue{}
	val.Id = ctl.info.Id

	// A read is necessary first to fill the struct with current values.
	if err := ioctl(ctl.mixer.file.Fd(), SNDRV_CTL_IOCTL_ELEM_READ, uintptr(unsafe.Pointer(val))); err != nil {
		return err
	}

	int64Values := unsafe.Slice((*int64)(unsafe.Pointer(&val.Value[0])), count)
	int64Values[index] = value

	return ioctl(ctl.mixer.file.Fd(), SNDRV_CTL_IOCTL_ELEM_WRITE, uintptr(unsafe.Pointer(val)))
}

// Array reads the entire multi-value array of a control.
// `array` must be a pointer to a slice of a type compatible with the control's type:
//   - MIXER_CTL_TYPE_BOOL, _INT, _ENUM: *[]int32
//   - MIXER_CTL_TYPE_BYTE: *[]byte
//   - MIXER_CTL_TYPE_INT64: *[]int64
//   - MIXER_CTL_TYPE_IEC958: *[]SndAesEbu (typically only one value)
func (ctl *MixerCtl) Array(array any) error {
	if ctl == nil {
		return fmt.Errorf("MixerCtl is nil")
	}

	if ctl.info.Count == 0 {
		return nil
	}

	if array == nil {
		return fmt.Errorf("array cannot be nil")
	}

	ptr := reflect.ValueOf(array)
	if ptr.Kind() != reflect.Ptr || ptr.IsNil() || ptr.Elem().Kind() != reflect.Slice {
		return fmt.Errorf("expected a non-nil pointer to a slice")
	}

	slice := ptr.Elem()
	count := int(ctl.info.Count)

	// Handle TLV bytes separately as it uses a different ioctl
	if ctl.Type() == MIXER_CTL_TYPE_BYTE && ctl.IsAccessTLVRw() {
		if slice.Type().Elem().Kind() != reflect.Uint8 {
			return fmt.Errorf("type mismatch: expected *[]byte for TLV control")
		}

		const maxTlvSize = 4096
		tlvHdrSize := unsafe.Sizeof(sndCtlTlv{})
		tlvBuf := make([]byte, int(tlvHdrSize)+maxTlvSize)
		tlv := (*sndCtlTlv)(unsafe.Pointer(&tlvBuf[0]))
		tlv.Numid = ctl.ID()
		tlv.Length = maxTlvSize

		if err := ioctl(ctl.mixer.file.Fd(), SNDRV_CTL_IOCTL_TLV_READ, uintptr(unsafe.Pointer(tlv))); err != nil {
			return fmt.Errorf("ioctl TLV_READ failed: %w", err)
		}

		actualLen := int(tlv.Length)
		resultSlice := reflect.MakeSlice(slice.Type(), actualLen, actualLen)
		dataSrc := tlvBuf[tlvHdrSize : tlvHdrSize+uintptr(actualLen)]
		reflect.Copy(resultSlice, reflect.ValueOf(dataSrc))
		ptr.Elem().Set(resultSlice)

		return nil
	}

	val := &sndCtlElemValue{}
	val.Id = ctl.info.Id
	if err := ioctl(ctl.mixer.file.Fd(), SNDRV_CTL_IOCTL_ELEM_READ, uintptr(unsafe.Pointer(val))); err != nil {
		return err
	}

	resultSlice := reflect.MakeSlice(slice.Type(), count, count)
	valuePtr := unsafe.Pointer(&val.Value[0])

	// Switch on the actual control type
	switch ctl.Type() {
	case MIXER_CTL_TYPE_BOOL, MIXER_CTL_TYPE_INT, MIXER_CTL_TYPE_ENUM:
		if slice.Type().Elem().Kind() != reflect.Int32 {
			return fmt.Errorf("type mismatch: expected *[]int32 for control type %s", ctl.TypeString())
		}

		src := unsafe.Slice((*int32)(valuePtr), count)
		reflect.Copy(resultSlice, reflect.ValueOf(src))

	case MIXER_CTL_TYPE_BYTE:
		if slice.Type().Elem().Kind() != reflect.Uint8 {
			return fmt.Errorf("type mismatch: expected *[]byte for BYTE control")
		}

		src := unsafe.Slice((*byte)(valuePtr), count)
		reflect.Copy(resultSlice, reflect.ValueOf(src))

	case MIXER_CTL_TYPE_INT64:
		if slice.Type().Elem().Kind() != reflect.Int64 {
			return fmt.Errorf("type mismatch: expected *[]int64 for INT64 control")
		}

		src := unsafe.Slice((*int64)(valuePtr), count)
		reflect.Copy(resultSlice, reflect.ValueOf(src))

	case MIXER_CTL_TYPE_IEC958:
		if slice.Type().Elem() != reflect.TypeOf(SndAesEbu{}) {
			return fmt.Errorf("type mismatch: expected *[]SndAesEbu for IEC958 control")
		}

		src := unsafe.Slice((*SndAesEbu)(valuePtr), count)
		reflect.Copy(resultSlice, reflect.ValueOf(src))

	default:
		return fmt.Errorf("unsupported control type for Array: %v", ctl.Type())
	}

	ptr.Elem().Set(resultSlice)

	return nil
}

// SetArray writes an entire multi-value array to a control.
// `array` must be a slice of a type compatible with the control's type:
//   - MIXER_CTL_TYPE_BOOL, _INT, _ENUM: []int32
//   - MIXER_CTL_TYPE_BYTE: []byte
//   - MIXER_CTL_TYPE_INT64: []int64
//   - MIXER_CTL_TYPE_IEC958: []SndAesEbu
func (ctl *MixerCtl) SetArray(array any) error {
	if ctl == nil {
		return fmt.Errorf("MixerCtl is nil")
	}

	if array == nil {
		return fmt.Errorf("array cannot be nil")
	}

	slice := reflect.ValueOf(array)
	if slice.Kind() != reflect.Slice {
		return fmt.Errorf("expected a slice")
	}

	count := slice.Len()
	if uint32(count) != ctl.info.Count {
		return fmt.Errorf("array length %d does not match control count %d", count, ctl.info.Count)
	}

	// Handle TLV bytes separately as it uses a different ioctl
	if ctl.Type() == MIXER_CTL_TYPE_BYTE && ctl.IsAccessTLVRw() {
		if slice.Type().Elem().Kind() != reflect.Uint8 {
			return fmt.Errorf("type mismatch: expected []byte for TLV control")
		}

		dataLen := slice.Len()
		tlvHdrSize := unsafe.Sizeof(sndCtlTlv{})
		tlvBuf := make([]byte, int(tlvHdrSize)+dataLen)
		tlv := (*sndCtlTlv)(unsafe.Pointer(&tlvBuf[0]))
		tlv.Numid = ctl.ID()
		tlv.Length = uint32(dataLen)

		dataDst := tlvBuf[tlvHdrSize:]
		reflect.Copy(reflect.ValueOf(dataDst), slice)

		return ioctl(ctl.mixer.file.Fd(), SNDRV_CTL_IOCTL_TLV_WRITE, uintptr(unsafe.Pointer(tlv)))
	}

	val := &sndCtlElemValue{}
	val.Id = ctl.info.Id
	valuePtr := unsafe.Pointer(&val.Value[0])

	switch ctl.Type() {
	case MIXER_CTL_TYPE_BOOL, MIXER_CTL_TYPE_INT, MIXER_CTL_TYPE_ENUM:
		if slice.Type().Elem().Kind() != reflect.Int32 {
			return fmt.Errorf("type mismatch: expected []int32 for control type %s", ctl.TypeString())
		}

		dest := unsafe.Slice((*int32)(valuePtr), count)
		reflect.Copy(reflect.ValueOf(dest), slice)

	case MIXER_CTL_TYPE_BYTE:
		if slice.Type().Elem().Kind() != reflect.Uint8 {
			return fmt.Errorf("type mismatch: expected []byte for BYTE control")
		}

		dest := unsafe.Slice((*byte)(valuePtr), count)
		reflect.Copy(reflect.ValueOf(dest), slice)

	case MIXER_CTL_TYPE_INT64:
		if slice.Type().Elem().Kind() != reflect.Int64 {
			return fmt.Errorf("type mismatch: expected []int64 for INT64 control")
		}

		dest := unsafe.Slice((*int64)(valuePtr), count)
		reflect.Copy(reflect.ValueOf(dest), slice)

	case MIXER_CTL_TYPE_IEC958:
		if slice.Type().Elem() != reflect.TypeOf(SndAesEbu{}) {
			return fmt.Errorf("type mismatch: expected []SndAesEbu for IEC958 control")
		}

		dest := unsafe.Slice((*SndAesEbu)(valuePtr), count)
		reflect.Copy(reflect.ValueOf(dest), slice)

	default:
		return fmt.Errorf("unsupported control type for SetArray: %v", ctl.Type())
	}

	return ioctl(ctl.mixer.file.Fd(), SNDRV_CTL_IOCTL_ELEM_WRITE, uintptr(unsafe.Pointer(val)))
}

// Percent gets the control's value as a percentage (0-100).
func (ctl *MixerCtl) Percent(index uint) (int, error) {
	if ctl == nil {
		return 0, fmt.Errorf("MixerCtl is nil")
	}

	if ctl.Type() != MIXER_CTL_TYPE_INT {
		return 0, fmt.Errorf("not an integer control")
	}

	if index >= uint(ctl.NumValues()) {
		return 0, fmt.Errorf("index out of bounds")
	}

	rangeMin, err := ctl.RangeMin()
	if err != nil {
		return 0, err
	}

	rangeMax, err := ctl.RangeMax()
	if err != nil {
		return 0, err
	}

	val, err := ctl.Value(index)
	if err != nil {
		return 0, err
	}

	if (rangeMax - rangeMin) == 0 {
		return 0, nil
	}

	return (100 * (val - rangeMin)) / (rangeMax - rangeMin), nil
}

// SetPercent sets the control's value as a percentage (0-100).
func (ctl *MixerCtl) SetPercent(index uint, percent int) error {
	if ctl == nil {
		return fmt.Errorf("MixerCtl is nil")
	}

	if ctl.Type() != MIXER_CTL_TYPE_INT {
		return fmt.Errorf("not an integer control")
	}

	if index >= uint(ctl.NumValues()) {
		return fmt.Errorf("index out of bounds")
	}

	if percent < 0 || percent > 100 {
		return fmt.Errorf("percent must be between 0 and 100")
	}

	rangeMin, err := ctl.RangeMin()
	if err != nil {
		return err
	}

	rangeMax, err := ctl.RangeMax()
	if err != nil {
		return err
	}

	val := rangeMin + ((rangeMax - rangeMin) * percent / 100)

	return ctl.SetValue(index, val)
}

// RangeMin returns the minimum value for an integer control.
func (ctl *MixerCtl) RangeMin() (int, error) {
	if ctl == nil {
		return 0, fmt.Errorf("MixerCtl is nil")
	}

	if ctl.Type() != MIXER_CTL_TYPE_INT {
		return 0, fmt.Errorf("not an integer control")
	}

	intInfo := (*integer)(unsafe.Pointer(&ctl.info.Value[0]))

	return int(intInfo.Min), nil
}

// RangeMax returns the maximum value for an integer control.
func (ctl *MixerCtl) RangeMax() (int, error) {
	if ctl == nil {
		return 0, fmt.Errorf("MixerCtl is nil")
	}

	if ctl.Type() != MIXER_CTL_TYPE_INT {
		return 0, fmt.Errorf("not an integer control")
	}

	intInfo := (*integer)(unsafe.Pointer(&ctl.info.Value[0]))

	return int(intInfo.Max), nil
}

// RangeMin64 returns the minimum value for an int64 control.
func (ctl *MixerCtl) RangeMin64() (int64, error) {
	if ctl == nil {
		return 0, fmt.Errorf("MixerCtl is nil")
	}

	if ctl.Type() != MIXER_CTL_TYPE_INT64 {
		return 0, fmt.Errorf("not an int64 control")
	}

	int64Info := (*integer64)(unsafe.Pointer(&ctl.info.Value[0]))

	return int64Info.Min, nil
}

// RangeMax64 returns the maximum value for an int64 control.
func (ctl *MixerCtl) RangeMax64() (int64, error) {
	if ctl == nil {
		return 0, fmt.Errorf("MixerCtl is nil")
	}

	if ctl.Type() != MIXER_CTL_TYPE_INT64 {
		return 0, fmt.Errorf("not an int64 control")
	}

	int64Info := (*integer64)(unsafe.Pointer(&ctl.info.Value[0]))

	return int64Info.Max, nil
}

// NumEnums returns the number of items for an enumerated control.
func (ctl *MixerCtl) NumEnums() (uint32, error) {
	if ctl == nil {
		return 0, fmt.Errorf("MixerCtl is nil")
	}

	if ctl.Type() != MIXER_CTL_TYPE_ENUM {
		return 0, fmt.Errorf("not an enumerated control")
	}

	// Cast the value union to our enumerated struct representation to read the item count.
	enumInfo := (*sndCtlEnum)(unsafe.Pointer(&ctl.info.Value[0]))

	return enumInfo.Items, nil
}

// EnumString returns the string associated with an enumerated value.
func (ctl *MixerCtl) EnumString(enumID uint) (string, error) {
	if ctl == nil {
		return "", fmt.Errorf("MixerCtl is nil")
	}

	if ctl.Type() != MIXER_CTL_TYPE_ENUM {
		return "", fmt.Errorf("not an enumerated control")
	}

	if ctl.ename == nil {
		if err := ctl.fillEnumStrings(); err != nil {
			return "", err
		}
	}

	if enumID >= uint(len(ctl.ename)) {
		return "", fmt.Errorf("enum ID out of bounds")
	}

	return ctl.ename[enumID], nil
}

// EnumValueString gets the string representation of the currently selected enumerated value.
// This is a convenience function that combines Value and EnumString.
func (ctl *MixerCtl) EnumValueString(index uint) (string, error) {
	if ctl == nil {
		return "", fmt.Errorf("MixerCtl is nil")
	}

	if ctl.Type() != MIXER_CTL_TYPE_ENUM {
		return "", fmt.Errorf("not an enumerated control")
	}

	if uint32(index) >= ctl.NumValues() {
		return "", fmt.Errorf("index %d is out of bounds", index)
	}

	// Get the integer value for the specified channel/index of the control
	enumValue, err := ctl.Value(index)
	if err != nil {
		return "", fmt.Errorf("failed to get control value: %w", err)
	}

	// Get the string corresponding to that integer value
	return ctl.EnumString(uint(enumValue))
}

// AllEnumStrings returns a slice containing all possible string values for an enumerated control.
func (ctl *MixerCtl) AllEnumStrings() ([]string, error) {
	if ctl == nil {
		return nil, fmt.Errorf("MixerCtl is nil")
	}

	if ctl.Type() != MIXER_CTL_TYPE_ENUM {
		return nil, fmt.Errorf("not an enumerated control")
	}

	if ctl.ename == nil {
		if err := ctl.fillEnumStrings(); err != nil {
			return nil, err
		}
	}

	// Return a copy to prevent modification of the internal cache.
	namesCopy := make([]string, len(ctl.ename))
	copy(namesCopy, ctl.ename)

	return namesCopy, nil
}

// SetEnumByString sets the value of an enumerated control by its string name.
func (ctl *MixerCtl) SetEnumByString(value string) error {
	if ctl == nil {
		return fmt.Errorf("MixerCtl is nil")
	}

	if ctl.Type() != MIXER_CTL_TYPE_ENUM {
		return fmt.Errorf("not an enumerated control")
	}

	if ctl.ename == nil {
		if err := ctl.fillEnumStrings(); err != nil {
			return err
		}
	}

	var enumIndex = -1
	for i, name := range ctl.ename {
		if name == value {
			enumIndex = i
			break
		}
	}

	if enumIndex == -1 {
		return fmt.Errorf("enum string '%s' not found", value)
	}

	val := &sndCtlElemValue{}
	val.Id = ctl.info.Id

	count := ctl.NumValues()
	intValues := unsafe.Slice((*int32)(unsafe.Pointer(&val.Value[0])), count)
	for i := uint32(0); i < count; i++ {
		intValues[i] = int32(enumIndex)
	}

	return ioctl(ctl.mixer.file.Fd(), SNDRV_CTL_IOCTL_ELEM_WRITE, uintptr(unsafe.Pointer(val)))
}

// fillEnumStrings caches the names of all possible values for an enum control.
func (ctl *MixerCtl) fillEnumStrings() error {
	numEnums, err := ctl.NumEnums()
	if err != nil {
		return err
	}

	// Sanity check to prevent OOM on buggy drivers that report nonsensical values.
	if numEnums > 1024 {
		return fmt.Errorf("driver reported unreasonable number of enum items: %d", numEnums)
	}

	ctl.ename = make([]string, numEnums)

	for i := uint32(0); i < numEnums; i++ {
		// Create a fresh, zeroed info struct for each query.
		var tempInfo sndCtlElemInfo

		// To query for an enum name, we identify the control by its numid
		// and specify which item name we want. This matches tinyalsa's logic.
		tempInfo.Id.Numid = ctl.info.Id.Numid

		// Cast the value union to our enumerated struct representation.
		enumInfoPtr := (*sndCtlEnum)(unsafe.Pointer(&tempInfo.Value[0]))

		// Set the input `item` index we are querying for.
		enumInfoPtr.Item = i

		// The ELEM_INFO ioctl uses the ID and `item` index as input and fills
		// the rest of the struct, including the `name` for that item.
		if err := ioctl(ctl.mixer.file.Fd(), SNDRV_CTL_IOCTL_ELEM_INFO, uintptr(unsafe.Pointer(&tempInfo))); err != nil {
			return fmt.Errorf("ioctl ELEM_INFO for enum item %d failed: %w", i, err)
		}

		// Now read the name filled in by the kernel from the populated tempInfo struct.
		ctl.ename[i] = cString(enumInfoPtr.Name[:])
	}

	return nil
}

// enumerateAllControls gets the information for every control on the mixer.
func (m *Mixer) enumerateAllControls() error {
	list := &sndCtlElemList{}

	// First call: get the count of controls
	if err := ioctl(m.file.Fd(), SNDRV_CTL_IOCTL_ELEM_LIST, uintptr(unsafe.Pointer(list))); err != nil {
		return fmt.Errorf("ioctl ELEM_LIST (get count) failed: %w", err)
	}

	count := list.Count
	if count == 0 {
		return nil
	}

	m.Ctls = make([]*MixerCtl, 0, count)
	ids := make([]sndCtlElemId, count)

	// Second call: get the actual control IDs
	list.Space = count
	list.Pids = uintptr(unsafe.Pointer(&ids[0]))

	if err := ioctl(m.file.Fd(), SNDRV_CTL_IOCTL_ELEM_LIST, uintptr(unsafe.Pointer(list))); err != nil {
		return fmt.Errorf("ioctl ELEM_LIST (get ids) failed: %w", err)
	}

	// Now enumerate each control by getting its detailed info
	for i := uint32(0); i < list.Used; i++ {
		info := sndCtlElemInfo{}
		info.Id = ids[i] // Copy the entire ID structure

		if err := ioctl(m.file.Fd(), SNDRV_CTL_IOCTL_ELEM_INFO, uintptr(unsafe.Pointer(&info))); err != nil {
			// Skip controls that we can't read info for
			continue
		}

		ctl := &MixerCtl{
			mixer: m,
			info:  info,
		}

		name := ctl.Name()
		m.Ctls = append(m.Ctls, ctl)
		m.ctlMap[name] = append(m.ctlMap[name], ctl)
		m.ctlIdMap[ctl.ID()] = ctl
	}

	return nil
}

// String returns a human-readable representation of the mixer control's properties and current state.
func (ctl *MixerCtl) String() string {
	if ctl == nil {
		return "<nil>"
	}

	var b strings.Builder

	// Helper for formatting key-value pairs with alignment
	kv := func(key, format string, args ...any) {
		b.WriteString(fmt.Sprintf("%-14s: ", key))
		b.WriteString(fmt.Sprintf(format, args...))
		b.WriteString("\n")
	}

	kv("Name", "'%s'", ctl.Name())
	kv("ID", "%d", ctl.ID())
	kv("Type", "%s", ctl.TypeString())
	kv("Values", "%d", ctl.NumValues())

	var accessFlags []string
	if (ctl.Access() & uint32(SNDRV_CTL_ELEM_ACCESS_READ)) != 0 {
		accessFlags = append(accessFlags, "Read")
	}
	if (ctl.Access() & uint32(SNDRV_CTL_ELEM_ACCESS_WRITE)) != 0 {
		accessFlags = append(accessFlags, "Write")
	}
	if ctl.IsAccessTLVRw() {
		accessFlags = append(accessFlags, "TLV")
	}
	kv("Access", "%s", strings.Join(accessFlags, ", "))

	switch ctl.Type() {
	case MIXER_CTL_TYPE_INT:
		minVal, errMin := ctl.RangeMin()
		maxVal, errMax := ctl.RangeMax()
		if errMin == nil && errMax == nil {
			kv("Range", "%d - %d", minVal, maxVal)
		}

		for i := uint(0); i < uint(ctl.NumValues()); i++ {
			val, errVal := ctl.Value(i)
			pct, errPct := ctl.Percent(i)
			if errVal == nil && errPct == nil {
				kv(fmt.Sprintf("Value[%d]", i), "%d (%d%%)", val, pct)
			} else {
				kv(fmt.Sprintf("Value[%d]", i), "<unreadable>")
			}
		}

	case MIXER_CTL_TYPE_BOOL:
		for i := uint(0); i < uint(ctl.NumValues()); i++ {
			val, err := ctl.Value(i)
			if err == nil {
				status := "Off"
				if val == 1 {
					status = "On"
				}
				kv(fmt.Sprintf("Value[%d]", i), "%d (%s)", val, status)
			} else {
				kv(fmt.Sprintf("Value[%d]", i), "<unreadable>")
			}
		}

	case MIXER_CTL_TYPE_ENUM:
		numEnums, err := ctl.NumEnums()
		if err == nil {
			kv("Enum Items", "%d", numEnums)
			allEnums, errEnums := ctl.AllEnumStrings()
			if errEnums == nil {
				for i, name := range allEnums {
					b.WriteString(fmt.Sprintf("    %d: '%s'\n", i, name))
				}
			}
		}

		for i := uint(0); i < uint(ctl.NumValues()); i++ {
			val, err := ctl.EnumValueString(i)
			if err == nil {
				kv(fmt.Sprintf("Value[%d]", i), "'%s'", val)
			} else {
				kv(fmt.Sprintf("Value[%d]", i), "<unreadable>")
			}
		}

	case MIXER_CTL_TYPE_INT64:
		minVal, errMin := ctl.RangeMin64()
		maxVal, errMax := ctl.RangeMax64()
		if errMin == nil && errMax == nil {
			kv("Range", "%d - %d", minVal, maxVal)
		}

		for i := uint(0); i < uint(ctl.NumValues()); i++ {
			val, err := ctl.Value64(i)
			if err == nil {
				kv(fmt.Sprintf("Value[%d]", i), "%d", val)
			} else {
				kv(fmt.Sprintf("Value[%d]", i), "<unreadable>")
			}
		}

	case MIXER_CTL_TYPE_BYTE:
		// Reading byte arrays might not always be useful to display,
		// but we can indicate that it's a byte control.
		b.WriteString(fmt.Sprintf("Note          : Use .Array() to read byte data.\n"))
	}

	return strings.TrimSpace(b.String())
}

// cString converts a C-style null-terminated byte array to a Go string.
func cString(b []byte) string {
	i := bytes.IndexByte(b, 0)
	if i == -1 {
		return string(b)
	}

	return string(b[:i])
}
