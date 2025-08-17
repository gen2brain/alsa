package alsa

import (
	"syscall"
	"unsafe"
)

// ioctl performs a generic ioctl syscall.
func ioctl(fd uintptr, req uintptr, arg uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg)
	if errno != 0 {
		return errno
	}

	return nil
}

// io builds an ioctl request code for a command with no data transfer.
func io(typ, nr uintptr) uintptr {
	const (
		_IOC_NRBITS    = 8
		_IOC_TYPEBITS  = 8
		_IOC_SIZEBITS  = 14
		_IOC_DIRBITS   = 2
		_IOC_NRSHIFT   = 0
		_IOC_TYPESHIFT = _IOC_NRSHIFT + _IOC_NRBITS
		_IOC_SIZESHIFT = _IOC_TYPESHIFT + _IOC_TYPEBITS
		_IOC_DIRSHIFT  = _IOC_SIZESHIFT + _IOC_SIZEBITS
		_IOC_NONE      = 0
	)

	return ((_IOC_NONE) << _IOC_DIRSHIFT) | (typ << _IOC_TYPESHIFT) | (nr << _IOC_NRSHIFT) | (0 << _IOC_SIZESHIFT)
}

// iow builds an ioctl request code for a write-only operation.
func iow(typ, nr, size uintptr) uintptr {
	const (
		_IOC_NRBITS    = 8
		_IOC_TYPEBITS  = 8
		_IOC_SIZEBITS  = 14
		_IOC_DIRBITS   = 2
		_IOC_NRSHIFT   = 0
		_IOC_TYPESHIFT = _IOC_NRSHIFT + _IOC_NRBITS
		_IOC_SIZESHIFT = _IOC_TYPESHIFT + _IOC_TYPEBITS
		_IOC_DIRSHIFT  = _IOC_SIZESHIFT + _IOC_SIZEBITS
		_IOC_WRITE     = 1
	)

	return ((_IOC_WRITE) << _IOC_DIRSHIFT) | (typ << _IOC_TYPESHIFT) | (nr << _IOC_NRSHIFT) | (size << _IOC_SIZESHIFT)
}

// ior builds a read-only ioctl request code.
func ior(typ, nr, size uintptr) uintptr {
	const (
		_IOC_NRBITS    = 8
		_IOC_TYPEBITS  = 8
		_IOC_SIZEBITS  = 14
		_IOC_DIRBITS   = 2
		_IOC_NRSHIFT   = 0
		_IOC_TYPESHIFT = _IOC_NRSHIFT + _IOC_NRBITS
		_IOC_SIZESHIFT = _IOC_TYPESHIFT + _IOC_TYPEBITS
		_IOC_DIRSHIFT  = _IOC_SIZESHIFT + _IOC_SIZEBITS
		_IOC_READ      = 2
	)

	return ((_IOC_READ) << _IOC_DIRSHIFT) | (typ << _IOC_TYPESHIFT) | (nr << _IOC_NRSHIFT) | (size << _IOC_SIZESHIFT)
}

// iowr builds a read-write ioctl request code.
func iowr(typ, nr, size uintptr) uintptr {
	const (
		_IOC_NRBITS    = 8
		_IOC_TYPEBITS  = 8
		_IOC_SIZEBITS  = 14
		_IOC_DIRBITS   = 2
		_IOC_NRSHIFT   = 0
		_IOC_TYPESHIFT = _IOC_NRSHIFT + _IOC_NRBITS
		_IOC_SIZESHIFT = _IOC_TYPESHIFT + _IOC_TYPEBITS
		_IOC_DIRSHIFT  = _IOC_SIZESHIFT + _IOC_SIZEBITS
		_IOC_READ      = 2
		_IOC_WRITE     = 1
	)

	return ((_IOC_READ | _IOC_WRITE) << _IOC_DIRSHIFT) | (typ << _IOC_TYPESHIFT) | (nr << _IOC_NRSHIFT) | (size << _IOC_SIZESHIFT)
}

var (
	// PCM IOCTLs
	SNDRV_PCM_IOCTL_HW_REFINE     uintptr
	SNDRV_PCM_IOCTL_HW_PARAMS     uintptr
	SNDRV_PCM_IOCTL_HW_FREE       uintptr
	SNDRV_PCM_IOCTL_SW_PARAMS     uintptr
	SNDRV_PCM_IOCTL_INFO          uintptr
	SNDRV_PCM_IOCTL_PAUSE         uintptr
	SNDRV_PCM_IOCTL_RESUME        uintptr
	SNDRV_PCM_IOCTL_PREPARE       uintptr
	SNDRV_PCM_IOCTL_START         uintptr
	SNDRV_PCM_IOCTL_DROP          uintptr
	SNDRV_PCM_IOCTL_DRAIN         uintptr
	SNDRV_PCM_IOCTL_DELAY         uintptr
	SNDRV_PCM_IOCTL_LINK          uintptr
	SNDRV_PCM_IOCTL_UNLINK        uintptr
	SNDRV_PCM_IOCTL_HWSYNC        uintptr
	SNDRV_PCM_IOCTL_SYNC_PTR      uintptr
	SNDRV_PCM_IOCTL_TTSTAMP       uintptr
	SNDRV_PCM_IOCTL_WRITEI_FRAMES uintptr
	SNDRV_PCM_IOCTL_READI_FRAMES  uintptr
	SNDRV_PCM_IOCTL_WRITEN_FRAMES uintptr
	SNDRV_PCM_IOCTL_READN_FRAMES  uintptr
	SNDRV_PCM_IOCTL_STATUS        uintptr

	// Control IOCTLs
	SNDRV_CTL_IOCTL_CARD_INFO        uintptr
	SNDRV_CTL_IOCTL_ELEM_LIST        uintptr
	SNDRV_CTL_IOCTL_ELEM_INFO        uintptr
	SNDRV_CTL_IOCTL_ELEM_READ        uintptr
	SNDRV_CTL_IOCTL_ELEM_WRITE       uintptr
	SNDRV_CTL_IOCTL_SUBSCRIBE_EVENTS uintptr
	SNDRV_CTL_IOCTL_TLV_READ         uintptr
	SNDRV_CTL_IOCTL_TLV_WRITE        uintptr
)

