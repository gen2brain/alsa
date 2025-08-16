package alsa_test

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/gen2brain/alsa"
)

// To run these tests, the 'snd-aloop' kernel module must be loaded:
//
// sudo modprobe snd-aloop
//
// This creates virtual loopback sound cards that allow testing playback and capture.

var (
	// kDefaultConfig mirrors the configuration used in the C++ tests.
	kDefaultConfig = alsa.Config{
		Channels:    2,
		Rate:        48000,
		PeriodSize:  1024,
		PeriodCount: 4,
		Format:      alsa.PCM_FORMAT_S16_LE,
	}
)

func TestPcmFormatToBits(t *testing.T) {
	testCases := map[alsa.PcmFormat]uint32{
		alsa.PCM_FORMAT_INVALID:    0, // Go implementation correctly returns 0, unlike the C++ test's FIXME.
		alsa.PCM_FORMAT_S16_LE:     16,
		alsa.PCM_FORMAT_S32_LE:     32,
		alsa.PCM_FORMAT_S8:         8,
		alsa.PCM_FORMAT_S24_LE:     32, // 24-bit stored in 32-bit container
		alsa.PCM_FORMAT_S24_3LE:    24, // Packed 24-bit
		alsa.PCM_FORMAT_S16_BE:     16,
		alsa.PCM_FORMAT_S24_BE:     32,
		alsa.PCM_FORMAT_S24_3BE:    24,
		alsa.PCM_FORMAT_S32_BE:     32,
		alsa.PCM_FORMAT_FLOAT_LE:   32,
		alsa.PCM_FORMAT_FLOAT_BE:   32,
		alsa.PCM_FORMAT_FLOAT64_LE: 64,
		alsa.PCM_FORMAT_FLOAT64_BE: 64,
	}

	for format, expectedBits := range testCases {
		t.Run(alsa.PcmParamFormatNames[format], func(t *testing.T) {
			bits := alsa.PcmFormatToBits(format)
			if bits != expectedBits {
				t.Errorf("PcmFormatToBits(%v) = %d; want %d", format, bits, expectedBits)
			}
		})
	}
}

// TestPcmHardware runs all hardware-related tests sequentially to avoid race conditions
// from multiple tests trying to access the same ALSA device concurrently.
func TestPcmHardware(t *testing.T) {
	t.Run("PcmOpenAndClose", testPcmOpenAndClose)
	t.Run("PcmOpenByName", testPcmOpenByName)
	t.Run("PcmPlaybackStartup", testPcmPlaybackStartup)
	t.Run("PcmGetters", testPcmGetters)
	t.Run("PcmFramesBytesConvert", testPcmFramesBytesConvert)
	t.Run("PcmTimestampBeforeStart", testPcmTimestampBeforeStart)
	t.Run("PcmWriteiFailsOnCapture", testPcmWriteiFailsOnCapture)
	t.Run("PcmReadiFailsOnPlayback", testPcmReadiFailsOnPlayback)
	t.Run("PcmWriteiTiming", testPcmWriteiTiming)
	t.Run("PcmGetDelay", testPcmGetDelay)
	t.Run("PcmReadiTiming", testPcmReadiTiming)
	t.Run("PcmMmapWrite", testPcmMmapWrite)
	t.Run("PcmStop", testPcmStop)
	t.Run("PcmParams", testPcmParams)
	t.Run("SetConfig", testSetConfig)
	t.Run("PcmLink", testPcmLink)
	t.Run("PcmDrain", testPcmDrain)
	t.Run("PcmPause", testPcmPause)
	t.Run("PcmLoopback", testPcmLoopback)
	t.Run("PcmMmapLoopback", testPcmMmapLoopback)
}

func testPcmOpenAndClose(t *testing.T) {
	// Test opening a non-existent device
	pcm, err := alsa.PcmOpen(1000, 1000, alsa.PCM_OUT, &kDefaultConfig)
	if err == nil {
		t.Error("expected error when opening non-existent device, but got nil")
		pcm.Close()
	}

	if pcm != nil && pcm.IsReady() {
		t.Error("pcm.IsReady() should be false for non-existent device")
	}

	// Test closing a nil pcm
	if err := (*alsa.PCM)(nil).Close(); err != nil {
		t.Errorf("closing a nil pcm should not return an error, but got %v", err)
	}

	// Test various open flags
	testCases := []struct {
		name  string
		flags alsa.PcmFlag
	}{
		{"OUT", alsa.PCM_OUT},
		{"OUT_MMAP", alsa.PCM_OUT | alsa.PCM_MMAP},
		{"OUT_MMAP_NOIRQ", alsa.PCM_OUT | alsa.PCM_MMAP | alsa.PCM_NOIRQ},
		{"OUT_NONBLOCK", alsa.PCM_OUT | alsa.PCM_NONBLOCK},
		{"OUT_MONOTONIC", alsa.PCM_OUT | alsa.PCM_MONOTONIC},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), tc.flags, &kDefaultConfig)
			if err != nil {
				// The TTSTAMP ioctl for PCM_MONOTONIC is not supported by all kernels/devices.
				// If it fails with ENOTTY or EINVAL, skip the test gracefully.
				if tc.flags&alsa.PCM_MONOTONIC != 0 {
					var errno syscall.Errno
					if errors.As(err, &errno) && (errors.Is(errno, syscall.ENOTTY) || errors.Is(errno, syscall.EINVAL)) {
						t.Skipf("Skipping monotonic test, TTSTAMP ioctl not supported by device: %v", err)

						return
					}
				}

				t.Fatalf("PcmOpen failed: %v", err)
			}

			if !pcm.IsReady() {
				t.Fatal("pcm.IsReady() returned false after successful open")
			}

			if err := pcm.Close(); err != nil {
				t.Fatalf("pcm.Close() failed: %v", err)
			}
		})
	}
}

