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

	flag.IntVar(&card, "card", 0, "The sound card number.")
	flag.IntVar(&device, "device", 0, "The device number.")
	flag.StringVar(&stream, "stream", "playback", "The stream direction ('playback' or 'capture').")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options]\n\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "Displays information about an ALSA PCM device.")
		fmt.Fprintln(os.Stderr, "\nOptions:")
		flag.PrintDefaults()
	}

	flag.Parse()

	var pcmFlags alsa.PcmFlag
	switch strings.ToLower(stream) {
	case "playback":
		pcmFlags = alsa.PCM_OUT
	case "capture":
		pcmFlags = alsa.PCM_IN
	default:
		fmt.Fprintf(os.Stderr, "Error: invalid stream direction '%s'. Must be 'playback' or 'capture'.\n", stream)
		os.Exit(1)
	}

	fmt.Printf("PCM card %d, device %d, stream %s:\n", card, device, stream)

	// Get the hardware parameters for the specified PCM device.
	// This is the core call to the alsa library to query capabilities.
	params, err := alsa.PcmParamsGet(uint(card), uint(device), pcmFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting PCM parameters: %v\n", err)
		os.Exit(1)
	}
	// Ensure that the allocated resources for the parameters are freed when the function exits.
	defer params.Free()

	// The PcmParams object has a String() method that conveniently formats
	// all the capabilities into a human-readable string, which we print here.
	fmt.Println(params)
}
