package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/gen2brain/alsa"
)

func main() {
	var (
		card   int
		device int
		stream string
	)

	flag.IntVar(&card, "card", -1, "The sound card number (-1 for all).")
	flag.IntVar(&device, "device", -1, "The device number (-1 for all).")
	flag.StringVar(&stream, "stream", "", "Stream direction: 'playback', 'capture', or empty for both.")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options]\n\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "Displays information and capabilities for ALSA PCM devices.")
		fmt.Fprintln(os.Stderr, "\nExamples:")
		fmt.Fprintln(os.Stderr, "  pcminfo                      # Show all capabilities for all devices on all cards")
		fmt.Fprintln(os.Stderr, "  pcminfo --card=0             # Show all for card 0")
		fmt.Fprintln(os.Stderr, "  pcminfo --card=0 --device=2  # Show all for card 0, device 2")
		fmt.Fprintln(os.Stderr, "  pcminfo --stream=playback    # Show only playback capabilities for all devices")
		fmt.Fprintln(os.Stderr, "\nOptions:")
		flag.PrintDefaults()
	}

	flag.Parse()

	cards, err := alsa.EnumerateCards()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error enumerating sound cards: %v\n", err)
		os.Exit(1)
	}

	if len(cards) == 0 {
		fmt.Println("No sound cards found.")

		return
	}

	for _, c := range cards {
		// Filter by card number if specified
		if card != -1 && c.ID != card {
			continue
		}

		for _, dev := range c.Devices {
			// Filter by device number if specified
			if device != -1 && dev.ID != device {
				continue
			}

			// Determine which streams to check based on the flag
			checkPlayback := strings.ToLower(stream) == "playback" || stream == ""
			checkCapture := strings.ToLower(stream) == "capture" || stream == ""

			var pcmFlags alsa.PcmFlag
			var streamName string

			if dev.IsPlayback && checkPlayback {
				pcmFlags = alsa.PCM_OUT
				streamName = "Playback"
			} else if !dev.IsPlayback && checkCapture {
				pcmFlags = alsa.PCM_IN
				streamName = "Capture"
			} else {
				continue
			}

			fmt.Printf("--> Card %d (%s), Device %d (%s), Stream: %s\n", c.ID, c.Description, dev.ID, dev.Description, streamName)

			// Get the hardware parameters for the specified PCM device.
			params, err := alsa.PcmParamsGet(uint(c.ID), uint(dev.ID), pcmFlags)
			if err != nil {
				fmt.Fprintf(os.Stderr, "    Error getting PCM parameters: %v\n\n", err)

				continue
			}

			// The PcmParams object has a String() method that conveniently formats
			// all the capabilities into a human-readable string.
			fmt.Print(params.String())
			fmt.Println()
		}
	}
}