func testPcmOpenByName(t *testing.T) {
	// Test valid name
	name := fmt.Sprintf("hw:%d,%d", loopbackCard, loopbackPlaybackDevice)
	pcm, err := alsa.PcmOpenByName(name, alsa.PCM_OUT, &kDefaultConfig)
	require.NoError(t, err, "PcmOpenByName failed for a valid name")
	require.NotNil(t, pcm)
	require.True(t, pcm.IsReady())
	pcm.Close()

	// Test invalid names
	_, err = alsa.PcmOpenByName("invalid_name", alsa.PCM_OUT, &kDefaultConfig)
	require.Error(t, err, "PcmOpenByName should fail for a name without 'hw:' prefix")

	_, err = alsa.PcmOpenByName("hw:foo,bar", alsa.PCM_OUT, &kDefaultConfig)
	require.Error(t, err, "PcmOpenByName should fail for non-numeric card/device")

	_, err = alsa.PcmOpenByName("hw:0", alsa.PCM_OUT, &kDefaultConfig)
	require.Error(t, err, "PcmOpenByName should fail for incomplete name")
}

func testPcmStop(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &kDefaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &kDefaultConfig)
	require.NoError(t, err)
	// No defer on capturePcm.Close(), we manage it manually with the goroutine.

	err = pcm.Link(capturePcm)
	if err != nil {
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}
	defer pcm.Unlink()

	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)

	// Goroutine to consume data to prevent the writer from blocking.
	go func() {
		defer wg.Done()
		buffer := make([]byte, alsa.PcmFramesToBytes(capturePcm, capturePcm.PeriodSize()))
		for {
			select {
			case <-done:
				// Close the capture PCM from within the goroutine that uses it.
				capturePcm.Close()
				return
			default:
				// This read will block until data is written or the stream is closed.
				_, err := capturePcm.ReadI(buffer, capturePcm.PeriodSize())
				if err != nil {
					// Expect an error when the stream is closed.
					return
				}
			}
		}
	}()

	// Write some data to start the stream
	buffer := make([]byte, alsa.PcmFramesToBytes(pcm, pcm.PeriodSize()))
	_, err = pcm.WriteI(buffer, pcm.PeriodSize())
	require.NoError(t, err)

	state, _ := pcm.State()
	require.Equal(t, alsa.PCM_STATE_RUNNING, state, "Stream should be in RUNNING state after writing")

	// Now stop the stream
	err = pcm.Stop()
	require.NoError(t, err, "pcm.Stop() failed")

	state, _ = pcm.State()
	require.Equal(t, alsa.PCM_STATE_SETUP, state, "Stream should be in SETUP state after stopping")

	// Cleanly shut down the goroutine.
	close(done)
	wg.Wait()
}

func testPcmPlaybackStartup(t *testing.T) {
	// This test replicates the simple "pcm-play.go" example failing scenario.
	// It ensures a playback-only stream can be started correctly by the first write,
	// without an explicit Start() call which would cause an immediate XRUN.
	config := kDefaultConfig
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &config)
	require.NoError(t, err, "Failed to open PCM for playback")
	defer pcm.Close()

	// To prevent the write from blocking indefinitely on the loopback device,
	// we must consume the data on the other end.
	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &config)
	require.NoError(t, err, "Could not open capture side of loopback")
	defer capturePcm.Close()

	// Link them to ensure they start together. This is good practice for loopback tests.
	err = pcm.Link(capturePcm)
	if err != nil {
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}
	defer pcm.Unlink()

	// We'll read from the capture device in a goroutine.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Make a buffer big enough to hold all the data we plan to write.
		captureFrames := config.PeriodSize * 2
		captureBuffer := make([]byte, alsa.PcmFramesToBytes(capturePcm, captureFrames))

		// This read will block until the playback side writes data and the stream starts.
		_, readErr := capturePcm.ReadI(captureBuffer, captureFrames)

		// We don't need to check the error here; we just need to unblock the writer.
		// But for robustness, we check for EBADF, which can happen during teardown.
		var errno syscall.Errno
		if readErr != nil && (!errors.As(readErr, &errno) || (!errors.Is(errno, syscall.EBADF) && !errors.Is(errno, syscall.EPIPE))) {
			// In a real test, we'd probably send this error back to the main thread.
			t.Logf("capture goroutine ReadI failed: %v", readErr)
		}
	}()

	// Create a buffer for one period.
	frames := config.PeriodSize
	buffer := make([]byte, alsa.PcmFramesToBytes(pcm, frames))

	// This first write should succeed and implicitly start both linked streams.
	written, err := pcm.WriteI(buffer, frames)

	// The key assertion is that this first write does not fail.
	// It should print the specific error if it fails, as requested.
	if err != nil {
		t.Fatalf("The first WriteI call failed with an error.\nError: %v", err)
	}

	require.Equal(t, int(frames), written, "The first WriteI call did not write the expected number of frames.")

	// A second write should also succeed.
	written, err = pcm.WriteI(buffer, frames)
	require.NoError(t, err, "The second WriteI call failed.")
	require.Equal(t, int(frames), written, "The second WriteI call did not write the expected number of frames.")

	// Clean up the goroutine.
	wg.Wait()
}

func testPcmGetters(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &kDefaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	if pcm.Fd() == ^uintptr(0) {
		t.Error("expected a valid file descriptor")
	}

	require.Equal(t, kDefaultConfig.Channels, pcm.Channels())
	require.Equal(t, kDefaultConfig.Rate, pcm.Rate())
	require.Equal(t, kDefaultConfig.Format, pcm.Format())
	require.Equal(t, kDefaultConfig.PeriodSize*kDefaultConfig.PeriodCount, pcm.BufferSize())
	require.Equal(t, "", pcm.Error())
}

