package alsa

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"unsafe"
)

// PcmParams represents the hardware capabilities of a PCM device.
type PcmParams struct {
	params *sndPcmHwParams
}

// PcmParamsGet queries the hardware parameters for a given PCM device to get its default settings.
// This function initializes the parameters and then uses the SNDRV_PCM_IOCTL_HW_PARAMS ioctl.
// The kernel then fills the structure with the hardware's default or current settings.
func PcmParamsGet(card, device uint, flags PcmFlag) (*PcmParams, error) {
	var streamChar byte
	if (flags & PCM_IN) != 0 {
		streamChar = 'c'
	} else {
		streamChar = 'p'
	}

	path := fmt.Sprintf("/dev/snd/pcmC%dD%d%c", card, device, streamChar)

	// Use O_NONBLOCK on open to avoid getting stuck
	file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open PCM device %s for query: %w", path, err)
	}
	defer file.Close()

	hwParams := &sndPcmHwParams{}
	paramInit(hwParams)

	// HW_REFINE will fill the struct with default parameters.
	if err := ioctl(file.Fd(), SNDRV_PCM_IOCTL_HW_REFINE, uintptr(unsafe.Pointer(hwParams))); err != nil {
		return nil, fmt.Errorf("ioctl HW_REFINE failed: %w", err)
	}

	return &PcmParams{params: hwParams}, nil
}

// RangeMin returns the minimum value for an interval parameter.
func (pp *PcmParams) RangeMin(param PcmParam) (uint32, error) {
	if pp == nil || pp.params == nil {
		return 0, fmt.Errorf("params not initialized")
	}

	if param < SNDRV_PCM_HW_PARAM_SAMPLE_BITS || param > SNDRV_PCM_HW_PARAM_TICK_TIME {
		return 0, fmt.Errorf("parameter %v is not an interval type", param)
	}

	return pp.params.Intervals[param-SNDRV_PCM_HW_PARAM_SAMPLE_BITS].MinVal, nil
}

// RangeMax returns the maximum value for an interval parameter.
func (pp *PcmParams) RangeMax(param PcmParam) (uint32, error) {
	if pp == nil || pp.params == nil {
		return 0, fmt.Errorf("params not initialized")
	}

	if param < SNDRV_PCM_HW_PARAM_SAMPLE_BITS || param > SNDRV_PCM_HW_PARAM_TICK_TIME {
		return 0, fmt.Errorf("parameter %v is not an interval type", param)
	}

	return pp.params.Intervals[param-SNDRV_PCM_HW_PARAM_SAMPLE_BITS].MaxVal, nil
}

// Mask returns the bitmask for a mask-type parameter.
func (pp *PcmParams) Mask(param PcmParam) (*PcmParamMask, error) {
	if pp == nil || pp.params == nil {
		return nil, fmt.Errorf("params not initialized")
	}

	if param < SNDRV_PCM_HW_PARAM_ACCESS || param > SNDRV_PCM_HW_PARAM_SUBFORMAT {
		return nil, fmt.Errorf("parameter %v is not a mask type", param)
	}

	maskPtr := &pp.params.Masks[param-SNDRV_PCM_HW_PARAM_ACCESS]

	return (*PcmParamMask)(unsafe.Pointer(maskPtr)), nil
}

// FormatIsSupported checks if a given PCM format is supported.
func (pp *PcmParams) FormatIsSupported(format PcmFormat) bool {
	mask, err := pp.Mask(SNDRV_PCM_HW_PARAM_FORMAT)
	if err != nil {
		return false
	}

	return mask.Test(uint(format))
}

