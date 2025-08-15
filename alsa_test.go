package alsa_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

var (
	// loopbackCard stores the dynamically found card number for the loopback device.
	loopbackCard = -1

	// dummyCard stores the dynamically found card number for the dummy device.
	dummyCard = -1
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

	os.Exit(m.Run())
}