func testPcmFramesBytesConvert(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &kDefaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	bytesPerFrame := alsa.PcmFormatToBits(kDefaultConfig.Format) / 8 * kDefaultConfig.Channels
	require.Equal(t, bytesPerFrame, alsa.PcmFramesToBytes(pcm, 1))
	require.Equal(t, uint32(1), alsa.PcmBytesToFrames(pcm, bytesPerFrame))
}

func testPcmTimestampBeforeStart(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &kDefaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	// This test is expected to fail on non-mmap streams.
	_, _, err = pcm.Timestamp()
	require.Error(t, err, "Timestamp() should fail on a non-MMAP stream")
	require.Contains(t, err.Error(), "only available for MMAP streams")
}

func testPcmWriteiFailsOnCapture(t *testing.T) {
	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &kDefaultConfig)
	require.NoError(t, err)
	defer capturePcm.Close()

	buffer := make([]byte, 128)
	_, err = capturePcm.WriteI(buffer, alsa.PcmBytesToFrames(capturePcm, uint32(len(buffer))))
	require.Error(t, err, "expected error when calling WriteI on a capture stream")
}

func testPcmReadiFailsOnPlayback(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &kDefaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	buffer := make([]byte, 128)
	_, err = pcm.ReadI(buffer, alsa.PcmBytesToFrames(pcm, uint32(len(buffer))))

	require.Error(t, err, "expected error when calling ReadI on a playback stream")
	require.Contains(t, err.Error(), "cannot read from a playback device")
}

func testPcmWriteiTiming(t *testing.T) {
	config := kDefaultConfig
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &config)
	require.NoError(t, err)
	defer pcm.Close()

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &config)
	require.NoError(t, err, "Could not open capture side of device")
	defer capturePcm.Close()

	// Link the streams to start them synchronously.
	err = pcm.Link(capturePcm)
	if err != nil {
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}
	defer pcm.Unlink()

	var wg sync.WaitGroup
	done := make(chan struct{})
	readyToRead := make(chan struct{})

	// We must actively consume the data on the capture side.
	wg.Add(1)
	go func() {
		defer wg.Done()
		captureBuffer := make([]byte, alsa.PcmFramesToBytes(capturePcm, capturePcm.PeriodSize()))
		frames := capturePcm.PeriodSize()

		close(readyToRead) // Signal that the reader is about to start its loop

		for {
			select {
			case <-done:
				return
			default:
				// This will block until the producer (main thread) starts writing.
				_, err := capturePcm.ReadI(captureBuffer, frames)
				if err != nil {
					var errno syscall.Errno
					// EBADF is expected if the main test closes the PCM before this goroutine exits.
					if errors.As(err, &errno) && (errors.Is(errno, syscall.EBADF) || errors.Is(errno, syscall.EPIPE)) {
						return
					}

					// Don't fail the test from inside the goroutine, just stop.
					return
				}
			}
		}
	}()

	defer func() {
		close(done)
		wg.Wait()
	}()

	// Wait until the consumer is ready to read.
	<-readyToRead

	const writeCount = 20
	bufferSize := alsa.PcmFramesToBytes(pcm, config.PeriodSize)
	buffer := make([]byte, bufferSize)
	frames := alsa.PcmBytesToFrames(pcm, bufferSize)

	start := time.Now()
	for i := 0; i < writeCount; i++ {
		// The first call to WriteI will implicitly start both linked streams.
		written, err := pcm.WriteI(buffer, frames)
		if err != nil {
			t.Fatalf("WriteI failed on iteration %d: %v", i, err)
		}

		if written != int(frames) {
			t.Fatalf("WriteI wrote %d frames, want %d", written, frames)
		}
	}

	duration := time.Since(start)

	// Since there is a consumer, the writes should not block for long.
	// The total time should be roughly the time it takes to play all the data.
	expectedFrames := uint32(writeCount) * config.PeriodSize
	expectedDurationMs := float64(expectedFrames) * 1000.0 / float64(config.Rate)
	durationMs := float64(duration.Milliseconds())

	// Allow a generous tolerance for timing assertions in a non-realtime environment.
	tolerance := 150.0 // ms
	if (durationMs-expectedDurationMs) > tolerance || (expectedDurationMs-durationMs) > tolerance {
		t.Logf("WriteI timing test: got %.2f ms, want ~%.2f ms. This can be flaky.", durationMs, expectedDurationMs)
	}
}

func testPcmGetDelay(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &kDefaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	// Delay should return an error or 0 if the stream is not running.
	delay, err := pcm.Delay()
	if err == nil {
		if delay < 0 {
			t.Errorf("expected non-negative delay, got %d", delay)
		}
	} else {
		// "file descriptor in bad state" (EBADF) is a valid error
		// if the stream hasn't started yet or the driver doesn't support this ioctl in the current state.
		t.Logf("pcm.Delay() returned an expected error on non-running stream: %v", err)
	}
}

