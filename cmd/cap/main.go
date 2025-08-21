package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"

	"github.com/gen2brain/alsa"
)

func main() {
	var (
		card        int
		device      int
		periodSize  int
		periodCount int
		channels    int
		rate        int
		formatStr   string
		duration    int
		mmap        bool
	)

	flag.IntVar(&card, "card", 0, "The card to capture from")
	flag.IntVar(&device, "device", 0, "The device to capture from")
	flag.IntVar(&periodSize, "period-size", 1024, "The size of a period in frames")
	flag.IntVar(&periodCount, "period-count", 4, "The number of periods")
	flag.IntVar(&channels, "channels", 2, "The number of channels")
	flag.IntVar(&rate, "rate", 48000, "The sample rate in Hz")
	flag.StringVar(&formatStr, "format", "s16", "The sample format (s16, s24, s32)")
	flag.IntVar(&duration, "duration", 5, "The duration of the capture in seconds")
	flag.BoolVar(&mmap, "mmap", false, "Use memory-mapped (MMAP) I/O")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] <output-wav-file>\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "\nOptions:")
		for _, name := range []string{"card", "device", "period-size", "period-count", "channels", "rate", "format", "duration", "mmap"} {
			f := flag.Lookup(name)
			if f != nil {
				fmt.Fprintf(os.Stderr, "  --%s\n    \t%v (default %q)\n", f.Name, f.Usage, f.DefValue)
			}
		}
	}

	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(1)
	}

	outputPath := flag.Arg(0)

	// Determine a PCM format from the format string.
	pcmFormat, bitDepth, err := determineFormat(formatStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error determining format: %v\n", err)
		os.Exit(1)
	}

	// Configure the ALSA PCM device for capture.
	config := alsa.Config{
		Channels:    uint32(channels),
		Rate:        uint32(rate),
		PeriodSize:  uint32(periodSize),
		PeriodCount: uint32(periodCount),
		Format:      pcmFormat,
	}

	fmt.Printf("Capturing from ALSA device: hw:%d,%d\n", card, device)
	fmt.Printf("Configuration: %d channels, %d Hz, %s\n", config.Channels, config.Rate, alsa.PcmParamFormatNames[config.Format])
	fmt.Printf("Period size: %d, Period count: %d\n", config.PeriodSize, config.PeriodCount)
	fmt.Printf("Capture duration: %d seconds\n", duration)
	fmt.Printf("Mode: %s\n", map[bool]string{false: "Standard I/O", true: "MMAP"}[mmap])

	// Open the ALSA PCM device for capture (PCM_IN).
	flags := alsa.PCM_IN
	if mmap {
		flags |= alsa.PCM_MMAP
	}

	pcm, err := alsa.PcmOpen(uint(card), uint(device), flags, &config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening ALSA device: %v\n", err)
		os.Exit(1)
	}
	defer pcm.Close()

	// A stream must be in the PREPARED state before it can be read from.
	// This is especially critical for MMAP streams.
	if err := pcm.Prepare(); err != nil {
		fmt.Fprintf(os.Stderr, "Error preparing ALSA device: %v\n", err)
		os.Exit(1)
	}

	// Create the output WAV file.
	wavFile, err := os.Create(outputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating WAV file: %v\n", err)
		os.Exit(1)
	}
	defer wavFile.Close()

	// Configure the WAV encoder.
	encoder := wav.NewEncoder(wavFile,
		int(config.Rate),
		bitDepth,
		int(config.Channels),
		1, // Audio format 1 is PCM
	)
	defer encoder.Close()

	// Calculate total frames to capture.
	totalFramesToCapture := uint32(duration) * config.Rate
	var framesCaptured uint32

	// Set up a signal handler to gracefully stop on Ctrl+C.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	fmt.Println("Starting capture... Press Ctrl+C to stop early.")

	// Create a buffer to hold one period of audio data.
	chunkFrames := config.PeriodSize
	buffer := make([]byte, alsa.PcmFramesToBytes(pcm, chunkFrames))

	// Capture loop.
	keepRunning := true
	for keepRunning && framesCaptured < totalFramesToCapture {
		select {
		case <-sigChan:
			fmt.Println("\nCapture interrupted by user.")
			keepRunning = false
		default:
			var read int
			var readErr error

			// Read interleaved audio data from the ALSA device.
			if mmap {
				read, readErr = pcm.MmapRead(buffer)
			} else {
				// For standard I/O ReadI returns frames, so we convert to bytes.
				framesRead, err := pcm.ReadI(buffer, chunkFrames)
				if err == nil {
					read = int(alsa.PcmFramesToBytes(pcm, uint32(framesRead)))
				}

				readErr = err
			}

			if readErr != nil {
				// The library's I/O functions attempt to recover from overruns (EPIPE),
				// so if we get an error here, it's likely unrecoverable.
				fmt.Fprintf(os.Stderr, "Error reading from ALSA device: %v\n", readErr)
				if errors.Is(readErr, syscall.EPIPE) {
					fmt.Fprintln(os.Stderr, "Got EPIPE (overrun), and recovery failed.")
				}

				keepRunning = false

				continue
			}

			if read > 0 {
				bytesRead := uint32(read)
				framesReadInChunk := alsa.PcmBytesToFrames(pcm, bytesRead)

				// Convert the raw byte buffer to an audio.IntBuffer for the encoder.
				intBuffer, err := bytesToIntBuffer(buffer[:bytesRead], pcm.Format(), int(pcm.Channels()))
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error converting buffer: %v\n", err)

					break
				}

				// Write the audio buffer to the WAV file.
				if err := encoder.Write(intBuffer); err != nil {
					fmt.Fprintf(os.Stderr, "Error writing to WAV file: %v\n", err)

					break
				}

				framesCaptured += framesReadInChunk
			}
		}
	}

	// For MMAP, explicitly stop the stream before closing.
	if mmap {
		pcm.Stop()
	}

	durationCaptured := time.Duration(float64(framesCaptured)/float64(config.Rate)) * time.Second
	fmt.Printf("Capture finished. Wrote %d frames (%.2f seconds) to %s\n", framesCaptured, durationCaptured.Seconds(), outputPath)
}

