package alsa

import (
	"sort"
	"strings"
)

// Constants for sorting weights, derived from alsa-lib/src/mixer/simple_none.c
const (
	MixerCompareWeightSimpleBase = 0
	MixerCompareWeightNextBase   = 10000000
	MixerCompareWeightNotFound   = 1000000000
)

// priorityNames is the main table for control name priorities.
// Lower index means higher priority (lower weight).
var priorityNames = []string{
	"Master",
	"Headphone",
	"Speaker",
	"Tone",
	"Bass",
	"Treble",
	"3D Control",
	"PCM",
	"Front",
	"Surround",
	"Center",
	"LFE",
	"Side",
	"Synth",
	"FM",
	"Wave",
	"Music",
	"DSP",
	"Line",
	"CD",
	"Mic",
	"Video",
	"Zoom Video",
	"Phone",
	"I2S",
	"IEC958",
	"PC Speaker",
	"Beep",
	"Aux",
	"Mono",
	"Playback",
	"Capture",
	"Mix",
}

// suffixNames1 provides secondary weight adjustments for specific suffixes.
var suffixNames1 = []string{
	"-",
}

// suffixNames2 provides tertiary weight adjustments.
var suffixNames2 = []string{
	"Mono",
	"Digital",
	"Switch",
	"Depth",
	"Wide",
	"Space",
	"Level",
	"Center",
	"Output",
	"Boost",
	"Tone",
	"Bass",
	"Treble",
}

// compareMixerPriorityLookup looks up a name in a priority table.
// It returns the weight (index * coef) + 1 if found, or MixerCompareWeightNotFound.
// It also updates the name string slice to point past the matching part.
func compareMixerPriorityLookup(name string, names []string, coef int) (int, string) {
	for i, n := range names {
		// Check if the current name starts with the priority name
		if strings.HasPrefix(name, n) {
			// Advance the name past the match
			remaining := name[len(n):]
			// Skip leading spaces
			remaining = strings.TrimLeft(remaining, " ")
			return (i * coef) + 1, remaining
		}
	}
	return MixerCompareWeightNotFound, name
}

// getCompareWeight calculates the sorting weight for a control name and index.
// This is a direct port of get_compare_weight from alsa-lib/src/mixer/simple_none.c.
func getCompareWeight(name string, index uint) int {
	res := MixerCompareWeightNotFound

	// 1. Primary lookup
	var weight int
	weight, name = compareMixerPriorityLookup(name, priorityNames, 1000)
	if weight == MixerCompareWeightNotFound {
		return MixerCompareWeightNotFound
	}
	res = weight

	// If name is empty after lookup, we are done
	if name == "" {
		return MixerCompareWeightSimpleBase + res + int(index)
	}

	// 2. Secondary lookup (suffixes)
	// The C code logic for suffix handling is a bit specific.
	// It tries to find a secondary suffix (names1) in the part of the name *before* the final suffix.
	// Or it uses the final suffix as a tertiary lookup (names2).

	// We simplify the C pointer arithmetic to Go string manipulation:
	// We look for " - " or just space separation logic.

	// Example: "3D Control - Switch"
	// Primary lookup matches "3D Control". Remaining: "- Switch"

	// Check if remaining starts with one of suffixNames1
	weight1, name1 := compareMixerPriorityLookup(name, suffixNames1, 200)
	if weight1 != MixerCompareWeightNotFound {
		res += weight1
		name = name1
	}

	// Check if remaining starts with one of suffixNames2
	weight2, _ := compareMixerPriorityLookup(name, suffixNames2, 20)
	if weight2 != MixerCompareWeightNotFound {
		res += weight2
	}

	return MixerCompareWeightSimpleBase + res + int(index)
}

// cleanControlName removes standard suffixes like " Playback Volume" for sorting purposes.
// This mimics alsa-lib/src/mixer/simple_none.c base_len function logic.
func cleanControlName(name string) string {
	// Exception: "Capture Volume" and "Capture Switch"
	if name == "Capture Volume" || name == "Capture Switch" {
		return "Capture"
	}

	suffixes := []string{
		" Playback Enum",
		" Playback Switch",
		" Playback Route",
		" Playback Volume",
		" Capture Enum",
		" Capture Switch",
		" Capture Route",
		" Capture Volume",
		" Enum",
		" Switch",
		" Route",
		" Volume",
	}

	for _, suffix := range suffixes {
		if strings.HasSuffix(name, suffix) {
			baseLen := len(name) - len(suffix)
			// Special check for "3D Control - Switch" to avoid stripping "Switch" if it's part of the name logic
			if baseLen > 0 && name[baseLen-1] == '-' {
				continue
			}
			return name[:baseLen]
		}
	}

	// Special case: "Input Source" -> "Input Source"
	if name == "Input Source" {
		return name
	}

	if strings.Contains(name, "3D Control") {
		if strings.Contains(name, "Depth") {
			return name
		}
	}

	return name
}

// sortControls sorts the mixer controls in place according to ALSA priority.
func (m *Mixer) sortControls() {
	sort.SliceStable(m.Ctls, func(i, j int) bool {
		c1 := m.Ctls[i]
		c2 := m.Ctls[j]

		// 1. Get cleaned names (base names)
		n1 := cleanControlName(c1.Name())
		n2 := cleanControlName(c2.Name())

		// 2. Calculate weights
		w1 := getCompareWeight(n1, uint(c1.Index()))
		w2 := getCompareWeight(n2, uint(c2.Index()))

		// If weight is not found (control not in table), give it a very high weight
		if w1 == MixerCompareWeightNotFound {
			w1 = MixerCompareWeightNotFound + int(c1.ID()) // Stable fallback
		}
		if w2 == MixerCompareWeightNotFound {
			w2 = MixerCompareWeightNotFound + int(c2.ID()) // Stable fallback
		}

		// 3. Compare weights
		if w1 != w2 {
			return w1 < w2
		}

		// 4. Fallback: Compare names alphabetically
		if n1 != n2 {
			return n1 < n2
		}

		// 5. Fallback: Compare indices
		return c1.Index() < c2.Index()
	})
}