func testPcmReadiTiming(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &kDefaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	playbackPcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &kDefaultConfig)
	require.NoError(t, err)
	defer playbackPcm.Close()

	// Link the streams to start them synchronously.
	err = playbackPcm.Link(pcm)
	if err != nil {
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}
	defer playbackPcm.Unlink()

	var wg sync.WaitGroup
	done := make(chan struct{})
	readyToWrite := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		playbackBuffer := make([]byte, alsa.PcmFramesToBytes(playbackPcm, playbackPcm.PeriodSize()))
		frames := playbackPcm.PeriodSize()

		close(readyToWrite) // Signal that the writer is ready

		for {
			select {
			case <-done:
				return
			default:
				_, err := playbackPcm.WriteI(playbackBuffer, frames)
				if err != nil {
					var errno syscall.Errno
					if errors.As(err, &errno) && (errors.Is(errno, syscall.EBADF) || errors.Is(errno, syscall.EPIPE)) {
						return
					}

					// Don't fail the test from inside the goroutine, just stop.
					return
				}
			}
		}
	}()

	defer func() {
		close(done)
		wg.Wait()
	}()

	// Wait for the producer to be ready before we start reading.
	<-readyToWrite

	const readCount = 20
	bufferSize := alsa.PcmFramesToBytes(pcm, kDefaultConfig.PeriodSize)
	buffer := make([]byte, bufferSize)
	frames := alsa.PcmBytesToFrames(pcm, bufferSize)

	start := time.Now()
	for i := 0; i < readCount; i++ {
		// The first call to ReadI will implicitly start both linked streams.
		read, err := pcm.ReadI(buffer, frames)
		if err != nil {
			t.Fatalf("ReadI failed on iteration %d: %v", i, err)
		}

		if read != int(frames) {
			t.Fatalf("ReadI read %d frames, want %d", read, frames)
		}
	}

	duration := time.Since(start)

	expectedDurationMs := float64(frames*readCount) * 1000.0 / float64(kDefaultConfig.Rate)
	durationMs := float64(duration.Milliseconds())

	tolerance := 150.0 // ms
	if (durationMs-expectedDurationMs) > tolerance || (expectedDurationMs-durationMs) > tolerance {
		t.Logf("ReadI timing test: got %.2f ms, want ~%.2f ms. This can be flaky.", durationMs, expectedDurationMs)
	}
}

func testPcmMmapWrite(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_MMAP, &kDefaultConfig)
	require.NoError(t, err, "PcmOpen with MMAP failed")
	defer pcm.Close()

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_MMAP, &kDefaultConfig)
	require.NoError(t, err, "PcmOpen capture (MMAP) failed")
	defer capturePcm.Close()

	// Link the streams to start them synchronously.
	err = pcm.Link(capturePcm)
	if err != nil {
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}
	defer pcm.Unlink()

	// For MMAP, streams must be explicitly prepared.
	require.NoError(t, pcm.Prepare(), "playback stream prepare failed")
	require.NoError(t, capturePcm.Prepare(), "capture stream prepare failed")

	var wg sync.WaitGroup
	done := make(chan struct{})
	readyToRead := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		readBuffer := make([]byte, alsa.PcmFramesToBytes(capturePcm, capturePcm.PeriodSize()))

		close(readyToRead) // Signal readiness before entering the read loop

		for {
			select {
			case <-done:
				return
			default:
				// This will block until the producer starts writing.
				_, err := capturePcm.MmapRead(readBuffer)
				if err != nil {
					var errno syscall.Errno
					if errors.As(err, &errno) && (errors.Is(errno, syscall.EBADF) || errors.Is(errno, syscall.EPIPE)) {
						return
					}

					// EAGAIN is okay, just continue the loop
					if errors.As(err, &errno) && errors.Is(errno, syscall.EAGAIN) {
						continue
					}

					// Don't fail the test from inside the goroutine, just stop.
					return
				}
			}
		}
	}()

	defer func() {
		close(done)
		wg.Wait()
	}()

	// Wait until the consumer is ready.
	<-readyToRead

	const writeCount = 20
	bufferSize := alsa.PcmFramesToBytes(pcm, kDefaultConfig.PeriodSize)
	buffer := make([]byte, bufferSize)

	start := time.Now()
	for i := 0; i < writeCount; i++ {
		// The first MmapWrite will auto-start the linked streams.
		written, err := pcm.MmapWrite(buffer)
		if err != nil {
			var errno syscall.Errno
			if errors.As(err, &errno) && errors.Is(errno, syscall.ENOTTY) {
				t.Skip("Skipping MMAP test: device does not support HWSYNC/SYNC_PTR (ENOTTY)")
			}

			// EAGAIN is okay, just continue the loop
			if errors.As(err, &errno) && errors.Is(errno, syscall.EAGAIN) {
				i-- // Retry this iteration

				continue
			}

			t.Fatalf("MmapWrite failed on iteration %d: %v", i, err)
		}

		if written != len(buffer) {
			t.Fatalf("MmapWrite wrote %d bytes, want %d", written, len(buffer))
		}
	}

	duration := time.Since(start)

	require.NoError(t, pcm.Stop())

	expectedFrames := uint32(writeCount) * kDefaultConfig.PeriodSize
	expectedDurationMs := float64(expectedFrames) * 1000.0 / float64(kDefaultConfig.Rate)
	durationMs := float64(duration.Milliseconds())

	tolerance := 250.0 // MMAP tests can have higher latency, increase tolerance
	if (durationMs-expectedDurationMs) > tolerance || (expectedDurationMs-durationMs) > tolerance {
		t.Logf("MmapWrite timing test: got %.2f ms, want ~%.2f ms. This can be flaky.", durationMs, expectedDurationMs)
	}
}

