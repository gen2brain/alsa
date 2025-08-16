package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
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
		mmap        bool
	)

	flag.IntVar(&card, "card", 0, "The card to receive the audio")
	flag.IntVar(&device, "device", 0, "The device to receive the audio")
	flag.IntVar(&periodSize, "period-size", 1024, "The size of a period in frames")
	flag.IntVar(&periodCount, "period-count", 4, "The number of periods")
	flag.IntVar(&channels, "channels", 0, "The amount of channels per frame (0 = use WAV file's channels)")
	flag.IntVar(&rate, "rate", 0, "The amount of frames per second (0 = use WAV file's rate)")
	flag.StringVar(&formatStr, "format", "", "The sample format (s16, s24, s32, float, float64)")
	flag.BoolVar(&mmap, "mmap", false, "Use memory-mapped (MMAP) I/O")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] <wav-file>\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "\nOptions:")
		for _, name := range []string{"card", "device", "period-size", "period-count", "channels", "rate", "format", "mmap"} {
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

	wavPath := flag.Arg(0)
	wavFile, err := os.Open(wavPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening WAV file: %v\n", err)
		os.Exit(1)
	}
	defer wavFile.Close()

	decoder := wav.NewDecoder(wavFile)
	if !decoder.IsValidFile() {
		fmt.Fprintln(os.Stderr, "Invalid WAV file")
		os.Exit(1)
	}

	config := alsa.Config{
		PeriodSize:  uint32(periodSize),
		PeriodCount: uint32(periodCount),
	}

	// Determine channels and rate from flags or WAV file.
	if channels > 0 {
		config.Channels = uint32(channels)
	} else {
		config.Channels = uint32(decoder.NumChans)
	}

	if rate > 0 {
		config.Rate = uint32(rate)
	} else {
		config.Rate = decoder.SampleRate
	}

	// Determine a PCM format from flags or by inspecting the WAV file.
	pcmFormat, err := determineFormat(formatStr, decoder)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error determining format: %v\n", err)
		os.Exit(1)
	}
	config.Format = pcmFormat

	fmt.Printf("Playing WAV file: %s\n", wavPath)
	fmt.Printf("ALSA device: hw:%d,%d\n", card, device)
	fmt.Printf("Configuration: %d channels, %d Hz, %s\n", config.Channels, config.Rate, alsa.PcmParamFormatNames[config.Format])
	fmt.Printf("Period size: %d, Period count: %d\n", config.PeriodSize, config.PeriodCount)
	fmt.Printf("Mode: %s\n", map[bool]string{false: "Standard I/O", true: "MMAP"}[mmap])

	flags := alsa.PCM_OUT
	if mmap {
		flags |= alsa.PCM_MMAP
	}

	pcm, err := alsa.PcmOpen(uint(card), uint(device), flags, &config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening ALSA device: %v\n", err)
		os.Exit(1)
	}
	defer pcm.Close()

	if mmap {
		if err := pcm.Prepare(); err != nil {
			fmt.Fprintf(os.Stderr, "Error preparing MMAP stream: %v\n", err)
			os.Exit(1)
		}
	}

	totalFramesDuration, err := decoder.Duration()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting WAV duration: %v\n", err)
		os.Exit(1)
	}

	totalFrames := uint32(totalFramesDuration.Seconds() * float64(decoder.SampleRate))
	framesWritten := uint32(0)

	fmt.Println("Starting playback...")
	startTime := time.Now()

	chunkFrames := config.PeriodSize
	pcmBuffer := &audio.IntBuffer{
		Format: &audio.Format{
			NumChannels: int(decoder.NumChans),
			SampleRate:  int(decoder.SampleRate),
		},
		Data: make([]int, int(chunkFrames)*int(decoder.NumChans)),
	}

	for {
		// n is the number of SAMPLES read from the decoder.
		n, err := decoder.PCMBuffer(pcmBuffer)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			fmt.Fprintf(os.Stderr, "Error reading PCM buffer from WAV: %v\n", err)
			os.Exit(1)
		}

		if n == 0 {
			break
		}

		// The actual samples read.
		samples := pcmBuffer.Data[:n]
		// The number of frames corresponding to the samples read.
		framesInChunk := uint32(n / int(config.Channels))
		var dataToWrite any

		// Convert the generic `[]int` from the decoder to the specific typed
		// slice needed by the ALSA device.
		switch config.Format {
		case alsa.PCM_FORMAT_S8:
			slice := make([]int8, n)
			for i, s := range samples {
				// Scale sample from source bit depth to 8-bit.
				slice[i] = int8(s >> (decoder.BitDepth - 8))
			}
			dataToWrite = slice
		case alsa.PCM_FORMAT_S16_LE:
			slice := make([]int16, n)
			for i, s := range samples {
				// Clamp value to int16 range before casting.
				if s > 32767 {
					s = 32767
				} else if s < -32768 {
					s = -32768
				}
				slice[i] = int16(s)
			}
			dataToWrite = slice
		case alsa.PCM_FORMAT_S24_LE, alsa.PCM_FORMAT_S32_LE:
			// For 24-bit and 32-bit formats, we pass a slice of int32.
			slice := make([]int32, n)
			for i, s := range samples {
				slice[i] = int32(s)
			}
			dataToWrite = slice
		case alsa.PCM_FORMAT_FLOAT_LE:
			slice := make([]float32, n)
			// Normalize integer sample to float range [-1.0, 1.0].
			maxVal := 1 << (decoder.BitDepth - 1)
			for i, s := range samples {
				slice[i] = float32(s) / float32(maxVal)
			}
			dataToWrite = slice
		case alsa.PCM_FORMAT_FLOAT64_LE:
			slice := make([]float64, n)
			// Normalize integer sample to float range [-1.0, 1.0].
			maxVal := 1 << (decoder.BitDepth - 1)
			for i, s := range samples {
				slice[i] = float64(s) / float64(maxVal)
			}
			dataToWrite = slice
		}

		if dataToWrite == nil {
			fmt.Fprintf(os.Stderr, "Format %s not handled in conversion, skipping chunk.\n", alsa.PcmParamFormatNames[config.Format])
			continue
		}

		// Write the typed slice directly to the ALSA device.
		var writeErr error
		if mmap {
			writtenBytes, err := pcm.MmapWrite(dataToWrite)
			if err != nil {
				writeErr = err
			} else {
				framesWritten += alsa.PcmBytesToFrames(pcm, uint32(writtenBytes))
			}
		} else {
			writtenFrames, err := pcm.WriteI(dataToWrite, framesInChunk)
			if err != nil {
				writeErr = err
			} else {
				framesWritten += uint32(writtenFrames)
			}
		}

		if writeErr != nil {
			fmt.Fprintf(os.Stderr, "Error writing to ALSA device: %v\n", writeErr)
			if errors.Is(writeErr, syscall.EPIPE) {
				fmt.Fprintln(os.Stderr, "Got EPIPE (underrun), and recovery failed.")
			}

			break
		}
	}

	if mmap {
		pcm.Stop()
	} else {
		pcm.Drain()
	}

	fmt.Printf("Playback finished in %v. (%d/%d frames played)\n", time.Since(startTime), framesWritten, totalFrames)
}

