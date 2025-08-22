package main

import (
	"encoding/binary"
	"errors"
	"io"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
	"github.com/hajimehoshi/go-mp3"
)

// AudioDecoder is an interface that abstracts the audio decoding process.
// It allows the playback loop to handle different audio formats (WAV, MP3, etc.) uniformly.
type AudioDecoder interface {
	// PCMBuffer reads decoded PCM audio data into the provided buffer.
	// It returns the number of samples (not frames) read.
	PCMBuffer(buf *audio.IntBuffer) (n int, err error)
	// Duration returns the total duration of the audio stream.
	Duration() (time.Duration, error)
	// NumChans returns the number of audio channels (e.g., 1 for mono, 2 for stereo).
	NumChans() uint16
	// SampleRate returns the sample rate in Hz (e.g., 44100).
	SampleRate() uint32
	// BitDepth returns the bit depth of the decoded samples (e.g., 16, 24).
	BitDepth() uint16
	// IsFloat returns true if the audio format is floating-point.
	IsFloat() bool
}

// wavDecoderWrapper wraps the go-audio WAV decoder to implement the AudioDecoder interface.
type wavDecoderWrapper struct {
	*wav.Decoder
}

// newWavDecoder creates a new WAV decoder wrapper.
func newWavDecoder(r io.ReadSeeker) (AudioDecoder, error) {
	decoder := wav.NewDecoder(r)
	if !decoder.IsValidFile() {
		return nil, errors.New("invalid WAV file")
	}

	return &wavDecoderWrapper{Decoder: decoder}, nil
}

func (w *wavDecoderWrapper) SampleRate() uint32 { return w.Decoder.SampleRate }
func (w *wavDecoderWrapper) NumChans() uint16   { return w.Decoder.NumChans }
func (w *wavDecoderWrapper) BitDepth() uint16   { return uint16(w.Decoder.BitDepth) }
func (w *wavDecoderWrapper) IsFloat() bool      { return w.Decoder.WavAudioFormat == 3 } // 3 == IEEE float

// mp3DecoderWrapper wraps the go-mp3 decoder to implement the AudioDecoder interface.
type mp3DecoderWrapper struct {
	decoder    *mp3.Decoder
	sampleRate uint32
	numChans   uint16
	bitDepth   uint16
	length     int64 // Total decoded size in bytes
}

// newMp3Decoder creates a new MP3 decoder wrapper.
func newMp3Decoder(r io.Reader) (AudioDecoder, error) {
	decoder, err := mp3.NewDecoder(r)
	if err != nil {
		return nil, err
	}

	return &mp3DecoderWrapper{
		decoder:    decoder,
		sampleRate: uint32(decoder.SampleRate()),
		numChans:   2,  // always decodes to stereo
		bitDepth:   16, // always decodes to 16-bit
		length:     decoder.Length(),
	}, nil
}

// PCMBuffer reads from the MP3 decoder and converts the 16-bit PCM byte data to integers.
func (m *mp3DecoderWrapper) PCMBuffer(buf *audio.IntBuffer) (n int, err error) {
	// The number of samples we can fit in the buffer.
	numSamples := len(buf.Data)
	// Bytes needed to hold those samples (16-bit = 2 bytes/sample).
	bytesToRead := numSamples * 2
	byteBuf := make([]byte, bytesToRead)

	// Read decoded 16-bit little-endian PCM data.
	bytesRead, err := m.decoder.Read(byteBuf)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}

	samplesRead := bytesRead / 2
	for i := 0; i < samplesRead; i++ {
		// Combine two bytes into a signed 16-bit integer.
		sample := int16(binary.LittleEndian.Uint16(byteBuf[i*2:]))
		buf.Data[i] = int(sample)
	}

	return samplesRead, err
}

func (m *mp3DecoderWrapper) Duration() (time.Duration, error) {
	// A frame is one sample per channel.
	bytesPerFrame := int64(m.numChans) * int64(m.bitDepth/8)
	if bytesPerFrame == 0 {
		return 0, errors.New("invalid frame size")
	}

	totalFrames := m.length / bytesPerFrame
	seconds := float64(totalFrames) / float64(m.sampleRate)

	return time.Duration(seconds * float64(time.Second)), nil
}

func (m *mp3DecoderWrapper) SampleRate() uint32 { return m.sampleRate }
func (m *mp3DecoderWrapper) NumChans() uint16   { return m.numChans }
func (m *mp3DecoderWrapper) BitDepth() uint16   { return m.bitDepth }
func (m *mp3DecoderWrapper) IsFloat() bool      { return false } // MP3 decodes to integer PCM