func testPcmParams(t *testing.T) {
	t.Run("GetAndFree", func(t *testing.T) {
		// Test non-existent device
		params, err := alsa.PcmParamsGet(1000, 1000, alsa.PCM_IN)
		require.Error(t, err, "expected error when getting params for non-existent device")

		// Test valid device
		params, err = alsa.PcmParamsGet(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT)
		require.NoError(t, err, "PcmParamsGet failed for valid device")
		require.NotNil(t, params, "PcmParamsGet returned nil params for valid device")
		params.Free() // Freeing should not panic
	})

	params, err := alsa.PcmParamsGet(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT)
	require.NoError(t, err)
	defer params.Free()

	t.Run("GetMask", func(t *testing.T) {
		_, err := params.Mask(alsa.PCM_PARAM_ACCESS)
		require.NoError(t, err, "Mask(PCM_PARAM_ACCESS) failed")

		// Test invalid param type
		_, err = params.Mask(alsa.PCM_PARAM_SAMPLE_BITS)
		require.Error(t, err, "expected error when getting mask for an interval parameter")
	})

	t.Run("GetRange", func(t *testing.T) {
		minVal, errMin := params.RangeMin(alsa.PCM_PARAM_RATE)
		maxVal, errMax := params.RangeMax(alsa.PCM_PARAM_RATE)

		require.NoError(t, errMin)
		require.NoError(t, errMax)

		if minVal == 0 || maxVal == 0 {
			t.Errorf("expected non-zero rate range, got min=%d, max=%d", minVal, maxVal)
		}

		if minVal > maxVal {
			t.Errorf("expected min rate <= max rate, got min=%d, max=%d", minVal, maxVal)
		}

		// Test invalid param type
		_, err = params.RangeMin(alsa.PCM_PARAM_ACCESS)
		require.Error(t, err, "expected error when getting range for a mask parameter")
	})

	t.Run("FormatIsSupported", func(t *testing.T) {
		// The loopback device should support standard formats.
		require.True(t, params.FormatIsSupported(alsa.PCM_FORMAT_S16_LE), "expected PCM_FORMAT_S16_LE to be supported")
		require.True(t, params.FormatIsSupported(alsa.PCM_FORMAT_S32_LE), "expected PCM_FORMAT_S32_LE to be supported")
	})

	t.Run("ToString", func(t *testing.T) {
		s := params.String()
		require.NotEmpty(t, s)
		require.NotEqual(t, "<nil>", s)
		require.Contains(t, s, "PCM device capabilities", "String() output missing expected header")
		require.Contains(t, s, "Access", "String() output missing 'Access' parameter")
		require.Contains(t, s, "Rate", "String() output missing 'Rate' parameter")
		t.Log("\n" + s)
	})
}

func testSetConfig(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, nil)
	require.NoError(t, err, "PcmOpen with nil config failed")
	defer pcm.Close()

	// Check that a default config was applied
	require.NotZero(t, pcm.Channels(), "expected non-zero channels with default config")

	// Try setting a new config
	newConfig := alsa.Config{
		Channels:    1,
		Rate:        16000,
		PeriodSize:  512,
		PeriodCount: 2,
		Format:      alsa.PCM_FORMAT_S16_LE,
	}

	require.NoError(t, pcm.SetConfig(&newConfig))

	// Verify the new config was applied
	finalConfig := pcm.Config()
	require.Equal(t, newConfig.Channels, finalConfig.Channels)
	require.Equal(t, newConfig.Rate, finalConfig.Rate)

	// Note: The driver might adjust period size/count, so we check the returned config.
	if finalConfig.PeriodSize != newConfig.PeriodSize {
		t.Logf("driver adjusted period size from %d to %d", newConfig.PeriodSize, finalConfig.PeriodSize)
	}
}

func testPcmLink(t *testing.T) {
	// Open two streams on the same subdevice of the loopback card.
	// This requires a card that supports multiple streams on one device, which snd-aloop does.
	pcm1, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &kDefaultConfig)
	require.NoError(t, err, "Failed to open playback stream")
	defer pcm1.Close()

	pcm2, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &kDefaultConfig)
	require.NoError(t, err, "Failed to open capture stream")
	defer pcm2.Close()

	if err := pcm1.Link(pcm2); err != nil {
		// Some kernels/ALSA versions might not support linking on all devices.
		// We log instead of failing hard.
		t.Logf("pcm1.Link(pcm2) failed: %v. This may not be supported on this system.", err)

		return
	}

	// If linking succeeds, unlinking should also succeed.
	require.NoError(t, pcm1.Unlink(), "pcm1.Unlink() failed")
}

func testPcmDrain(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &kDefaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &kDefaultConfig)
	require.NoError(t, err, "Could not open capture side of device")
	defer capturePcm.Close()

	err = pcm.Link(capturePcm)
	if err != nil {
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}
	defer pcm.Unlink()

	var wg sync.WaitGroup
	done := make(chan struct{})
	readyToRead := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()

		captureBuffer := make([]byte, alsa.PcmFramesToBytes(capturePcm, capturePcm.PeriodSize()))
		frames := capturePcm.PeriodSize()

		close(readyToRead)

		for {
			select {
			case <-done:
				return
			default:
				_, err := capturePcm.ReadI(captureBuffer, frames)
				if err != nil {
					var errno syscall.Errno
					if errors.As(err, &errno) && (errors.Is(errno, syscall.EBADF) || errors.Is(errno, syscall.EPIPE)) {
						return
					}

					return
				}
			}
		}
	}()

	defer func() {
		close(done)
		wg.Wait()
	}()

	<-readyToRead

	// Write some data to the buffer
	bufferSize := pcm.BufferSize()
	frames := bufferSize / 2 // Write half the buffer
	buffer := make([]byte, alsa.PcmFramesToBytes(pcm, frames))

	written, err := pcm.WriteI(buffer, frames)
	require.NoError(t, err)
	require.Equal(t, int(frames), written, "WriteI failed before drain")

	// Drain should block until the data is played.
	start := time.Now()
	require.NoError(t, pcm.Drain())
	duration := time.Since(start)

	expectedDurationMs := float64(frames) * 1000.0 / float64(pcm.Rate())
	durationMs := float64(duration.Milliseconds())

	// Allow a wide margin for error
	if durationMs < expectedDurationMs*0.8 {
		t.Errorf("Drain returned too quickly. Got %.2f ms, expected >~%.2f ms", durationMs, expectedDurationMs)
	}
}