// determineFormat selects the appropriate ALSA PCM format.
func determineFormat(formatStr string, decoder *wav.Decoder) (alsa.PcmFormat, error) {
	if formatStr != "" {
		switch formatStr {
		case "s8":
			return alsa.PCM_FORMAT_S8, nil
		case "s16":
			return alsa.PCM_FORMAT_S16_LE, nil
		case "s24":
			return alsa.PCM_FORMAT_S24_LE, nil
		case "s32":
			return alsa.PCM_FORMAT_S32_LE, nil
		case "float":
			return alsa.PCM_FORMAT_FLOAT_LE, nil
		case "float64":
			return alsa.PCM_FORMAT_FLOAT64_LE, nil
		default:
			return 0, fmt.Errorf("unsupported format string: %s", formatStr)
		}
	}

	if decoder.WavAudioFormat == 3 { // IEEE float
		switch decoder.BitDepth {
		case 32:
			return alsa.PCM_FORMAT_FLOAT_LE, nil
		case 64:
			return alsa.PCM_FORMAT_FLOAT64_LE, nil
		default:
			return 0, fmt.Errorf("unsupported float bit depth from WAV: %d", decoder.BitDepth)
		}
	}

	// Assume integer PCM.
	switch decoder.BitDepth {
	case 8:
		return alsa.PCM_FORMAT_S8, nil
	case 16:
		return alsa.PCM_FORMAT_S16_LE, nil
	case 24:
		return alsa.PCM_FORMAT_S24_LE, nil
	case 32:
		return alsa.PCM_FORMAT_S32_LE, nil
	default:
		return 0, fmt.Errorf("unsupported integer bit depth from WAV: %d", decoder.BitDepth)
	}
}
