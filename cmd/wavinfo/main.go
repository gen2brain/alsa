package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/go-audio/wav"
)

func main() {
	var help bool
	flag.BoolVar(&help, "help", false, "Show this help message")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] <wav-file>\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "\nOptions:")
		fmt.Fprintln(os.Stderr, "  --help      Show this help message")
	}

	flag.Parse()

	if help || flag.NArg() != 1 {
		flag.Usage()
		os.Exit(1)
	}

	wavPath := flag.Arg(0)

	file, err := os.Open(wavPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open file: %v\n", err)
		os.Exit(1)
	}
	defer file.Close()

	decoder := wav.NewDecoder(file)

	if !decoder.IsValidFile() {
		fmt.Fprintln(os.Stderr, "Invalid WAV file")
		os.Exit(1)
	}

	fmt.Printf("Filename:           %s\n", wavPath)
	fmt.Printf("Channels:           %d\n", decoder.NumChans)
	fmt.Printf("Sample Rate:        %d Hz\n", decoder.SampleRate)
	fmt.Printf("Bits Per Sample:    %d\n", decoder.BitDepth)

	// Determine the format string (PCM or Float).
	// Format 1 is integer PCM, Format 3 is IEEE Float.
	var formatStr string
	if decoder.WavAudioFormat == 3 {
		formatStr = "IEEE Float"
	} else {
		formatStr = "Signed Integer PCM"
	}

	fmt.Printf("Format:             %s\n", formatStr)

	duration, err := decoder.Duration()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get duration: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Duration:           %s\n", formatDuration(duration))

	// The total number of frames can be calculated from the duration and sample rate.
	totalFrames := int(duration.Seconds() * float64(decoder.SampleRate))

	fmt.Printf("Frames:             %d\n", totalFrames)
}

// formatDuration formats a time.Duration into a more readable HH:MM:SS.ms format.
func formatDuration(d time.Duration) string {
	nanos := d.Nanoseconds() % 1e9
	millis := nanos / 1e6

	seconds := int(d.Seconds()) % 60
	minutes := int(d.Minutes()) % 60
	hours := int(d.Hours())

	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, seconds, millis)
}
