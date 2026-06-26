package alsa_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/gen2brain/alsa"
)

// Card and device numbers are resolved at runtime in TestMain; they are not
// fixed across machines.
var (
	loopbackCard           = -1
	dummyCard              = -1
	loopbackPlaybackDevice = -1
	loopbackCaptureDevice  = -1
)

// findCard searches /proc/asound/cards for the passed device name and returns its card number. Returns -1 if not found.
func findCard(name string) int {
	content, err := os.ReadFile("/proc/asound/cards")
	if err != nil {
		return -1
	}

	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		if strings.Contains(line, name) {
			var card int
			// Lines look like " 0 [Loopback       ]: Loopback - Loopback".
			_, err := fmt.Sscanf(line, " %d", &card)
			if err == nil {
				return card
			}
		}
	}

	return -1
}

// TestMain skips the whole suite (exit 0) unless both virtual cards are present.
func TestMain(m *testing.M) {
	loopbackCard = findCard("Loopback")
	if loopbackCard == -1 {
		fmt.Println("snd-aloop not loaded (sudo modprobe snd-aloop); skipping tests.")
		os.Exit(0)
	}

	dummyCard = findCard("Dummy")
	if dummyCard == -1 {
		fmt.Println("snd-dummy not loaded (sudo modprobe snd-dummy); skipping tests.")
		os.Exit(0)
	}

	loopbackPlaybackDevice = 0
	loopbackCaptureDevice = 1

	os.Exit(m.Run())
}

func TestEnumerateCards(t *testing.T) {
	cards, err := alsa.EnumerateCards()
	require.NoError(t, err, "EnumerateCards should not return an error")
	require.NotEmpty(t, cards, "EnumerateCards should find at least one card")

	foundLoopback := false
	for _, card := range cards {
		if card.ID == loopbackCard {
			foundLoopback = true
			assert.Equal(t, "Loopback", card.Name, "Loopback card name should be correct")
			assert.True(t, len(card.Devices) >= 2, "Loopback card should have at least playback and capture devices")

			foundPlayback := false
			foundCapture := false

			for _, device := range card.Devices {
				if device.ID == loopbackPlaybackDevice && device.IsPlayback {
					foundPlayback = true
				}
				if device.ID == loopbackCaptureDevice && !device.IsPlayback {
					foundCapture = true
				}
			}

			assert.True(t, foundPlayback, "Should find loopback playback device")
			assert.True(t, foundCapture, "Should find loopback capture device")

			break
		}
	}

	assert.True(t, foundLoopback, "Should find the loopback card")

	foundDummy := false
	for _, card := range cards {
		if card.ID == dummyCard {
			foundDummy = true
			assert.Equal(t, "Dummy", card.Name, "Dummy card name should be correct")

			break
		}
	}

	assert.True(t, foundDummy, "Should find the dummy card")
}
