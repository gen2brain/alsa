//go:build linux

package alsa

import (
	"sort"
	"testing"
)

func TestCleanControlName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Master Playback Volume", "Master"},
		{"Master Playback Switch", "Master"},
		{"Headphone Playback Volume", "Headphone"},
		{"Capture Volume", "Capture"},
		{"Capture Switch", "Capture"},
		{"PCM Playback Volume", "PCM"},
		{"Mic Capture Volume", "Mic"},
		{"Input Source", "Input Source"},
		{"3D Control - Switch", "3D Control - Switch"},
	}

	for _, tt := range tests {
		got := cleanControlName(tt.input)
		if got != tt.expected {
			t.Errorf("cleanControlName(%q) = %q; want %q", tt.input, got, tt.expected)
		}
	}
}

func TestGetCompareWeight(t *testing.T) {
	// These expected values are based on the priority table:
	// Master: 0 (1000 base) -> 1
	// Headphone: 1 (1000 base) -> 1001
	// PCM: 7 (1000 base) -> 7001

	tests := []struct {
		name  string
		index uint
		want  int
	}{
		{"Master", 0, 1},
		{"Headphone", 0, 1001},
		{"Speaker", 0, 2001},
		{"PCM", 0, 7001},
		{"Mic", 0, 20001},
		{"Capture", 0, 31001},

		// Suffixes
		// "Mic Boost"
		// Primary: "Mic" (20001). Remaining " Boost".
		// " Boost" -> "Boost".
		// Suffix2 "Boost" is index 9. Coef 20.
		// res2 = 9 * 20 + 1 = 181.
		// Total = 20001 + 181 = 20182.
		{"Mic Boost", 0, 20182},

		// "Master Mono"
		// Primary: "Master" (1). Remaining " Mono".
		// Suffix2 "Mono" is index 0. Coef 20.
		// res2 = 0 * 20 + 1 = 1.
		// Total = 1 + 1 = 2.
		{"Master Mono", 0, 2},
	}

	for _, tt := range tests {
		got := getCompareWeight(tt.name, tt.index)
		if got != tt.want {
			t.Errorf("getCompareWeight(%q, %d) = %d; want %d", tt.name, tt.index, got, tt.want)
		}
	}
}

func TestSortingOrder(t *testing.T) {
	// This simulates the full sort behavior
	controls := []struct {
		name  string
		index int
	}{
		{"Beep Playback Volume", 0},
		{"Speaker Playback Volume", 0},
		{"Headphone Playback Volume", 0},
		{"Capture Volume", 0},
		{"Master Playback Volume", 0},
		{"Digital Playback Volume", 0}, // "Digital" not in primary table
	}

	type weightedCtl struct {
		name   string
		weight int
	}

	weighted := make([]weightedCtl, len(controls))
	for i, c := range controls {
		clean := cleanControlName(c.name)
		w := getCompareWeight(clean, uint(c.index))
		weighted[i] = weightedCtl{c.name, w}
	}

	sort.SliceStable(weighted, func(i, j int) bool {
		return weighted[i].weight < weighted[j].weight
	})

	expectedOrder := []string{
		"Master Playback Volume",
		"Headphone Playback Volume",
		"Speaker Playback Volume",
		"Beep Playback Volume",
		"Capture Volume",
		"Digital Playback Volume",
	}

	for i, w := range weighted {
		if w.name != expectedOrder[i] {
			t.Errorf("Index %d: got %s, want %s (weight: %d)", i, w.name, expectedOrder[i], w.weight)
		}
	}
}
