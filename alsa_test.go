package alsa_test

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

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

// findDevices dynamically finds the first available playback and capture devices on the loopback card.
func findDevices() error {
	if loopbackCard == -1 {
		return errors.New("loopback card not found")
	}

	var playbackDev, captureDev = -1, -1

	// Find the first available playback device on the card.
	for i := 0; i < 8; i++ {
		params, err := alsa.PcmParamsGet(uint(loopbackCard), uint(i), alsa.PCM_OUT)
		if err == nil {
			params.Free()
			playbackDev = i

			break
		}
	}

	// Find the first available capture device on the card.
	for i := 0; i < 8; i++ {
		params, err := alsa.PcmParamsGet(uint(loopbackCard), uint(i), alsa.PCM_IN)
		if err == nil {
			params.Free()
			captureDev = i
		
			break
		}
	}

	if playbackDev == -1 || captureDev == -1 {
		return fmt.Errorf("could not find a suitable playback/capture device pair on card %d", loopbackCard)
	}

	loopbackPlaybackDevice = playbackDev
	loopbackCaptureDevice = captureDev

	return nil
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

	if err := findDevices(); err != nil {
		fmt.Println(err)
		os.Exit(0) // Exit successfully, skipping tests.
	}

	os.Exit(m.Run())
}