// determineFormat maps a string identifier to an ALSA format and WAV bit depth.
func determineFormat(formatStr string) (alsa.PcmFormat, int, error) {
	switch formatStr {
	case "s16":
		return alsa.SNDRV_PCM_FORMAT_S16_LE, 16, nil
	case "s24":
		// Note: S24_LE in ALSA is 24 bits of data packed into a 32-bit integer.
		// The wav encoder expects the bit depth to be 24.
		return alsa.SNDRV_PCM_FORMAT_S24_LE, 24, nil
	case "s32":
		return alsa.SNDRV_PCM_FORMAT_S32_LE, 32, nil
	default:
		return 0, 0, fmt.Errorf("unsupported format: '%s'. Supported formats are s16, s24, s32", formatStr)
	}
}

// bytesToIntBuffer converts a raw byte slice from ALSA into an audio.IntBuffer
// that the go-audio/wav encoder can understand.
func bytesToIntBuffer(data []byte, format alsa.PcmFormat, channels int) (*audio.IntBuffer, error) {
	bytesPerSample := int(alsa.PcmFormatToBits(format) / 8)
	if bytesPerSample == 0 {
		return nil, fmt.Errorf("unsupported ALSA format for conversion: %v", format)
	}

	numSamples := len(data) / bytesPerSample
	intData := make([]int, numSamples)

	offset := 0
	for i := 0; i < numSamples; i++ {
		switch format {
		case alsa.SNDRV_PCM_FORMAT_S16_LE:
			sample := int16(binary.LittleEndian.Uint16(data[offset:]))
			intData[i] = int(sample)
		case alsa.SNDRV_PCM_FORMAT_S24_LE:
			// S24_LE is stored in 32 bits (4 bytes), with the upper 8 bits unused.
			// We read 3 bytes in little-endian order and sign-extend it.
			val := uint32(data[offset]) | uint32(data[offset+1])<<8 | uint32(data[offset+2])<<16
			if val&0x800000 != 0 { // Check if the 24th bit is set (negative)
				val |= 0xFF000000 // Sign extend
			}
			intData[i] = int(int32(val))
		case alsa.SNDRV_PCM_FORMAT_S32_LE:
			sample := int32(binary.LittleEndian.Uint32(data[offset:]))
			intData[i] = int(sample)
		default:
			return nil, fmt.Errorf("unhandled ALSA format in conversion: %v", format)
		}
		offset += bytesPerSample
	}

	bitDepth := 0
	switch format {
	case alsa.SNDRV_PCM_FORMAT_S16_LE:
		bitDepth = 16
	case alsa.SNDRV_PCM_FORMAT_S24_LE:
		bitDepth = 24
	case alsa.SNDRV_PCM_FORMAT_S32_LE:
		bitDepth = 32
	}

	return &audio.IntBuffer{
		Format: &audio.Format{
			NumChannels: channels,
			SampleRate:  0, // Sample rate is set on the encoder, not needed here.
		},
		Data:           intData,
		SourceBitDepth: bitDepth,
	}, nil
}
