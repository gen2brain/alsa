package alsa

import "testing"

func mkCtl(name string, index uint32) *MixerCtl {
	c := &MixerCtl{}
	copy(c.info.Id.Name[:], name)
	c.info.Id.Index = index

	return c
}

func TestBaseName(t *testing.T) {
	cases := map[string]string{
		"Master Playback Volume":    "Master",
		"Headphone Playback Volume": "Headphone",
		"PCM Playback Switch":       "PCM",
		"Capture Volume":            "Capture",
		"Capture Switch":            "Capture",
		"Mic Boost Volume":          "Mic Boost",
		"Input Source":              "Input Source",
		"3D Control - Switch":       "3D Control - Switch", // '-' guard keeps suffix
		"Beep Playback Volume":      "Beep",
	}

	for name, want := range cases {
		if got := baseName(name); got != want {
			t.Errorf("baseName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestGetCompareWeight(t *testing.T) {
	// Single-word base names: weight is ordinal*1000 + 1 + idx.
	weights := map[string]int{
		"Master":    1,
		"Headphone": 1001,
		"Speaker":   2001,
		"PCM":       7001,
		"Capture":   31001,
		"Mix":       32001,
	}
	for name, want := range weights {
		if got := getCompareWeight(name, 0); got != want {
			t.Errorf("getCompareWeight(%q, 0) = %d, want %d", name, got, want)
		}
	}

	if got := getCompareWeight("Nonexistent", 0); got != mixerWeightNotFound {
		t.Errorf("unknown name weight = %d, want %d", got, mixerWeightNotFound)
	}

	// idx is added when the base name has no remainder.
	if a, b := getCompareWeight("Master", 0), getCompareWeight("Master", 1); b != a+1 {
		t.Errorf("index should add 1: got %d and %d", a, b)
	}

	// Priority order must be strictly increasing in table order.
	order := []string{"Master", "Headphone", "Speaker", "PCM", "Mic", "Capture", "Mix"}
	for i := 1; i < len(order); i++ {
		if getCompareWeight(order[i-1], 0) >= getCompareWeight(order[i], 0) {
			t.Errorf("%q should sort before %q", order[i-1], order[i])
		}
	}
}

func TestMixerSort(t *testing.T) {
	m := &Mixer{Ctls: []*MixerCtl{
		mkCtl("Capture Volume", 0),
		mkCtl("Some Random Control", 0),
		mkCtl("PCM Playback Volume", 0),
		mkCtl("Master Playback Volume", 0),
		mkCtl("Speaker Playback Volume", 0),
		mkCtl("Headphone Playback Volume", 0),
		mkCtl("Mic Playback Volume", 0),
	}}

	m.Sort()

	want := []string{
		"Master Playback Volume",
		"Headphone Playback Volume",
		"Speaker Playback Volume",
		"PCM Playback Volume",
		"Mic Playback Volume",
		"Capture Volume",
		"Some Random Control",
	}

	for i, w := range want {
		if got := m.Ctls[i].Name(); got != w {
			t.Errorf("Ctls[%d] = %q, want %q", i, got, w)
		}
	}
}

func TestMixerSortUnknownKeepsKernelOrder(t *testing.T) {
	m := &Mixer{Ctls: []*MixerCtl{
		mkCtl("Zeta Control", 0),
		mkCtl("Alpha Control", 0),
		mkCtl("Master Playback Volume", 0),
	}}

	m.Sort()

	want := []string{"Master Playback Volume", "Zeta Control", "Alpha Control"}
	for i, w := range want {
		if got := m.Ctls[i].Name(); got != w {
			t.Errorf("Ctls[%d] = %q, want %q", i, got, w)
		}
	}
}

func TestMixerSortNil(t *testing.T) {
	var m *Mixer
	m.Sort() // must not panic
}
