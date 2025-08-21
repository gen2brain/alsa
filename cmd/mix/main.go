package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/gen2brain/alsa"
)

func main() {
	var (
		card     uint
		device   uint
		list     bool
		noUpdate bool
	)

	flag.UintVar(&card, "card", 0, "The card number to use.")
	flag.UintVar(&device, "device", 0, "The device number to use.")
	flag.BoolVar(&list, "list", false, "List all controls.")
	flag.BoolVar(&noUpdate, "no-update", false, "Don't update the mixer controls before displaying them.")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] [control] [value...]\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "\nOptions:")
		for _, name := range []string{"card", "device", "list", "no-update"} {
			f := flag.Lookup(name)
			if f != nil {
				fmt.Fprintf(os.Stderr, "  --%s\n    \t%v (default %q)\n", f.Name, f.Usage, f.DefValue)
			}
		}
		fmt.Fprintln(os.Stderr, "\nTo set a control, provide the control name or ID and the desired value(s).")
		fmt.Fprintln(os.Stderr, "If no control is specified, all controls and their values are listed.")
	}

	flag.Parse()

	mixer, err := alsa.MixerOpen(card)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening mixer for card %d: %v\n", card, err)
		os.Exit(1)
	}
	defer mixer.Close()

	if !noUpdate {
		if err := mixer.AddNewCtls(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Failed to add new controls: %v\n", err)
		}
	}

	args := flag.Args()

	if list {
		printAllControls(mixer, true)

		return
	}

	if len(args) == 0 {
		printAllControls(mixer, false)

		return
	}

	// The first argument can be a control name or ID
	controlIdentifier := args[0]
	values := args[1:]

	var ctl *alsa.MixerCtl

	// Try parsing as a numeric ID first
	if id, err := strconv.ParseUint(controlIdentifier, 10, 32); err == nil {
		ctl, err = mixer.Ctl(uint32(id))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: Cannot find control with ID %d: %v\n", id, err)
			os.Exit(1)
		}
	} else {
		// If not an ID, treat it as a name
		ctl, err = mixer.CtlByName(controlIdentifier)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: Cannot find control with name '%s': %v\n", controlIdentifier, err)
			os.Exit(1)
		}
	}

	if len(values) == 0 {
		// If no values are provided, just print the info for the specified control
		printControl(ctl, false)
	} else {
		// If values are provided, set the control
		if err := setControlValue(ctl, values); err != nil {
			fmt.Fprintf(os.Stderr, "Error setting value for control '%s': %v\n", ctl.Name(), err)
			os.Exit(1)
		}

		fmt.Printf("Set control '%s' successfully.\n", ctl.Name())
	}
}

// printAllControls lists all available mixer controls and optionally their values.
func printAllControls(mixer *alsa.Mixer, listOnly bool) {
	numCtls := mixer.NumCtls()

	fmt.Printf("Mixer card '%s' has %d controls.\n", mixer.Name(), numCtls)
	fmt.Println("---------------------------------------")

	for i := 0; i < numCtls; i++ {
		ctl, err := mixer.CtlByIndex(uint(i))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Could not get control at index %d: %v\n", i, err)

			continue
		}

		printControl(ctl, listOnly)
	}
}

// printControl prints detailed information about a single mixer control.
func printControl(ctl *alsa.MixerCtl, listOnly bool) {
	if listOnly {
		fmt.Printf("%d: %s\n", ctl.ID(), ctl.Name())

		return
	}

	fmt.Printf("%d: %s (%s, %d values)\n", ctl.ID(), ctl.Name(), ctl.TypeString(), ctl.NumValues())

	switch ctl.Type() {
	case alsa.SNDRV_CTL_ELEM_TYPE_INTEGER:
		printIntegerControl(ctl)
	case alsa.SNDRV_CTL_ELEM_TYPE_BOOLEAN:
		printBooleanControl(ctl)
	case alsa.SNDRV_CTL_ELEM_TYPE_ENUMERATED:
		printEnumControl(ctl)
	case alsa.SNDRV_CTL_ELEM_TYPE_BYTES:
		printByteControl(ctl)
	case alsa.SNDRV_CTL_ELEM_TYPE_INTEGER64:
		printInt64Control(ctl)
	default:
		fmt.Println("  Value: <unsupported type>")
	}

	fmt.Println()
}

// printIntegerControl prints details for an integer control.
func printIntegerControl(ctl *alsa.MixerCtl) {
	minVal, errMin := ctl.RangeMin()
	maxVal, errMax := ctl.RangeMax()
	if errMin == nil && errMax == nil {
		fmt.Printf("  Range: %d - %d\n", minVal, maxVal)
	}

	var values []string

	for i := uint(0); i < uint(ctl.NumValues()); i++ {
		val, err := ctl.Value(i)
		if err != nil {
			values = append(values, "<error>")

			continue
		}
		pct, err := ctl.Percent(i)
		if err == nil {
			values = append(values, fmt.Sprintf("%d (%d%%)", val, pct))
		} else {
			values = append(values, fmt.Sprintf("%d", val))
		}
	}

	fmt.Printf("  Value: %s\n", strings.Join(values, ", "))
}