func testPcmPause(t *testing.T) {
	config := kDefaultConfig
	// Set the start threshold to the full buffer size.
	// This makes the stream start only when the buffer is full,
	// preventing an immediate underrun due to scheduling delays in the writer goroutine.
	config.StartThreshold = config.PeriodSize * config.PeriodCount

	flags := alsa.PCM_OUT
	captureFlags := alsa.PCM_IN

	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), flags, &config)
	require.NoError(t, err)
	defer pcm.Close()

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), captureFlags, &config)
	require.NoError(t, err, "PcmOpen capture failed")
	defer capturePcm.Close()

	err = pcm.Link(capturePcm)
	if err != nil {
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}
	defer pcm.Unlink()

	// Prepare is good practice before starting I/O.
	require.NoError(t, pcm.Prepare(), "playback stream prepare failed")
	require.NoError(t, capturePcm.Prepare(), "capture stream prepare failed")

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	// This goroutine will continuously supply data to the playback device.
	go func() {
		defer wg.Done()
		writeBuf := make([]byte, alsa.PcmFramesToBytes(pcm, pcm.PeriodSize()))
		for {
			select {
			case <-done:
				return
			default:
				// This call will start the stream and will block if the buffer is full
				// or if the stream is paused.
				_, err := pcm.WriteI(writeBuf, pcm.PeriodSize())
				if err != nil {
					// An error (e.g. EPIPE on stop) will cause the goroutine to exit.
					return
				}
			}
		}
	}()

	// This goroutine will continuously consume data from the capture device.
	go func() {
		defer wg.Done()
		readBuf := make([]byte, alsa.PcmFramesToBytes(capturePcm, capturePcm.PeriodSize()))
		for {
			select {
			case <-done:
				return
			default:
				// This call will block until data is available or the stream is paused.
				_, err := capturePcm.ReadI(readBuf, capturePcm.PeriodSize())
				if err != nil {
					return
				}
			}
		}
	}()

	// Give the goroutines time to start the streams.
	time.Sleep(100 * time.Millisecond)

	state, _ := pcm.State()
	require.Equal(t, alsa.PCM_STATE_RUNNING, state, "PCM stream should be running before pause")

	// Pause the stream. The I/O goroutines should now block.
	err = pcm.Pause(true)
	if err != nil {
		var errno syscall.Errno
		if errors.As(err, &errno) && (errors.Is(errno, syscall.ENOTTY) || errors.Is(errno, syscall.ENOSYS)) {
			t.Skipf("Skipping pause test, PAUSE ioctl is not supported: %v", err)
			close(done)
			wg.Wait()
			return
		}
		require.NoError(t, err, "pcm.Pause(true) failed")
	}

	state, _ = pcm.State()
	require.Equal(t, alsa.PCM_STATE_PAUSED, state, "PCM stream state should be PAUSED")

	time.Sleep(50 * time.Millisecond)

	// Resume the stream. The I/O goroutines should unblock and continue.
	require.NoError(t, pcm.Pause(false), "pcm.Pause(false) failed")

	state, _ = pcm.State()
	require.Equal(t, alsa.PCM_STATE_RUNNING, state, "PCM stream state should be RUNNING after resume")

	// Let the goroutines run for another moment.
	time.Sleep(50 * time.Millisecond)

	// Clean up.
	close(done)
	wg.Wait()
}

