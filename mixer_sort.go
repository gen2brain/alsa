package alsa

import (
	"sort"
	"strings"
)

// Faithful port of libasound's control ordering (alsa-lib,
// src/mixer/simple_none.c: get_compare_weight, base_len,
// compare_mixer_priority_lookup), reproducing alsamixer's user-logical order.
// The reference's quirks are kept on purpose so the result matches it exactly.

const (
	mixerWeightSimpleBase = 0
	mixerWeightNotFound   = 1000000000
)

// priorityNames orders the base control names; earlier entries sort first.
var priorityNames = []string{
	"Master", "Headphone", "Speaker", "Tone", "Bass", "Treble", "3D Control",
	"PCM", "Front", "Surround", "Center", "LFE", "Side", "Synth", "FM",
	"Wave", "Music", "DSP", "Line", "CD", "Mic", "Video", "Zoom Video",
	"Phone", "I2S", "IEC958", "PC Speaker", "Beep", "Aux", "Mono",
	"Playback", "Capture", "Mix",
}

var suffixNames1 = []string{"-"}

var suffixNames2 = []string{
	"Mono", "Digital", "Switch", "Depth", "Wide", "Space", "Level",
	"Center", "Output", "Boost", "Tone", "Bass", "Treble",
}

// mixerSuffixes are stripped to find a base name, most specific first; the first
// suffix the name ends with wins.
var mixerSuffixes = []string{
	" Playback Enum", " Capture Enum", " Enum",
	" Playback Switch", " Playback Route", " Playback Volume",
	" Capture Switch", " Capture Route", " Capture Volume",
	" Switch", " Route", " Volume",
}

// compareMixerPriorityLookup returns ordinal*coef+1 and the remainder for the
// first table entry that prefixes name, or mixerWeightNotFound.
func compareMixerPriorityLookup(name string, names []string, coef int) (int, string) {
	for i, n := range names {
		if strings.HasPrefix(name, n) {
			rest := name[len(n):]
			if len(rest) > 0 && rest[0] == ' ' {
				rest = rest[1:]
			}

			return i*coef + 1, rest
		}
	}

	return mixerWeightNotFound, name
}

// getCompareWeight computes the sort weight for a base control name and index.
func getCompareWeight(name string, idx uint32) int {
	res, name := compareMixerPriorityLookup(name, priorityNames, 1000)
	if res == mixerWeightNotFound {
		return mixerWeightNotFound
	}

	if name == "" {
		return mixerWeightSimpleBase + res + int(idx)
	}

	// Isolate the trailing word, mirroring the reference pointer arithmetic.
	end := len(name)
	i := end - 1
	for i != 0 && name[i] != ' ' {
		i--
	}
	for i != 0 && name[i] == ' ' {
		i--
	}

	if i != 0 {
		for i != 0 && name[i] != ' ' {
			i--
		}
		name = name[i:]

		res1, rest := compareMixerPriorityLookup(name, suffixNames1, 200)
		if res1 == mixerWeightNotFound {
			return res
		}
		res += res1
		name = rest
	}

	// The reference discards this weight; a match only enables the idx term.
	if res2, _ := compareMixerPriorityLookup(name, suffixNames2, 20); res2 == mixerWeightNotFound {
		return res
	}

	return mixerWeightSimpleBase + res + int(idx)
}

// baseName strips a control name to its ordering base, mirroring alsa-lib's base_len.
func baseName(name string) string {
	if name == "Capture Volume" || name == "Capture Switch" {
		return "Capture"
	}

	for _, suf := range mixerSuffixes {
		if len(name) > len(suf) && strings.HasSuffix(name, suf) {
			l := len(name) - len(suf)
			if l < 1 || name[l-1] != '-' { // not "... - Switch"
				return name[:l]
			}
		}
	}

	if name == "Input Source" {
		return name
	}

	if strings.Contains(name, "3D Control") && strings.Contains(name, "Depth") {
		return name
	}

	return name
}

// Sort reorders the controls into libasound's user-logical order (as alsamixer
// presents them). Enumeration defaults to kernel order; Sort opts into the
// logical order. Names not in the priority table keep kernel order, after known
// ones.
func (m *Mixer) Sort() {
	if m == nil {
		return
	}

	sort.SliceStable(m.Ctls, func(i, j int) bool {
		return ctlWeight(m.Ctls[i]) < ctlWeight(m.Ctls[j])
	})
}

func ctlWeight(c *MixerCtl) int {
	return getCompareWeight(baseName(c.Name()), c.Index())
}