// printBooleanControl prints details for a boolean control.
func printBooleanControl(ctl *alsa.MixerCtl) {
	var values []string

	for i := uint(0); i < uint(ctl.NumValues()); i++ {
		val, err := ctl.Value(i)
		if err != nil {
			values = append(values, "<error>")
		} else if val > 0 {
			values = append(values, "On")
		} else {
			values = append(values, "Off")
		}
	}

	fmt.Printf("  Value: %s\n", strings.Join(values, ", "))
}

// printEnumControl prints details for an enumerated control.
func printEnumControl(ctl *alsa.MixerCtl) {
	allEnums, err := ctl.AllEnumStrings()
	if err == nil {
		fmt.Printf("  Enums: %s\n", strings.Join(allEnums, ", "))
	}

	var values []string
	for i := uint(0); i < uint(ctl.NumValues()); i++ {
		valStr, err := ctl.EnumValueString(i)
		if err != nil {
			values = append(values, "<error>")
		} else {
			values = append(values, valStr)
		}
	}

	fmt.Printf("  Value: %s\n", strings.Join(values, ", "))
}

// printByteControl prints details for a byte control (often TLV data).
func printByteControl(ctl *alsa.MixerCtl) {
	var data []byte

	err := ctl.Array(&data)
	if err != nil {
		fmt.Printf("  Value: <error reading bytes: %v>\n", err)

		return
	}

	// For brevity, only show the first few bytes if it's long
	limit := 16
	if len(data) > limit {
		fmt.Printf("  Value (first %d bytes): %v...\n", limit, data[:limit])
	} else {
		fmt.Printf("  Value: %v\n", data)
	}
}

// printInt64Control prints details for a 64-bit integer control.
func printInt64Control(ctl *alsa.MixerCtl) {
	minVal, errMin := ctl.RangeMin64()
	maxVal, errMax := ctl.RangeMax64()
	if errMin == nil && errMax == nil {
		fmt.Printf("  Range: %d - %d\n", minVal, maxVal)
	}

	var values []string

	for i := uint(0); i < uint(ctl.NumValues()); i++ {
		val, err := ctl.Value64(i)
		if err != nil {
			values = append(values, "<error>")
		} else {
			values = append(values, fmt.Sprintf("%d", val))
		}
	}

	fmt.Printf("  Value: %s\n", strings.Join(values, ", "))
}

// setControlValue parses string arguments and sets the control's value.
func setControlValue(ctl *alsa.MixerCtl, values []string) error {
	numValuesToSet := len(values)
	if numValuesToSet == 0 {
		return fmt.Errorf("no value provided")
	}

	// If only one value is provided, apply it to all channels of the control.
	if numValuesToSet == 1 {
		singleValue := values[0]
		for i := uint(0); i < uint(ctl.NumValues()); i++ {
			if err := setSingleValue(ctl, i, singleValue); err != nil {
				return err
			}
		}

		return nil
	}

	// If multiple values are provided, they must match the number of control values.
	if uint32(numValuesToSet) != ctl.NumValues() {
		return fmt.Errorf("provided %d values, but control has %d values", numValuesToSet, ctl.NumValues())
	}

	for i, v := range values {
		if err := setSingleValue(ctl, uint(i), v); err != nil {
			return err
		}
	}

	return nil
}

// setSingleValue sets a single value on a control at a specific index.
func setSingleValue(ctl *alsa.MixerCtl, index uint, valueStr string) error {
	switch ctl.Type() {
	case alsa.SNDRV_CTL_ELEM_TYPE_INTEGER:
		// Handle percentage suffix
		if strings.HasSuffix(valueStr, "%") {
			pctStr := strings.TrimSuffix(valueStr, "%")
			pct, err := strconv.Atoi(pctStr)
			if err != nil {
				return fmt.Errorf("invalid percentage value '%s'", valueStr)
			}

			return ctl.SetPercent(index, pct)
		}

		val, err := strconv.Atoi(valueStr)
		if err != nil {
			return fmt.Errorf("invalid integer value '%s'", valueStr)
		}

		return ctl.SetValue(index, val)

	case alsa.SNDRV_CTL_ELEM_TYPE_BOOLEAN:
		val, err := parseBool(valueStr)
		if err != nil {
			return err
		}

		return ctl.SetValue(index, val)

	case alsa.SNDRV_CTL_ELEM_TYPE_ENUMERATED:
		return ctl.SetEnumByString(valueStr)

	case alsa.SNDRV_CTL_ELEM_TYPE_INTEGER64:
		val, err := strconv.ParseInt(valueStr, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid int64 value '%s'", valueStr)
		}

		return ctl.SetValue64(index, val)

	case alsa.SNDRV_CTL_ELEM_TYPE_BYTES:
		// Setting byte arrays via command line is complex and not implemented here.
		return fmt.Errorf("setting BYTE controls via command line is not supported")

	default:
		return fmt.Errorf("cannot set value for unsupported control type %s", ctl.TypeString())
	}
}

// parseBool is a helper to interpret various string representations of a boolean.
func parseBool(s string) (int, error) {
	s = strings.ToLower(s)
	switch s {
	case "1", "on", "true", "yes":
		return 1, nil
	case "0", "off", "false", "no":
		return 0, nil
	}

	return 0, fmt.Errorf("invalid boolean value '%s'", s)
}