func testPcmLoopback(t *testing.T) {
	testFormats := []struct {
		name   string
		format alsa.PcmFormat
	}{
		{"S16_LE", alsa.PCM_FORMAT_S16_LE},
		{"FLOAT_LE", alsa.PCM_FORMAT_FLOAT_LE},
	}

	// First, check if the loopback device supports the formats we want to test.
	playbackParams, err := alsa.PcmParamsGet(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT)
	require.NoError(t, err, "Failed to get params for loopback playback device")
	defer playbackParams.Free()

	captureParams, err := alsa.PcmParamsGet(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN)
	require.NoError(t, err, "Failed to get params for loopback capture device")
	defer captureParams.Free()

	for _, tf := range testFormats {
		t.Run(tf.name, func(t *testing.T) {
			if !playbackParams.FormatIsSupported(tf.format) {
				t.Skipf("Playback device does not support format %s, skipping", alsa.PcmParamFormatNames[tf.format])
			}

			if !captureParams.FormatIsSupported(tf.format) {
				t.Skipf("Capture device does not support format %s, skipping", alsa.PcmParamFormatNames[tf.format])
			}

			config := kDefaultConfig
			config.Format = tf.format

			// Open playback stream
			pcmOut, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &config)
			require.NoError(t, err, "PcmOpen(playback) failed")
			defer pcmOut.Close()

			// Open capture stream
			pcmIn, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &config)
			require.NoError(t, err, "PcmOpen(capture) failed")
			defer pcmIn.Close()

			// Link them for synchronous start
			err = pcmOut.Link(pcmIn)
			if err != nil {
				t.Skipf("Failed to link PCM streams, skipping test: %v", err)
			}
			defer pcmOut.Unlink()

			// Explicitly prepare before starting goroutines for stability.
			require.NoError(t, pcmOut.Prepare(), "playback stream prepare failed")
			require.NoError(t, pcmIn.Prepare(), "capture stream prepare failed")

			var wg sync.WaitGroup
			done := make(chan struct{})

			// Signal when playback has actually written at least one period.
			playbackStarted := make(chan struct{})

			// Use thread-safe error holders to report errors from goroutines.
			var captureErr, playbackErr error
			var captureErrMtx, playbackErrMtx sync.Mutex

			setCaptureErr := func(e error) {
				captureErrMtx.Lock()
				defer captureErrMtx.Unlock()
				if captureErr == nil {
					captureErr = e
				}
			}

			setPlaybackErr := func(e error) {
				playbackErrMtx.Lock()
				defer playbackErrMtx.Unlock()
				if playbackErr == nil {
					playbackErr = e
				}
			}

			// Capture goroutine
			wg.Add(1)
			go func() {
				defer wg.Done()

				bufferSize := alsa.PcmFramesToBytes(pcmIn, pcmIn.PeriodSize())
				frames := pcmIn.PeriodSize()
				buffer := make([]byte, bufferSize)

				started := false
				counter := 0

				for {
					select {
					case <-done:
						return
					default:
						read, err := pcmIn.ReadI(buffer, frames)
						if err != nil {
							var errno syscall.Errno
							if errors.As(err, &errno) && (errors.Is(errno, syscall.EBADF) || errors.Is(errno, syscall.EPIPE)) {
								return
							}

							setCaptureErr(fmt.Errorf("the ReadI failed on iteration %d: %w", counter, err))

							return
						}

						if read != int(frames) {
							setCaptureErr(fmt.Errorf("short read on iteration %d: got %d frames, want %d", counter, read, frames))

							return
						}

						// Don't evaluate energy until playback has started writing.
						if !started {
							select {
							case <-playbackStarted:
								started = true
							default:
								continue
							}
						}

						// Let the buffer fill with the sine wave before checking energy.
						if counter >= 5 {
							e := energy(buffer, config.Format)
							if e <= 0.0 {
								setCaptureErr(fmt.Errorf("captured signal has no energy (e=%.2f) on iteration %d", e, counter))

								return
							}
						}

						counter++
					}
				}
			}()

			// Playback goroutine
			wg.Add(1)
			go func() {
				defer wg.Done()

				generator := newSineToneGenerator(config, 1000, 0) // 1kHz tone, 0dB
				bufferSize := alsa.PcmFramesToBytes(pcmOut, pcmOut.PeriodSize())
				frames := pcmOut.PeriodSize()
				buffer := make([]byte, bufferSize)
				counter := 0
				signaled := false

				for {
					select {
					case <-done:
						return
					default:
						generator.Read(buffer)
						written, err := pcmOut.WriteI(buffer, frames)
						if err != nil {
							var errno syscall.Errno
							if errors.As(err, &errno) && (errors.Is(errno, syscall.EBADF) || errors.Is(errno, syscall.EPIPE)) {
								return
							}

							setPlaybackErr(fmt.Errorf("the WriteI failed on iteration %d: %w", counter, err))

							return
						}
						if written != int(frames) {
							setPlaybackErr(fmt.Errorf("short write on iteration %d: got %d frames, want %d", counter, written, frames))

							return
						}

						if !signaled {
							close(playbackStarted)
							signaled = true
						}

						counter++
					}
				}
			}()

			// Run for a short period to allow playback and capture.
			time.Sleep(500 * time.Millisecond)
			close(done)
			wg.Wait()

			// Check for errors that occurred in the goroutines.
			require.NoError(t, captureErr, "Capture goroutine failed")
			require.NoError(t, playbackErr, "Playback goroutine failed")
		})
	}
}

func testPcmMmapLoopback(t *testing.T) {
	config := kDefaultConfig
	config.Format = alsa.PCM_FORMAT_S16_LE

	// Ensure the loopback device supports the required format and MMAP access.
	playbackParams, err := alsa.PcmParamsGet(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT)
	require.NoError(t, err)
	defer playbackParams.Free()
	if !playbackParams.FormatIsSupported(config.Format) {
		t.Skipf("Playback device does not support format %s", alsa.PcmParamFormatNames[config.Format])
	}

	accessMask, err := playbackParams.Mask(alsa.PCM_PARAM_ACCESS)
	require.NoError(t, err)
	if !accessMask.Test(uint(0)) { // 0 == SNDRV_PCM_ACCESS_MMAP_INTERLEAVED
		t.Skip("Playback device does not support MMAP access")
	}

	// Open playback stream with MMAP
	pcmOut, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_MMAP, &config)
	require.NoError(t, err, "PcmOpen(playback, mmap) failed")
	defer pcmOut.Close()

	// Open capture stream with MMAP
	pcmIn, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_MMAP, &config)
	require.NoError(t, err, "PcmOpen(capture, mmap) failed")
	defer pcmIn.Close()

	// Link them for synchronous start
	err = pcmOut.Link(pcmIn)
	if err != nil {
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}
	defer pcmOut.Unlink()

	// For MMAP, streams must be explicitly prepared.
	require.NoError(t, pcmOut.Prepare(), "playback stream prepare failed")
	require.NoError(t, pcmIn.Prepare(), "capture stream prepare failed")

	var wg sync.WaitGroup
	done := make(chan struct{})

	// Signal when playback has actually written at least one period.
	playbackStarted := make(chan struct{})

	var captureErr, playbackErr error
	var captureErrMtx, playbackErrMtx sync.Mutex

	setCaptureErr := func(e error) {
		captureErrMtx.Lock()
		defer captureErrMtx.Unlock()
		if captureErr == nil {
			captureErr = e
		}
	}

	setPlaybackErr := func(e error) {
		playbackErrMtx.Lock()
		defer playbackErrMtx.Unlock()
		if playbackErr == nil {
			playbackErr = e
		}
	}

	// Capture goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()

		// Use a period-sized buffer for MMAP read as well, to match the write side's granularity.
		buffer := make([]byte, alsa.PcmFramesToBytes(pcmIn, pcmIn.PeriodSize()))
		counter := 0
		started := false

		for {
			select {
			case <-done:
				return
			default:
				read, err := pcmIn.MmapRead(buffer)
				if err != nil {
					var errno syscall.Errno
					if errors.As(err, &errno) && (errors.Is(errno, syscall.EBADF) || errors.Is(errno, syscall.EPIPE) || errors.Is(errno, syscall.EAGAIN)) {
						// For EAGAIN in a loopback test, we expect data soon, so continue unless done.
						if errors.Is(err, syscall.EAGAIN) {
							continue
						}

						return
					}

					setCaptureErr(fmt.Errorf("MmapRead failed on iteration %d: %w", counter, err))

					return
				}
				if read == 0 {
					continue
				}

				// Don't evaluate energy until playback has started writing.
				if !started {
					select {
					case <-playbackStarted:
						started = true
					default:
						continue
					}
				}

				// Let the buffer fill with the sine wave before checking energy.
				if counter >= 5 {
					e := energy(buffer[:read], config.Format)
					if e <= 0.0 {
						setCaptureErr(fmt.Errorf("captured signal has no energy (e=%.2f) on iteration %d", e, counter))

						return
					}
				}

				counter++
			}
		}
	}()

	// Playback goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()

		generator := newSineToneGenerator(config, 1000, 0) // 1kHz tone, 0dB
		buffer := make([]byte, alsa.PcmFramesToBytes(pcmOut, pcmOut.PeriodSize()))
		counter := 0
		signaled := false

		for {
			select {
			case <-done:
				return
			default:
				generator.Read(buffer)
				written, err := pcmOut.MmapWrite(buffer)
				if err != nil {
					var errno syscall.Errno
					if errors.As(err, &errno) && (errors.Is(errno, syscall.EBADF) || errors.Is(errno, syscall.EPIPE) || errors.Is(errno, syscall.EAGAIN)) {
						// EAGAIN might happen if the buffer is momentarily full.
						if errors.Is(err, syscall.EAGAIN) {
							continue
						}

						return
					}

					setPlaybackErr(fmt.Errorf("MmapWrite failed on iteration %d: %w", counter, err))

					return
				}
				if written != len(buffer) {
					setPlaybackErr(fmt.Errorf("short mmap write on iteration %d: got %d, want %d", counter, written, len(buffer)))

					return
				}

				if !signaled {
					close(playbackStarted)
					signaled = true
				}

				counter++
			}
		}
	}()

	// Run for a short period to allow playback and capture.
	time.Sleep(500 * time.Millisecond)
	close(done)
	wg.Wait()
	pcmOut.Stop()

	// Check for errors that occurred in the goroutines.
	require.NoError(t, captureErr, "Capture goroutine failed")
	require.NoError(t, playbackErr, "Playback goroutine failed")
}

