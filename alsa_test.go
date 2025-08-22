package alsa_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gen2brain/alsa"
)

var (
	// loopbackCard stores the dynamically found card number for the loopback device.
	loopbackCard = -1

	// dummyCard stores the dynamically found card number for the dummy device.
	dummyCard = -1

	// loopbackPlaybackDevice is the playback device on the loopback card.
	loopbackPlaybackDevice = -1

	// loopbackCaptureDevice is the capture device on the loopback card.
	loopbackCaptureDevice = -1
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
			// The format is " 0 [Loopback       ]: Loopback - Loopback"
			_, err := fmt.Sscanf(line, " %d", &card)
			if err == nil {
				return card
			}
		}
	}

	return -1
}

// TestMain checks for the loopback device before running tests.
func TestMain(m *testing.M) {
	loopbackCard = findCard("Loopback")
	if loopbackCard == -1 {
		fmt.Println("ALSA loopback device not found. Skipping tests.")
		fmt.Println("Please run: sudo modprobe snd-aloop")
		os.Exit(0) // Exit successfully, skipping tests.
	}

	dummyCard = findCard("Dummy")
	if dummyCard == -1 {
		fmt.Println("ALSA dummy device not found. Skipping tests.")
		fmt.Println("Please run: sudo modprobe snd-dummy")
		os.Exit(0) // Exit successfully, skipping tests.
	}

	loopbackPlaybackDevice = 0
	loopbackCaptureDevice = 1

	os.Exit(m.Run())
}

func TestEnumerateCards(t *testing.T) {
	cards, err := alsa.EnumerateCards()
	require.NoError(t, err, "EnumerateCards should not return an error")
	require.NotEmpty(t, cards, "EnumerateCards should find at least one card")

	for _, card := range cards {
		fmt.Print(card.String())
	}

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