// String returns a human-readable representation of the PCM device's capabilities.
func (pp *PcmParams) String() string {
	if pp == nil || pp.params == nil {
		return "<nil>"
	}

	var b strings.Builder

	// Helper to print masks using a string slice for names
	printMaskSlice := func(name string, param PcmParam, names []string) {
		mask, err := pp.Mask(param)
		if err != nil {
			return
		}

		var supported []string
		for i, n := range names {
			if i < len(names) && len(n) > 0 && mask.Test(uint(i)) {
				supported = append(supported, n)
			}
		}

		if len(supported) > 0 {
			b.WriteString(fmt.Sprintf("%12s: %s\n", name, strings.Join(supported, ", ")))
		}
	}

	// Helper to print format masks using the map
	printFormatMask := func() {
		mask, err := pp.Mask(SNDRV_PCM_HW_PARAM_FORMAT)
		if err != nil {
			return
		}

		var supported []string

		// Sort keys for consistent output
		var keys []int
		for k := range PcmParamFormatNames {
			keys = append(keys, int(k))
		}

		sort.Ints(keys)

		for _, k := range keys {
			f := PcmFormat(k)
			if name, ok := PcmParamFormatNames[f]; ok && mask.Test(uint(f)) {
				supported = append(supported, name)
			}
		}

		if len(supported) > 0 {
			b.WriteString(fmt.Sprintf("%12s: %s\n", "Format", strings.Join(supported, ", ")))
		}
	}

	// Helper to print interval parameters
	printInterval := func(name string, param PcmParam, unit string) {
		rangeMin, errMin := pp.RangeMin(param)
		rangeMax, errMax := pp.RangeMax(param)

		if errMin != nil || errMax != nil {
			return
		}

		if rangeMax == 0 || rangeMax == ^uint32(0) { // Don't print meaningless ranges
			return
		}

		b.WriteString(fmt.Sprintf("%12s: min=%-6d max=%-6d %s\n", name, rangeMin, rangeMax, unit))
	}

	b.WriteString("PCM device capabilities:\n")
	printMaskSlice("Access", SNDRV_PCM_HW_PARAM_ACCESS, PcmParamAccessNames)
	printFormatMask()
	printMaskSlice("Subformat", SNDRV_PCM_HW_PARAM_SUBFORMAT, PcmParamSubformatNames)
	printInterval("Rate", SNDRV_PCM_HW_PARAM_RATE, "Hz")
	printInterval("Channels", SNDRV_PCM_HW_PARAM_CHANNELS, "")
	printInterval("Sample bits", SNDRV_PCM_HW_PARAM_SAMPLE_BITS, "")
	printInterval("Period size", SNDRV_PCM_HW_PARAM_PERIOD_SIZE, "frames")
	printInterval("Periods", SNDRV_PCM_HW_PARAM_PERIODS, "")

	return b.String()
}

// paramInit initializes a sndPcmHwParams struct to allow all possible values.
func paramInit(p *sndPcmHwParams) {
	for n := range p.Masks {
		for i := range p.Masks[n].Bits {
			p.Masks[n].Bits[i] = ^uint32(0)
		}
	}

	for n := range p.Intervals {
		p.Intervals[n].MaxVal = ^uint32(0)
	}

	p.Rmask = ^uint32(0)
	p.Info = ^uint32(0)
}

func paramSetMask(p *sndPcmHwParams, param PcmParam, bit uint32) {
	// The first 3 params are masks
	if param < SNDRV_PCM_HW_PARAM_ACCESS || param > SNDRV_PCM_HW_PARAM_SUBFORMAT {
		return
	}

	mask := &p.Masks[param-SNDRV_PCM_HW_PARAM_ACCESS]
	for i := range mask.Bits {
		mask.Bits[i] = 0
	}

	if bit >= 256 { // SNDRV_MASK_MAX
		return
	}

	mask.Bits[bit>>5] |= 1 << (bit & 31)
}

func paramSetInt(p *sndPcmHwParams, param PcmParam, val uint32) {
	if param < SNDRV_PCM_HW_PARAM_SAMPLE_BITS || param > SNDRV_PCM_HW_PARAM_TICK_TIME {
		return
	}

	// The interval array index is the parameter value minus the value of the first interval param.
	interval := &p.Intervals[param-SNDRV_PCM_HW_PARAM_SAMPLE_BITS]
	interval.MinVal = val
	interval.MaxVal = val
	interval.Flags = SNDRV_PCM_INTERVAL_INTEGER
}

func paramSetMin(p *sndPcmHwParams, param PcmParam, val uint32) {
	if param < SNDRV_PCM_HW_PARAM_SAMPLE_BITS || param > SNDRV_PCM_HW_PARAM_TICK_TIME {
		return
	}

	interval := &p.Intervals[param-SNDRV_PCM_HW_PARAM_SAMPLE_BITS]
	interval.MinVal = val
}

func paramGetInt(p *sndPcmHwParams, param PcmParam) uint32 {
	if param < SNDRV_PCM_HW_PARAM_SAMPLE_BITS || param > SNDRV_PCM_HW_PARAM_TICK_TIME {
		return 0
	}

	// The interval array index is the parameter value minus the value of the first interval param.
	interval := &p.Intervals[param-SNDRV_PCM_HW_PARAM_SAMPLE_BITS]

	// Read the MinVal of the interval.
	// The driver finalizes the configuration by narrowing the interval.
	return interval.MinVal
}
