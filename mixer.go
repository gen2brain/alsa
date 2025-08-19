package alsa

import (
	"bytes"
	"fmt"
	"os"
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

// Ctl returns a mixer control by its numeric ID.
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

	// In ALSA, the `Typ` field identifies the event category. For control element
	// changes (the ones we care about), this type is SNDRV_CTL_EVENT_ELEM.
	if ev.Typ != SNDRV_CTL_EVENT_ELEM {
		return nil, fmt.Errorf("received non-element event type: %d", ev.Typ)
	}

	event := &MixerEvent{Type: MixerEventType(ev.Elem.Mask)}

	// The mask indicates what kind of event it is. For VALUE, INFO, ADD, and REMOVE, the event data contains the element ID.
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

// cString converts a C-style null-terminated byte array to a Go string.
func cString(b []byte) string {
	i := bytes.IndexByte(b, 0)
	if i == -1 {
		return string(b)
	}

	return string(b[:i])
}