// sineToneGenerator is a helper for audio tests that generates a sine wave.
type sineToneGenerator struct {
	phases []float64
	gain   float64
	step   float64
	format alsa.PcmFormat
	numCh  uint32
}

// newSineToneGenerator creates a sine wave generator.
// Frequency is in Hz, levelDB is the gain in decibels (0 for a full scale).
func newSineToneGenerator(config alsa.Config, frequency float64, levelDB float64) *sineToneGenerator {
	g := &sineToneGenerator{
		format: config.Format,
		numCh:  config.Channels,
		// The phase increment per frame.
		step:   frequency * 2 * math.Pi / float64(config.Rate),
		gain:   math.Pow(10, levelDB/20.0),
		phases: make([]float64, config.Channels),
	}

	// Create a phase offset between channels for stereo signals.
	phaseStep := 0.0
	if config.Channels > 1 {
		phaseStep = math.Pi / 2 / float64(config.Channels-1)
	}

	for i := uint32(0); i < config.Channels; i++ {
		g.phases[i] = float64(i) * phaseStep
	}

	return g
}

// Read fills the buffer with sine wave data.
func (g *sineToneGenerator) Read(buffer []byte) {
	bytesPerSample := alsa.PcmFormatToBits(g.format) / 8
	frameSize := bytesPerSample * g.numCh
	numFrames := len(buffer) / int(frameSize)

	for f := 0; f < numFrames; f++ { // Loop over frames
		for c := 0; c < int(g.numCh); c++ { // Loop over channels in the current frame
			sine := math.Sin(g.phases[c]) * g.gain
			offset := (f*int(g.numCh) + c) * int(bytesPerSample)

			switch g.format {
			case alsa.PCM_FORMAT_S16_LE:
				var sample int16
				if sine >= 1.0 {
					sample = 32767
				} else if sine <= -1.0 {
					sample = -32768
				} else {
					sample = int16(sine * 32767)
				}

				binary.LittleEndian.PutUint16(buffer[offset:], uint16(sample))
			case alsa.PCM_FORMAT_FLOAT_LE:
				var sample float32
				if sine >= 1.0 {
					sample = 1.0
				} else if sine <= -1.0 {
					sample = -1.0
				} else {
					sample = float32(sine)
				}

				binary.LittleEndian.PutUint32(buffer[offset:], math.Float32bits(sample))
			}
		}

		// Increment phase for all channels after each frame.
		for c := 0; c < int(g.numCh); c++ {
			g.phases[c] += g.step
		}
	}
}

// energy calculates the signal energy (sum of squares of samples) in a buffer.
func energy(buffer []byte, format alsa.PcmFormat) float64 {
	sum := 0.0

	switch format {
	case alsa.PCM_FORMAT_S16_LE:
		samples := unsafe.Slice((*int16)(unsafe.Pointer(&buffer[0])), len(buffer)/2)
		for _, sample := range samples {
			val := float64(sample)
			sum += val * val
		}
	case alsa.PCM_FORMAT_FLOAT_LE:
		samples := unsafe.Slice((*float32)(unsafe.Pointer(&buffer[0])), len(buffer)/4)
		for _, sample := range samples {
			val := float64(sample)
			sum += val * val
		}
	}

	return sum
}