func init() {
	// PCM IOCTLs ('A' for ALSA)
	SNDRV_PCM_IOCTL_HW_REFINE = iowr('A', 0x10, unsafe.Sizeof(sndPcmHwParams{}))
	SNDRV_PCM_IOCTL_HW_PARAMS = iowr('A', 0x11, unsafe.Sizeof(sndPcmHwParams{}))
	SNDRV_PCM_IOCTL_HW_FREE = io('A', 0x12)
	SNDRV_PCM_IOCTL_SW_PARAMS = iowr('A', 0x13, unsafe.Sizeof(sndPcmSwParams{}))
	SNDRV_PCM_IOCTL_INFO = ior('A', 0x01, unsafe.Sizeof(sndPcmInfo{}))

	// State change IOCTLs
	SNDRV_PCM_IOCTL_PREPARE = io('A', 0x40)
	SNDRV_PCM_IOCTL_START = io('A', 0x42)
	SNDRV_PCM_IOCTL_DROP = io('A', 0x43)
	SNDRV_PCM_IOCTL_DRAIN = io('A', 0x44)
	SNDRV_PCM_IOCTL_PAUSE = iow('A', 0x45, unsafe.Sizeof(int32(0)))
	SNDRV_PCM_IOCTL_RESUME = io('A', 0x47)

	// Synchronization IOCTLs
	SNDRV_PCM_IOCTL_HWSYNC = io('A', 0x22)
	SNDRV_PCM_IOCTL_DELAY = ior('A', 0x21, unsafe.Sizeof(SndPcmSframesT(0)))
	SNDRV_PCM_IOCTL_TTSTAMP = iow('A', 0x03, unsafe.Sizeof(int32(0)))
	SNDRV_PCM_IOCTL_SYNC_PTR = iowr('A', 0x23, unsafe.Sizeof(sndPcmSyncPtr{}))

	// Linking IOCTLs
	SNDRV_PCM_IOCTL_LINK = iow('A', 0x60, unsafe.Sizeof(int32(0)))
	SNDRV_PCM_IOCTL_UNLINK = io('A', 0x61)

	// Frame transfer IOCTLs
	SNDRV_PCM_IOCTL_WRITEI_FRAMES = iow('A', 0x50, unsafe.Sizeof(sndXferi{}))
	SNDRV_PCM_IOCTL_READI_FRAMES = ior('A', 0x51, unsafe.Sizeof(sndXferi{}))
	SNDRV_PCM_IOCTL_WRITEN_FRAMES = iow('A', 0x52, unsafe.Sizeof(sndXfern{}))
	SNDRV_PCM_IOCTL_READN_FRAMES = ior('A', 0x53, unsafe.Sizeof(sndXfern{}))
	SNDRV_PCM_IOCTL_STATUS = ior('A', 0x20, unsafe.Sizeof(sndPcmStatus{}))

	// Control IOCTLs ('U' for UAC)
	SNDRV_CTL_IOCTL_CARD_INFO = ior('U', 0x01, unsafe.Sizeof(sndCtlCardInfo{}))
	SNDRV_CTL_IOCTL_ELEM_LIST = iowr('U', 0x10, unsafe.Sizeof(sndCtlElemList{}))
	SNDRV_CTL_IOCTL_ELEM_INFO = iowr('U', 0x11, unsafe.Sizeof(sndCtlElemInfo{}))
	SNDRV_CTL_IOCTL_ELEM_READ = iowr('U', 0x12, unsafe.Sizeof(sndCtlElemValue{}))
	SNDRV_CTL_IOCTL_ELEM_WRITE = iowr('U', 0x13, unsafe.Sizeof(sndCtlElemValue{}))
	SNDRV_CTL_IOCTL_SUBSCRIBE_EVENTS = iowr('U', 0x16, unsafe.Sizeof(int32(0)))
	SNDRV_CTL_IOCTL_TLV_READ = iowr('U', 0x1a, unsafe.Sizeof(sndCtlTlv{}))
	SNDRV_CTL_IOCTL_TLV_WRITE = iowr('U', 0x1b, unsafe.Sizeof(sndCtlTlv{}))
}
