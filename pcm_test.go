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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/gen2brain/alsa"
)

// To run these tests, the 'snd-aloop' kernel module must be loaded:
//
// sudo modprobe snd-aloop
//
// This creates a virtual loopback sound card that allows testing playback and capture.

var (
	defaultConfig = alsa.Config{
		Channels:    2,
		Rate:        48000,
		PeriodSize:  1024,
		PeriodCount: 4,
		Format:      alsa.SNDRV_PCM_FORMAT_S16_LE,
	}
)

func TestPcmFormatToBits(t *testing.T) {
	testCases := map[alsa.PcmFormat]uint32{
		alsa.SNDRV_PCM_FORMAT_INVALID:    0,
		alsa.SNDRV_PCM_FORMAT_S16_LE:     16,
		alsa.SNDRV_PCM_FORMAT_S32_LE:     32,
		alsa.SNDRV_PCM_FORMAT_S8:         8,
		alsa.SNDRV_PCM_FORMAT_S24_LE:     32, // 24-bit stored in 32-bit container
		alsa.SNDRV_PCM_FORMAT_S24_3LE:    24, // Packed 24-bit
		alsa.SNDRV_PCM_FORMAT_S16_BE:     16,
		alsa.SNDRV_PCM_FORMAT_S24_BE:     32,
		alsa.SNDRV_PCM_FORMAT_S24_3BE:    24,
		alsa.SNDRV_PCM_FORMAT_S32_BE:     32,
		alsa.SNDRV_PCM_FORMAT_FLOAT_LE:   32,
		alsa.SNDRV_PCM_FORMAT_FLOAT_BE:   32,
		alsa.SNDRV_PCM_FORMAT_FLOAT64_LE: 64,
		alsa.SNDRV_PCM_FORMAT_FLOAT64_BE: 64,
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

func TestPcmInvalidBuffers(t *testing.T) {
	t.Parallel()
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
	require.NoError(t, err, "Failed to open PCM for invalid buffer tests")
	defer pcm.Close()

	// Test Write with various invalid inputs
	_, err = pcm.Write(nil)
	assert.Error(t, err, "Write with nil buffer should fail")

	_, err = pcm.Write(123)
	assert.Error(t, err, "Write with non-slice buffer should fail")

	var unsupportedSlice = []int64{1, 1, 1, 1}
	_, err = pcm.Write(unsupportedSlice)
	assert.Error(t, err, "Write with unsupported slice type should fail")

	var emptySlice []int16
	_, err = pcm.Write(emptySlice)
	assert.NoError(t, err, "Write with empty slice should succeed")
}

// TestPcmHardware runs all hardware-related tests sequentially.
func TestPcmHardware(t *testing.T) {
	t.Run("PcmOpenAndClose", testPcmOpenAndClose)
	t.Run("PcmOpenByName", testPcmOpenByName)
	t.Run("PcmPlaybackStartup", testPcmPlaybackStartup)
	t.Run("PcmGetters", testPcmGetters)
	t.Run("PcmFramesBytesConvert", testPcmFramesBytesConvert)
	t.Run("PcmWriteFailsOnCapture", testPcmWriteFailsOnCapture)
	t.Run("PcmReadFailsOnPlayback", testPcmReadFailsOnPlayback)
	t.Run("PcmWriteTiming", testPcmWriteTiming)
	t.Run("PcmGetDelay", testPcmGetDelay)
	t.Run("PcmReadTiming", testPcmReadTiming)
	t.Run("PcmReadWriteSample", testPcmReadWriteSimple)
	t.Run("PcmMmapWrite", testPcmMmapWrite)
	t.Run("PcmMmapWriteTiming", testPcmMmapWriteTiming)
	t.Run("PcmMmapRead", testPcmMmapRead)
	t.Run("PcmMmapReadTiming", testPcmMmapReadTiming)
	t.Run("PcmState", testPcmState)
	t.Run("PcmStop", testPcmStop)
	t.Run("PcmWait", testPcmWait)
	t.Run("PcmParams", testPcmParams)
	t.Run("SetConfig", testSetConfig)
	t.Run("PcmLink", testPcmLink)
	t.Run("PcmDrain", testPcmDrain)
	t.Run("PcmPause", testPcmPause)
	t.Run("PcmLoopback", testPcmLoopback)
	t.Run("PcmNonBlocking", testPcmNonBlocking)
	t.Run("PcmMmapLoopback", testPcmMmapLoopback)
	t.Run("PcmMmapNonBlocking", testPcmMmapNonBlocking)
}

func testPcmOpenAndClose(t *testing.T) {
	// Test opening a non-existent device
	pcm, err := alsa.PcmOpen(1000, 1000, alsa.PCM_OUT, &defaultConfig)
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
			pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), tc.flags, &defaultConfig)
			if err != nil {
				// The TTSTAMP ioctl for PCM_MONOTONIC is not supported by all kernels/devices.
				if tc.flags&alsa.PCM_MONOTONIC != 0 {
					if errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.EINVAL) {
						t.Fatalf("Skipping monotonic test, TTSTAMP ioctl not supported by device: %v", err)

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
	pcm, err := alsa.PcmOpenByName(name, alsa.PCM_OUT, &defaultConfig)
	require.NoError(t, err, "PcmOpenByName failed for a valid name")
	require.NotNil(t, pcm)
	require.True(t, pcm.IsReady())
	pcm.Close()

	// Test invalid names
	_, err = alsa.PcmOpenByName("invalid_name", alsa.PCM_OUT, &defaultConfig)
	require.Error(t, err, "PcmOpenByName should fail for a name without 'hw:' prefix")

	_, err = alsa.PcmOpenByName("hw:foo,bar", alsa.PCM_OUT, &defaultConfig)
	require.Error(t, err, "PcmOpenByName should fail for non-numeric card/device")

	_, err = alsa.PcmOpenByName("hw:0", alsa.PCM_OUT, &defaultConfig)
	require.Error(t, err, "PcmOpenByName should fail for incomplete name")
}

func testPcmState(t *testing.T) {
	t.Run("StateNonMmap", func(t *testing.T) {
		pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
		require.NoError(t, err)
		defer pcm.Close()

		// Initial cached state is SETUP. State should report this (or OPEN, depending on the driver).
		initialState := pcm.State()
		assert.Contains(t, []alsa.PcmState{alsa.SNDRV_PCM_STATE_OPEN, alsa.SNDRV_PCM_STATE_SETUP}, initialState)

		require.NoError(t, pcm.Prepare())
		// After Prepare, cached state is PREPARED. Kernel should report the same.
		assert.Equal(t, alsa.SNDRV_PCM_STATE_PREPARED, pcm.State(), "State should be PREPARED after prepare")

		// Starting an empty stream is expected to cause an immediate underrun (EPIPE).
		// We call Start() but don't check the error, as EPIPE is the correct behavior here.
		_ = pcm.Start()

		// On a playback-only stream with no data, this will immediately underrun.
		// Give it a moment for the state to be detectable.
		time.Sleep(50 * time.Millisecond)

		// State should now report XRUN, or PREPARED if the driver auto-stops on underrun (due to stop_threshold).
		finalState := pcm.State()
		assert.Contains(t, []alsa.PcmState{alsa.SNDRV_PCM_STATE_XRUN, alsa.SNDRV_PCM_STATE_PREPARED}, finalState,
			"State should be XRUN or PREPARED after starting an empty stream")
	})

	t.Run("StateMmap", func(t *testing.T) {
		pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_MMAP, &defaultConfig)
		require.NoError(t, err)
		defer pcm.Close()

		// For MMAP streams, the status struct is mmapped, so the State should be accurate.
		// After setup, the state in the kernel's mmap region should be SETUP.
		initialState := pcm.State()
		assert.Contains(t, []alsa.PcmState{alsa.SNDRV_PCM_STATE_OPEN, alsa.SNDRV_PCM_STATE_SETUP}, initialState)

		require.NoError(t, pcm.Prepare())
		assert.Equal(t, alsa.SNDRV_PCM_STATE_PREPARED, pcm.State(), "State should be PREPARED after prepare")

		// Starting an empty stream is expected to cause an immediate underrun (EPIPE).
		// We call Start() but don't check the error, as EPIPE is the correct behavior here.
		_ = pcm.Start()

		time.Sleep(50 * time.Millisecond) // Allow time for underrun

		// State should now report XRUN, or PREPARED if the driver auto-stops on underrun.
		finalState := pcm.State()
		assert.Contains(t, []alsa.PcmState{alsa.SNDRV_PCM_STATE_XRUN, alsa.SNDRV_PCM_STATE_PREPARED}, finalState, "State should be XRUN or PREPARED after starting an empty mmap stream")
	})
}

func testPcmStop(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &defaultConfig)
	require.NoError(t, err)
	defer capturePcm.Close()

	err = pcm.Link(capturePcm)
	if err != nil {
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}
	defer pcm.Unlink()

	// Explicitly prepare streams for predictable behavior.
	require.NoError(t, pcm.Prepare())
	require.NoError(t, capturePcm.Prepare())

	var wg sync.WaitGroup
	// Add synchronization to ensure the reader is ready.
	readyToRead := make(chan struct{})
	wg.Add(1)

	go func() {
		defer wg.Done()
		buffer := make([]byte, alsa.PcmFramesToBytes(capturePcm, capturePcm.PeriodSize()))

		close(readyToRead) // Signal readiness

		for {
			_, err := capturePcm.Read(buffer)
			if err != nil {
				// An error (EPIPE/EBADF) is expected when the stream is stopped.
				// This is the signal for the goroutine to exit.
				return
			}
		}
	}()

	// Wait for the reader to be ready.
	<-readyToRead

	// Write some data to start the stream. We fill the entire buffer to ensure
	// it doesn't underrun immediately while the capture side reads.
	buffer := make([]byte, alsa.PcmFramesToBytes(pcm, pcm.BufferSize()))
	// Write on a prepared stream starts both linked streams.
	_, err = pcm.Write(buffer)
	require.NoError(t, err)

	// Use polling to wait for the RUNNING state.
	var state alsa.PcmState
	// Wait up to 1 second (100 * 10ms)
	for i := 0; i < 100; i++ {
		state = pcm.State()
		if state == alsa.SNDRV_PCM_STATE_RUNNING {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	require.Equal(t, alsa.SNDRV_PCM_STATE_RUNNING, state, "Stream should be in RUNNING state after writing")

	// Now, stop the stream. This tests the Stop() function.
	// This call will also unblock the capturePcm.Read() in the goroutine.
	err = pcm.Stop()
	require.NoError(t, err, "pcm.Stop() failed")

	// Poll for the state change after stopping.
	for i := 0; i < 50; i++ {
		state = pcm.State()
		// Stop() (DROP) typically brings the state back to SETUP.
		if state == alsa.SNDRV_PCM_STATE_SETUP {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	require.Equal(t, alsa.SNDRV_PCM_STATE_SETUP, state, "Stream should be in SETUP state after stopping")

	// Wait for the reader goroutine to finish. It will exit because pcm.Stop() caused its blocking Read() call to return an error.
	wg.Wait()
}

func testPcmWait(t *testing.T) {
	t.Run("Timeout", func(t *testing.T) {
		// Use a capture stream (PCM_IN) for the timeout test.
		pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &defaultConfig)
		require.NoError(t, err)
		defer pcm.Close()

		// A stream must be prepared before a meaningful wait for I/O can occur.
		require.NoError(t, pcm.Prepare())

		// On a prepared but non-running capture stream, Wait should time out as no data is available.
		ready, err := pcm.Wait(10)
		assert.NoError(t, err)
		assert.False(t, ready, "Wait should time out and return false on an empty capture stream")
	})

	t.Run("Ready", func(t *testing.T) {
		pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
		require.NoError(t, err)
		defer pcm.Close()

		// A prepared playback stream should be immediately ready for output.
		require.NoError(t, pcm.Prepare())

		ready, err := pcm.Wait(1000)
		assert.NoError(t, err)
		assert.True(t, ready, "Playback stream should be ready for writing")
	})
}

func testPcmPlaybackStartup(t *testing.T) {
	// Configure the stream to start only after 2 periods of data are available.
	config := defaultConfig
	config.StartThreshold = config.PeriodSize * 2

	// Ensure the stop threshold is large enough not to interfere with the test.
	config.StopThreshold = config.PeriodSize * config.PeriodCount

	// Open playback and capture devices.
	// Handle Close/Stop/Wait in a centralized defer block to ensure the correct order.
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &config)
	require.NoError(t, err)

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &config)
	require.NoError(t, err)

	// Link streams for synchronous operation and prepare them for I/O.
	err = pcm.Link(capturePcm)
	linkSucceeded := err == nil
	if !linkSucceeded {
		pcm.Close()
		capturePcm.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	require.NoError(t, pcm.Prepare())
	require.NoError(t, capturePcm.Prepare())

	// Start a capture goroutine to drain the loopback buffer, allowing the playback stream to run without blocking.
	var wg sync.WaitGroup
	done := make(chan struct{})
	// Add synchronization channel.
	readyToRead := make(chan struct{})
	wg.Add(1)

	go func() {
		defer wg.Done()
		buffer := make([]byte, alsa.PcmFramesToBytes(capturePcm, capturePcm.PeriodSize()))

		close(readyToRead) // Signal readiness

		for {
			// Check done before potentially blocking read.
			select {
			case <-done:
				return
			default:
				// Proceed to blocking read.
			}

			// This Read relies on Stop() being called to unblock during shutdown.
			_, err := capturePcm.Read(buffer)
			if err != nil {
				// Exit on error (EPIPE/EBADF), which is expected when the stream is stopped/closed.
				return
			}
		}
	}()

	// Centralized cleanup with correct order (Stop before Wait).
	defer func() {
		close(done)
		// Stop the streams explicitly to unblock the Read goroutine.
		_ = pcm.Stop()
		_ = capturePcm.Stop()
		wg.Wait() // Wait for the goroutine to exit cleanly.

		// Unlink and close.
		if linkSucceeded {
			_ = pcm.Unlink()
		}

		_ = pcm.Close()
		_ = capturePcm.Close()
	}()

	// Wait for the reader to be ready.
	<-readyToRead

	// Verify the initial state is PREPARED.
	require.Equal(t, alsa.SNDRV_PCM_STATE_PREPARED, pcm.State(), "Initial state should be PREPARED")

	// Write one period of data (less than the start threshold).
	buffer := make([]byte, alsa.PcmFramesToBytes(pcm, pcm.PeriodSize()))
	written, err := pcm.Write(buffer)
	require.NoError(t, err)
	require.Equal(t, int(pcm.PeriodSize()), written)

	// Verify state is still PREPARED.
	require.Equal(t, alsa.SNDRV_PCM_STATE_PREPARED, pcm.State(), "State should remain PREPARED after writing less than threshold")

	// Write a second period of data to meet the start threshold.
	written, err = pcm.Write(buffer)

	require.NoError(t, err)
	require.Equal(t, int(pcm.PeriodSize()), written)

	// Poll for the state change.
	var finalState alsa.PcmState
	// Allow up to 1 second (100 * 10ms) for the stream to start.
	for i := 0; i < 100; i++ {
		finalState = pcm.State()
		if finalState == alsa.SNDRV_PCM_STATE_RUNNING {
			break
		}

		// If XRUN happens, the test failed.
		if finalState == alsa.SNDRV_PCM_STATE_XRUN {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	require.Equal(t, alsa.SNDRV_PCM_STATE_RUNNING, finalState, "State should be RUNNING after start threshold is met")
}

func testPcmGetters(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	if pcm.Fd() == ^uintptr(0) {
		t.Error("expected a valid file descriptor")
	}

	require.Equal(t, alsa.PCM_OUT, pcm.Flags())
	require.Equal(t, defaultConfig.PeriodCount, pcm.PeriodCount())

	// The loopback playback device is opened, which is index 0. Its subdevice number should also be 0.
	require.Equal(t, uint32(loopbackPlaybackDevice), pcm.Subdevice())
	require.Equal(t, 0, pcm.Xruns(), "Xruns should be 0 on a newly opened stream")

	require.Equal(t, defaultConfig.Channels, pcm.Channels())
	require.Equal(t, defaultConfig.Rate, pcm.Rate())
	require.Equal(t, defaultConfig.Format, pcm.Format())
	require.Equal(t, defaultConfig.PeriodSize*defaultConfig.PeriodCount, pcm.BufferSize())

	// Test PeriodTime calculation
	expectedNs := (1e9 * float64(defaultConfig.PeriodSize)) / float64(defaultConfig.Rate)
	expectedDuration := time.Duration(expectedNs)
	require.Equal(t, expectedDuration, pcm.PeriodTime(), "PeriodTime should be calculated correctly")
}

func testPcmFramesBytesConvert(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	bytesPerFrame := alsa.PcmFormatToBits(defaultConfig.Format) / 8 * defaultConfig.Channels
	require.Equal(t, bytesPerFrame, alsa.PcmFramesToBytes(pcm, 1))
	require.Equal(t, uint32(1), alsa.PcmBytesToFrames(pcm, bytesPerFrame))
}

func testPcmWriteFailsOnCapture(t *testing.T) {
	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &defaultConfig)
	require.NoError(t, err)
	defer capturePcm.Close()

	buffer := make([]byte, 128)
	_, err = capturePcm.Write(buffer)
	require.Error(t, err, "expected error when calling Write on a capture stream")
}

func testPcmReadFailsOnPlayback(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	buffer := make([]byte, 128)
	_, err = pcm.Read(buffer)

	require.Error(t, err, "expected error when calling Read on a playback stream")
	require.Contains(t, err.Error(), "cannot read from a playback device")
}

func testPcmWriteTiming(t *testing.T) {
	config := defaultConfig
	// Handle Close/Stop/Wait in a centralized defer block.
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &config)
	require.NoError(t, err)

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &config)
	require.NoError(t, err, "Could not open capture side of device")

	// Link the streams to start them synchronously.
	err = pcm.Link(capturePcm)
	linkSucceeded := err == nil
	if !linkSucceeded {
		pcm.Close()
		capturePcm.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	// Prepare explicitly.
	require.NoError(t, pcm.Prepare())
	require.NoError(t, capturePcm.Prepare())

	var wg sync.WaitGroup
	done := make(chan struct{})
	readyToRead := make(chan struct{})

	// Thread-safe error reporting from goroutine.
	var captureErr error
	var captureErrMtx sync.Mutex
	setCaptureErr := func(e error) {
		// Only record errors if we are NOT shutting down.
		select {
		case <-done:
			return
		default:
		}

		captureErrMtx.Lock()
		defer captureErrMtx.Unlock()

		if captureErr == nil {
			captureErr = e
		}
	}

	wg.Add(1)

	go func() {
		defer wg.Done()
		captureBuffer := make([]byte, alsa.PcmFramesToBytes(capturePcm, capturePcm.PeriodSize()))

		close(readyToRead) // Signal that the reader is about to start its loop

		for {
			select {
			case <-done:
				return
			default:
				// Continue to Read.
			}

			// This will block. Relies on Stop() for shutdown.
			_, err := capturePcm.Read(captureBuffer)
			if err != nil {
				// EBADF, EPIPE, or EBADFD are expected errors when the stream is
				// stopped during shutdown. This is the signal to exit the goroutine.
				if errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
					return
				}

				// Report any other unexpected errors.
				setCaptureErr(fmt.Errorf("capturePcm.Read failed: %w", err))
				return
			}
		}
	}()

	// Centralized cleanup (Stop before Wait).
	defer func() {
		close(done)
		// Stop streams to unblock the goroutine before waiting.
		_ = pcm.Stop()
		_ = capturePcm.Stop()
		wg.Wait()

		// Check errors after wait.
		captureErrMtx.Lock()
		if captureErr != nil {
			t.Logf("Capture goroutine reported an error: %v", captureErr)
		}
		captureErrMtx.Unlock()

		// Cleanup resources.
		if linkSucceeded {
			_ = pcm.Unlink()
		}
		_ = pcm.Close()
		_ = capturePcm.Close()
	}()

	// Wait until the consumer is ready to read.
	<-readyToRead

	const writeCount = 20
	bufferSize := alsa.PcmFramesToBytes(pcm, config.PeriodSize)
	buffer := make([]byte, bufferSize)
	frames := alsa.PcmBytesToFrames(pcm, bufferSize)

	start := time.Now()
	writesCompleted := 0
	for i := 0; i < writeCount; i++ {
		// Check if peer failed before writing.
		captureErrMtx.Lock()
		if captureErr != nil {
			captureErrMtx.Unlock()
			t.Fatalf("Aborting write loop because capture goroutine failed: %v", captureErr)
		}
		captureErrMtx.Unlock()

		// The first call to Write will implicitly start both linked streams (as they are prepared).
		written, err := pcm.Write(buffer)
		if err != nil {
			// Allow EPIPE/EBADFD here as it can happen during concurrent tests if the stream stops unexpectedly (e.g. underrun).
			if errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
				t.Logf("Write encountered expected error (EPIPE/EBADFD) on iteration %d, stopping write loop: %v", i, err)

				break
			}

			t.Fatalf("Write failed on iteration %d: %v", i, err)
		}

		if written != int(frames) {
			t.Fatalf("Write wrote %d frames, want %d", written, frames)
		}

		writesCompleted++
	}

	// Check if peer failed before Drain.
	captureErrMtx.Lock()
	if captureErr != nil {
		captureErrMtx.Unlock()
		// If the capture side failed, Drain will likely hang or return EPIPE.
		t.Logf("Skipping Drain because capture goroutine failed: %v", captureErr)
	} else {
		captureErrMtx.Unlock()
		// After writing all data, call Drain to wait for playback to complete.
		err = pcm.Drain()
		// An xrun (EPIPE) can occur in Drain if the consumer stops unexpectedly or an underrun happened earlier. This is acceptable.
		if err != nil && !errors.Is(err, syscall.EPIPE) {
			require.NoError(t, err, "Drain failed after writing")
		}
	}

	duration := time.Since(start)

	// The total time should be roughly the time it takes to play the data actually written.
	expectedFrames := uint32(writesCompleted) * config.PeriodSize
	expectedDurationMs := math.Ceil(float64(expectedFrames) * 1000.0 / float64(config.Rate))
	durationMs := float64(duration.Milliseconds())

	// Allow a generous tolerance for timing assertions.
	tolerance := 200.0
	if (durationMs-expectedDurationMs) > tolerance || (expectedDurationMs-durationMs) > tolerance {
		t.Logf("Write+Drain timing test: got %.2f ms, want ~%.2f ms. This can be flaky.", durationMs, expectedDurationMs)
	}
}

func testPcmGetDelay(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &defaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	// Delay should return an error or 0 if the stream is not running.
	delay, err := pcm.Delay()
	if err == nil {
		if delay < 0 {
			t.Errorf("expected non-negative delay, got %d", delay)
		}
	}
}

func testPcmReadTiming(t *testing.T) {
	// Handle Close/Stop/Wait in a centralized defer block.
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &defaultConfig)
	require.NoError(t, err)

	playbackPcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
	require.NoError(t, err)

	// Link the streams to start them synchronously.
	err = playbackPcm.Link(pcm)
	linkSucceeded := err == nil
	if !linkSucceeded {
		pcm.Close()
		playbackPcm.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	// Prepare explicitly.
	require.NoError(t, playbackPcm.Prepare())
	require.NoError(t, pcm.Prepare())

	var wg sync.WaitGroup
	done := make(chan struct{})
	readyToWrite := make(chan struct{})

	// Thread-safe error reporting from goroutine.
	var writerErr error
	var writerErrMtx sync.Mutex
	setWriterErr := func(e error) {
		// Only record errors if we are NOT shutting down.
		select {
		case <-done:
			return
		default:
		}

		writerErrMtx.Lock()
		defer writerErrMtx.Unlock()

		if writerErr == nil {
			writerErr = e
		}
	}

	wg.Add(1)

	go func() {
		defer wg.Done()
		playbackBuffer := make([]byte, alsa.PcmFramesToBytes(playbackPcm, playbackPcm.PeriodSize()))

		close(readyToWrite) // Signal that the writer is ready

		for {
			select {
			case <-done:
				return
			default:
				// Continue to Write.
			}

			// The first Write call on the prepared stream will start both linked streams.
			// This blocks. Relies on Stop() for shutdown.
			_, err := playbackPcm.Write(playbackBuffer)
			if err != nil {
				// EBADF, EPIPE, or EBADFD are expected errors when the stream is
				// stopped during shutdown. This is the signal to exit the goroutine.
				if errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
					return
				}

				// Report any other unexpected errors.
				setWriterErr(fmt.Errorf("playbackPcm.Write failed: %w", err))
				return
			}
		}
	}()

	// Centralized cleanup (Stop before Wait).
	defer func() {
		close(done)
		// Stop streams to unblock the goroutine before waiting.
		_ = pcm.Stop()
		_ = playbackPcm.Stop()
		wg.Wait()

		// Check errors after wait.
		writerErrMtx.Lock()
		if writerErr != nil {
			t.Logf("Writer goroutine reported an error: %v", writerErr)
		}
		writerErrMtx.Unlock()

		// Cleanup resources.
		if linkSucceeded {
			_ = playbackPcm.Unlink()
		}
		_ = pcm.Close()
		_ = playbackPcm.Close()
	}()

	// Wait for the producer to be ready before we start reading.
	<-readyToWrite

	const readCount = 20
	bufferSize := alsa.PcmFramesToBytes(pcm, defaultConfig.PeriodSize)
	buffer := make([]byte, bufferSize)
	frames := alsa.PcmBytesToFrames(pcm, bufferSize)

	start := time.Now()
	readsCompleted := 0
	for i := 0; i < readCount; i++ {
		// Check if peer failed before reading.
		writerErrMtx.Lock()
		if writerErr != nil {
			writerErrMtx.Unlock()
			t.Fatalf("Aborting read loop because writer goroutine failed: %v", writerErr)
		}
		writerErrMtx.Unlock()

		// The Read call will block until data is available.
		read, err := pcm.Read(buffer)
		if err != nil {
			// EPIPE/EBADFD might happen if the writer underruns and stops.
			if errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
				t.Logf("Read encountered expected error (EPIPE/EBADFD) on iteration %d, stopping read loop: %v", i, err)

				break
			}

			t.Fatalf("Read failed on iteration %d: %v", i, err)
		}

		if read != int(frames) {
			t.Fatalf("Read read %d frames, want %d", read, frames)
		}
		readsCompleted++
	}

	duration := time.Since(start)

	expectedDurationMs := math.Ceil(float64(uint32(readsCompleted)*frames) * 1000.0 / float64(defaultConfig.Rate))
	durationMs := float64(duration.Milliseconds())

	tolerance := 200.0
	if (durationMs-expectedDurationMs) > tolerance || (expectedDurationMs-durationMs) > tolerance {
		t.Logf("Read timing test: got %.2f ms, want ~%.2f ms. This can be flaky.", durationMs, expectedDurationMs)
	}
}

func testPcmReadWriteSimple(t *testing.T) {
	config := defaultConfig
	// Handle Close/Stop/Wait explicitly at the end to ensure correct order.
	pcmOut, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &config)
	require.NoError(t, err)

	pcmIn, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &config)
	require.NoError(t, err)

	err = pcmOut.Link(pcmIn)
	if err != nil {
		pcmOut.Close()
		pcmIn.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	// Prepare explicitly.
	require.NoError(t, pcmOut.Prepare())
	require.NoError(t, pcmIn.Prepare())

	var wg sync.WaitGroup
	done := make(chan struct{})

	// Thread-safe error holders
	var readerErr, writerErr error
	var readerErrMtx, writerErrMtx sync.Mutex

	setReaderErr := func(e error) {
		readerErrMtx.Lock()
		defer readerErrMtx.Unlock()
		if readerErr == nil {
			readerErr = e
		}
	}
	setWriterErr := func(e error) {
		writerErrMtx.Lock()
		defer writerErrMtx.Unlock()
		if writerErr == nil {
			writerErr = e
		}
	}

	// Add synchronization for startup.
	readyToRead := make(chan struct{})
	readyToWrite := make(chan struct{})

	// Reader goroutine
	wg.Add(1)

	go func() {
		defer wg.Done()
		readBuffer := make([]byte, alsa.PcmFramesToBytes(pcmIn, pcmIn.PeriodSize()))

		close(readyToRead)

		for {
			// Check done before blocking read.
			select {
			case <-done:
				return
			default:
				// Proceed to blocking read.
			}

			// This blocks. Relies on Stop() for shutdown.
			_, err := pcmIn.Read(readBuffer)
			if err != nil {
				// EPIPE, EBADF, or EBADFD are expected on shutdown or if the stream stops
				if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EBADF) || errors.Is(err, unix.EBADFD) {
					return
				}

				setReaderErr(err)

				return
			}
		}
	}()

	// Writer goroutine
	wg.Add(1)

	go func() {
		defer wg.Done()
		writeBuffer := make([]byte, alsa.PcmFramesToBytes(pcmOut, pcmOut.PeriodSize()))

		close(readyToWrite)

		for {
			// Check done before blocking write.
			select {
			case <-done:
				return
			default:
				// Proceed to blocking write.
			}

			// This blocks. Relies on Stop() for shutdown.
			_, err := pcmOut.Write(writeBuffer)
			if err != nil {
				// EPIPE, EBADF, or EBADFD are expected on shutdown or if the stream stops (underrun)
				if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EBADF) || errors.Is(err, unix.EBADFD) {
					return
				}

				setWriterErr(err)

				return
			}
		}
	}()

	// Wait for readiness.
	<-readyToRead
	<-readyToWrite

	// Run for a short period.
	time.Sleep(500 * time.Millisecond)
	close(done)

	// Stop the streams BEFORE waiting for the goroutines.
	// This unblocks the blocking Read/Write calls, preventing a hang in wg.Wait().
	_ = pcmOut.Stop()
	_ = pcmIn.Stop()

	// Wait for the goroutines to exit cleanly.
	wg.Wait()

	// Now that goroutines are finished, it's safe to close and unlink.
	_ = pcmOut.Unlink()
	_ = pcmIn.Close()
	_ = pcmOut.Close()

	// Check for errors.
	readerErrMtx.Lock()
	require.NoError(t, readerErr, "Reader goroutine failed")
	readerErrMtx.Unlock()

	writerErrMtx.Lock()
	require.NoError(t, writerErr, "Writer goroutine failed")
	writerErrMtx.Unlock()
}

func testPcmMmapWrite(t *testing.T) {
	config := defaultConfig
	if config.PeriodCount > 1 {
		// Start when the buffer is almost full to prevent immediate underrun.
		config.StartThreshold = config.PeriodSize * (config.PeriodCount - 1)
	} else {
		config.StartThreshold = config.PeriodSize
	}

	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_MMAP, &defaultConfig)
	require.NoError(t, err, "PcmOpen with MMAP failed")
	defer pcm.Close()

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_MMAP, &defaultConfig)
	require.NoError(t, err, "PcmOpen capture (MMAP) failed")
	defer capturePcm.Close()

	// Verify that the hardware parameters match exactly.
	finalConfigOut := pcm.Config()
	finalConfigIn := capturePcm.Config()

	// We only compare parameters relevant for the hardware configuration.
	if finalConfigOut.Channels != finalConfigIn.Channels ||
		finalConfigOut.Rate != finalConfigIn.Rate ||
		finalConfigOut.Format != finalConfigIn.Format ||
		finalConfigOut.PeriodSize != finalConfigIn.PeriodSize ||
		finalConfigOut.PeriodCount != finalConfigIn.PeriodCount {
		t.Fatalf("Loopback device parameters do not match after configuration. Out: C=%d R=%d F=%v PS=%d PC=%d, In: C=%d R=%d F=%v PS=%d PC=%d",
			finalConfigOut.Channels, finalConfigOut.Rate, finalConfigOut.Format, finalConfigOut.PeriodSize, finalConfigOut.PeriodCount,
			finalConfigIn.Channels, finalConfigIn.Rate, finalConfigIn.Format, finalConfigIn.PeriodSize, finalConfigIn.PeriodCount)
	}

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
					if errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
						return
					}

					// EAGAIN is okay, just continue the loop
					if errors.Is(err, syscall.EAGAIN) {
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
	bufferSize := alsa.PcmFramesToBytes(pcm, pcm.PeriodSize())
	buffer := make([]byte, bufferSize)

	start := time.Now()
	for i := 0; i < writeCount; i++ {
		// The first MmapWrite will auto-start the linked streams.
		written, err := pcm.MmapWrite(buffer)
		if err != nil {
			if errors.Is(err, syscall.ENOTTY) {
				t.Skip("Skipping MMAP test: device does not support HWSYNC/SYNC_PTR (ENOTTY)")
			}

			if errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
				t.Logf("MmapWrite encountered expected error (EPIPE/EBADFD) on iteration %d, stopping write loop: %v", i, err)

				break
			}

			// EAGAIN is okay, just continue the loop
			if errors.Is(err, syscall.EAGAIN) {
				i-- // Retry this iteration

				continue
			}

			t.Fatalf("MmapWrite failed on iteration %d: %v", i, err)
		}

		expectedFrames := int(alsa.PcmBytesToFrames(pcm, uint32(len(buffer))))
		if written != expectedFrames {
			t.Fatalf("MmapWrite wrote %d frames, want %d", written, expectedFrames)
		}
	}

	duration := time.Since(start)

	require.NoError(t, pcm.Stop())

	expectedFrames := uint32(writeCount) * defaultConfig.PeriodSize
	expectedDurationMs := float64(expectedFrames) * 1000.0 / float64(defaultConfig.Rate)
	durationMs := float64(duration.Milliseconds())

	tolerance := 250.0
	if (durationMs-expectedDurationMs) > tolerance || (expectedDurationMs-durationMs) > tolerance {
		t.Logf("MmapWrite timing test: got %.2f ms, want ~%.2f ms. This can be flaky.", durationMs, expectedDurationMs)
	}
}

func testPcmMmapRead(t *testing.T) {
	// Use separate configs for playback and capture to handle start thresholds correctly.
	playbackConfig := defaultConfig
	playbackConfig.Format = alsa.SNDRV_PCM_FORMAT_S16_LE
	// Set a large start threshold to prevent underruns at the beginning of the stream.
	if playbackConfig.PeriodCount > 1 {
		playbackConfig.StartThreshold = playbackConfig.PeriodSize * (playbackConfig.PeriodCount - 1)
	} else {
		playbackConfig.StartThreshold = playbackConfig.PeriodSize
	}

	captureConfig := defaultConfig
	captureConfig.Format = alsa.SNDRV_PCM_FORMAT_S16_LE
	// By setting the start threshold to be larger than the buffer, we ensure that
	// the explicit Start() call within MmapRead is never triggered.
	// Instead, the stream will be started implicitly by the linked playback stream.
	captureConfig.StartThreshold = captureConfig.PeriodSize*captureConfig.PeriodCount + 1

	// Ensure the loopback device supports the required format and MMAP access using PcmParamsGet.
	playbackParams, err := alsa.PcmParamsGet(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT)
	if err == nil {
		if !playbackParams.FormatIsSupported(playbackConfig.Format) {
			t.Skipf("Playback device does not support format %s", alsa.PcmParamFormatNames[playbackConfig.Format])
		}
		accessMask, err := playbackParams.Mask(alsa.SNDRV_PCM_HW_PARAM_ACCESS)
		if err == nil && !accessMask.Test(uint(alsa.SNDRV_PCM_ACCESS_MMAP_INTERLEAVED)) {
			t.Skip("Playback device does not support MMAP access")
		}
	}

	flagsOut := alsa.PCM_OUT | alsa.PCM_MMAP
	pcmOut, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), flagsOut, &playbackConfig)
	require.NoError(t, err, "PcmOpen(playback, mmap) failed")
	defer pcmOut.Close()

	flagsIn := alsa.PCM_IN | alsa.PCM_MMAP
	pcmIn, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), flagsIn, &captureConfig)
	require.NoError(t, err, "PcmOpen(capture, mmap) failed")
	defer pcmIn.Close()

	// Verify that the hardware parameters match exactly.
	finalConfigOut := pcmOut.Config()
	finalConfigIn := pcmIn.Config()

	// We only compare parameters relevant for the hardware configuration.
	if finalConfigOut.Channels != finalConfigIn.Channels ||
		finalConfigOut.Rate != finalConfigIn.Rate ||
		finalConfigOut.Format != finalConfigIn.Format ||
		finalConfigOut.PeriodSize != finalConfigIn.PeriodSize ||
		finalConfigOut.PeriodCount != finalConfigIn.PeriodCount {
		t.Fatalf("Loopback device parameters do not match after configuration. Out: C=%d R=%d F=%v PS=%d PC=%d, In: C=%d R=%d F=%v PS=%d PC=%d",
			finalConfigOut.Channels, finalConfigOut.Rate, finalConfigOut.Format, finalConfigOut.PeriodSize, finalConfigOut.PeriodCount,
			finalConfigIn.Channels, finalConfigIn.Rate, finalConfigIn.Format, finalConfigIn.PeriodSize, finalConfigIn.PeriodCount)
	}

	err = pcmOut.Link(pcmIn)
	if err != nil {
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}
	defer pcmOut.Unlink()

	require.NoError(t, pcmOut.Prepare(), "playback stream prepare failed")
	require.NoError(t, pcmIn.Prepare(), "capture stream prepare failed")

	var wg sync.WaitGroup
	done := make(chan struct{})

	// Thread-safe error holders
	var writerErr, readerErr error
	var writerErrMtx, readerErrMtx sync.Mutex

	setWriterErr := func(e error) {
		writerErrMtx.Lock()
		defer writerErrMtx.Unlock()
		if writerErr == nil {
			writerErr = e
		}
	}

	setReaderErr := func(e error) {
		readerErrMtx.Lock()
		defer readerErrMtx.Unlock()
		if readerErr == nil {
			readerErr = e
		}
	}

	energyFound := false
	var energyMtx sync.Mutex

	// Writer goroutine
	wg.Add(1)

	go func() {
		defer wg.Done()
		generator := newSineToneGenerator(playbackConfig, 440, 0)
		buffer := make([]byte, alsa.PcmFramesToBytes(pcmOut, pcmOut.PeriodSize()))

		for {
			select {
			case <-done:
				return
			default:
				generator.Read(buffer)
				_, err := pcmOut.MmapWrite(buffer)
				if err != nil {
					// EBADF is expected when the PCM is closed during the shutdown.
					if errors.Is(err, syscall.EBADF) {
						return
					}

					// EPIPE or EBADFD means an XRUN occurred and recovery failed inside MmapWrite.
					// This is a failure unless we are shutting down.
					if errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
						select {
						case <-done:
							// Shutdown in progress, ignore the error.
							return
						default:
							// Not shutting down, report the failure.
							setWriterErr(fmt.Errorf("MmapWrite failed with unrecoverable XRUN (EPIPE/EBADFD): %w", err))

							return
						}
					}

					// EAGAIN means the buffer is full, which is okay; just continue.
					if errors.Is(err, syscall.EAGAIN) {
						continue
					}

					setWriterErr(err)

					return
				}
			}
		}
	}()

	// Reader goroutine
	wg.Add(1)

	go func() {
		defer wg.Done()
		readBuffer := make([]byte, alsa.PcmFramesToBytes(pcmIn, pcmIn.PeriodSize()))

		for {
			select {
			case <-done:
				energyMtx.Lock()
				if !energyFound {
					setReaderErr(fmt.Errorf("test finished but no signal energy was ever detected"))
				}
				energyMtx.Unlock()
				return
			default:
				read, err := pcmIn.MmapRead(readBuffer)
				if err != nil {
					// EBADF is expected on shutdown.
					if errors.Is(err, syscall.EBADF) {
						return
					}

					// EPIPE or EBADFD means an XRUN (overrun) occurred and recovery failed.
					// This is a failure unless we are shutting down.
					if errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
						select {
						case <-done:
							// Shutdown in progress, ignore the error.
							return
						default:
							// Not shutting down, report the failure.
							setReaderErr(fmt.Errorf("MmapRead failed with unrecoverable XRUN (EPIPE/EBADFD): %w", err))
							return
						}
					}

					// EAGAIN is not an error, just means no data is ready.
					if errors.Is(err, syscall.EAGAIN) {
						time.Sleep(1 * time.Millisecond) // Avoid busy-waiting

						continue
					}

					setReaderErr(err)

					return
				}

				if read == 0 {
					continue
				}

				// Since we waited for playbackStarted, any data we now read should contain the generated signal.
				energyMtx.Lock()
				if !energyFound {
					bytesRead := alsa.PcmFramesToBytes(pcmIn, uint32(read))
					if energy(readBuffer[:bytesRead], playbackConfig.Format) > 0 {
						energyFound = true
					}
				}
				energyMtx.Unlock()
			}
		}
	}()

	// Run for a short period
	time.Sleep(500 * time.Millisecond)
	close(done)
	wg.Wait()

	// After goroutines are done, deferred Close calls will execute.
	// Stop is still good practice for MMAP to ensure hardware is quiet.
	_ = pcmOut.Stop()

	writerErrMtx.Lock()
	require.NoError(t, writerErr, "Writer goroutine encountered an error")
	writerErrMtx.Unlock()

	readerErrMtx.Lock()
	require.NoError(t, readerErr, "Reader goroutine encountered an error")
	readerErrMtx.Unlock()

	energyMtx.Lock()
	assert.True(t, energyFound, "Did not detect any signal energy in the captured audio")
	energyMtx.Unlock()
}

func testPcmParams(t *testing.T) {
	t.Run("GetHwParams", func(t *testing.T) {
		// Test getting params for a non-existent device
		params, err := alsa.PcmParamsGet(1000, 1000, alsa.PCM_IN)
		require.Error(t, err, "expected error when getting params for non-existent device")
		require.Nil(t, params)

		// Test valid device for PcmParamsGet to get defaults
		params, err = alsa.PcmParamsGet(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT)
		if err != nil {
			require.NoError(t, err, "PcmParamsGet failed for valid device")
		}
		require.NotNil(t, params, "PcmParamsGet returned nil params for valid device")

		// For default params, the range min and max should be equal, representing the single default value.
		rate, errMin := params.Min(alsa.SNDRV_PCM_HW_PARAM_RATE)
		rateMax, errMax := params.Max(alsa.SNDRV_PCM_HW_PARAM_RATE)
		require.NoError(t, errMin)
		require.NoError(t, errMax)
		assert.NotZero(t, rateMax, "Max rate should not be zero")
		assert.NotZero(t, rate, "Min rate should not be zero")

		channels, err := params.Min(alsa.SNDRV_PCM_HW_PARAM_CHANNELS)
		require.NoError(t, err)
		assert.NotZero(t, channels, "Channels should not be zero")

		// The mask for format will now only have one bit set for the default format.
		formatMask, err := params.Mask(alsa.SNDRV_PCM_HW_PARAM_FORMAT)
		require.NoError(t, err)
		setBits := 0
		for i := 0; i < int(alsa.SNDRV_PCM_FORMAT_U18_3BE)+1; i++ {
			if formatMask.Test(uint(i)) {
				setBits++
			}
		}

		s := params.String()
		require.NotEmpty(t, s)
		t.Log("\n" + s)
	})
}

func testSetConfig(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, nil)
	require.NoError(t, err, "PcmOpen with nil config failed")
	defer pcm.Close()

	// Check that a default config was applied
	require.NotZero(t, pcm.Channels(), "expected non-zero channels with default config")

	// Try setting a new config. We use a config that is likely to be supported by snd-aloop,
	// changing only the buffer geometry, as the device may have a fixed rate/channel count.
	newConfig := alsa.Config{
		Channels:    defaultConfig.Channels,
		Rate:        defaultConfig.Rate,
		PeriodSize:  512,
		PeriodCount: 2,
		Format:      alsa.SNDRV_PCM_FORMAT_S16_LE,
	}

	err = pcm.SetConfig(&newConfig)
	require.NoError(t, err)

	// Verify the new config was applied. The driver may adjust some parameters.
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
	pcm1, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
	require.NoError(t, err, "Failed to open playback stream")
	defer pcm1.Close()

	pcm2, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &defaultConfig)
	require.NoError(t, err, "Failed to open capture stream")
	defer pcm2.Close()

	if err := pcm1.Link(pcm2); err != nil {
		// Some kernels/ALSA versions might not support linking on all devices.
		t.Skipf("pcm1.Link(pcm2) failed: %v. This may not be supported on this system.", err)

		return
	}

	// If linking succeeds, unlinking should also succeed.
	require.NoError(t, pcm1.Unlink(), "pcm1.Unlink() failed")
}

func testPcmDrain(t *testing.T) {
	// Handle Close/Stop/Wait in a centralized defer block.
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
	require.NoError(t, err)

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &defaultConfig)
	require.NoError(t, err, "Could not open capture side of device")

	err = pcm.Link(capturePcm)
	if err != nil {
		pcm.Close()
		capturePcm.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	// Prepare explicitly.
	require.NoError(t, pcm.Prepare())
	require.NoError(t, capturePcm.Prepare())

	var wg sync.WaitGroup
	done := make(chan struct{})
	readyToRead := make(chan struct{})

	// Thread-safe error reporting from goroutine.
	var captureErr error
	var captureErrMtx sync.Mutex
	setCaptureErr := func(e error) {
		// Only record errors if we are NOT shutting down.
		select {
		case <-done:
			return
		default:
		}
		captureErrMtx.Lock()
		defer captureErrMtx.Unlock()
		if captureErr == nil {
			captureErr = e
		}
	}

	wg.Add(1)

	go func() {
		defer wg.Done()

		captureBuffer := make([]byte, alsa.PcmFramesToBytes(capturePcm, capturePcm.PeriodSize()))

		close(readyToRead)

		for {
			select {
			case <-done:
				return
			default:
				// Continue to Read.
			}

			// This blocks. Relies on Stop() for shutdown.
			_, err := capturePcm.Read(captureBuffer)
			if err != nil {
				// EBADF, EPIPE, or EBADFD are expected errors when the stream is
				// stopped during shutdown. This is the signal to exit the goroutine.
				if errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
					return
				}

				// Report any other unexpected errors.
				setCaptureErr(fmt.Errorf("capturePcm.Read failed: %w", err))

				return
			}
		}
	}()

	// Centralized cleanup (Stop before Wait).
	defer func() {
		close(done)
		// Stop streams to unblock the goroutine before waiting.
		_ = pcm.Stop()
		_ = capturePcm.Stop()
		wg.Wait()

		// Check errors after wait.
		captureErrMtx.Lock()
		if captureErr != nil {
			t.Logf("Capture goroutine reported an error: %v", captureErr)
		}
		captureErrMtx.Unlock()

		// Cleanup resources.
		_ = pcm.Unlink()
		_ = pcm.Close()
		_ = capturePcm.Close()
	}()

	<-readyToRead

	// Write some data to the buffer
	bufferSize := pcm.BufferSize()
	frames := bufferSize / 2 // Write half the buffer
	buffer := make([]byte, alsa.PcmFramesToBytes(pcm, frames))

	written, err := pcm.Write(buffer)
	require.NoError(t, err)
	require.Equal(t, int(frames), written, "Write failed before drain")

	// Check if peer failed before Drain.
	captureErrMtx.Lock()
	if captureErr != nil {
		captureErrMtx.Unlock()
		// If the capture side failed, the Drain will likely hang.
		t.Fatalf("Aborting Drain because capture goroutine failed: %v", captureErr)
	}
	captureErrMtx.Unlock()

	// Drain should block until the data is played.
	start := time.Now()
	err = pcm.Drain()
	// EPIPE can happen if an underrun occurred. This is acceptable.
	if err != nil && !errors.Is(err, syscall.EPIPE) {
		require.NoError(t, err, "Drain failed")
	}

	duration := time.Since(start)

	expectedDurationMs := float64(frames) * 1000.0 / float64(pcm.Rate())
	durationMs := float64(duration.Milliseconds())

	// Allow a wide margin for error (50% tolerance for slow hardware)
	if durationMs < expectedDurationMs*0.5 {
		t.Errorf("Drain returned too quickly. Got %.2f ms, expected >~%.2f ms", durationMs, expectedDurationMs*0.5)
	}
}

func testPcmPause(t *testing.T) {
	config := defaultConfig
	// Set the start threshold low to start streaming quickly.
	config.StartThreshold = config.PeriodSize

	flags := alsa.PCM_OUT
	captureFlags := alsa.PCM_IN

	// Handle Close/Stop/Wait in a centralized defer block.
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), flags, &config)
	require.NoError(t, err)

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), captureFlags, &config)
	require.NoError(t, err, "PcmOpen capture failed")

	err = pcm.Link(capturePcm)
	if err != nil {
		pcm.Close()
		capturePcm.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	// Prepare is good practice before starting I/O.
	require.NoError(t, pcm.Prepare(), "playback stream prepare failed")
	require.NoError(t, capturePcm.Prepare(), "capture stream prepare failed")

	done := make(chan struct{})
	var wg sync.WaitGroup

	// Synchronization channels.
	readyToWrite := make(chan struct{})
	readyToRead := make(chan struct{})

	// Centralized cleanup (Stop before Wait).
	defer func() {
		close(done)
		// Stop streams to unblock I/O goroutines.
		_ = pcm.Stop()
		_ = capturePcm.Stop()
		wg.Wait()

		// Cleanup resources.
		_ = pcm.Unlink()
		_ = pcm.Close()
		_ = capturePcm.Close()
	}()

	wg.Add(2)

	// This goroutine will continuously supply data to the playback device.
	go func() {
		defer wg.Done()
		writeBuf := make([]byte, alsa.PcmFramesToBytes(pcm, pcm.PeriodSize()))
		close(readyToWrite)
		for {
			select {
			case <-done:
				return
			default:
				// Continue to Write.
			}

			// This call will start the stream and will block. Relies on Stop() for shutdown.
			_, err := pcm.Write(writeBuf)
			if err != nil {
				// An error (e.g., EPIPE on stop) will cause the goroutine to exit.
				return
			}
		}
	}()

	// This goroutine will continuously consume data from the capture device.
	go func() {
		defer wg.Done()
		readBuf := make([]byte, alsa.PcmFramesToBytes(capturePcm, capturePcm.PeriodSize()))
		close(readyToRead)
		for {
			select {
			case <-done:
				return
			default:
				// Continue to Read.
			}

			// This call will block. Relies on Stop() for shutdown.
			_, err := capturePcm.Read(readBuf)
			if err != nil {
				return
			}
		}
	}()

	// Wait for synchronization.
	<-readyToWrite
	<-readyToRead

	// Poll for state.
	var state alsa.PcmState
	// Allow up to 1s (100 * 10ms)
	for i := 0; i < 100; i++ {
		state = pcm.State()
		if state == alsa.SNDRV_PCM_STATE_RUNNING {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	require.Equal(t, alsa.SNDRV_PCM_STATE_RUNNING, state, "PCM stream should be running before pause")

	// Pause the stream. The I/O goroutines should now block (in their respective I/O calls).
	err = pcm.Pause(true)
	if err != nil {
		if errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.ENOSYS) {
			t.Skipf("Skipping pause test, PAUSE ioctl is not supported: %v", err)

			// Cleanup happens via defer.
			return
		}

		require.NoError(t, err, "pcm.Pause(true) failed")
	}

	// Poll for PAUSED state.
	for i := 0; i < 50; i++ {
		state = pcm.State()
		if state == alsa.SNDRV_PCM_STATE_PAUSED {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	require.Equal(t, alsa.SNDRV_PCM_STATE_PAUSED, state, "PCM stream state should be PAUSED")

	time.Sleep(100 * time.Millisecond)

	// Resume the stream. The I/O goroutines should unblock and continue.
	require.NoError(t, pcm.Pause(false), "pcm.Pause(false) failed")

	// Poll for RUNNING state again.
	for i := 0; i < 50; i++ {
		state = pcm.State()
		if state == alsa.SNDRV_PCM_STATE_RUNNING {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	require.Equal(t, alsa.SNDRV_PCM_STATE_RUNNING, state, "PCM stream state should be RUNNING after resume")

	// Let the goroutines run for another moment.
	time.Sleep(100 * time.Millisecond) // Increased duration.
}

func testPcmLoopback(t *testing.T) {
	testFormats := []struct {
		name   string
		format alsa.PcmFormat
	}{
		{"S16_LE", alsa.SNDRV_PCM_FORMAT_S16_LE},
		{"FLOAT_LE", alsa.SNDRV_PCM_FORMAT_FLOAT_LE},
	}

	// First, check if the loopback device supports the formats we want to test.
	playbackParams, err := alsa.PcmParamsGet(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT)
	if err != nil {
		require.NoError(t, err, "Failed to get params for loopback playback device")
	}

	captureParams, err := alsa.PcmParamsGet(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN)
	if err != nil {
		captureParams, err = alsa.PcmParamsGet(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN)
		require.NoError(t, err, "Failed to get params for loopback capture device")
	}

	for _, tf := range testFormats {
		t.Run(tf.name, func(t *testing.T) {
			if !playbackParams.FormatIsSupported(tf.format) {
				t.Skipf("Playback device does not support format %s, skipping", alsa.PcmParamFormatNames[tf.format])
			}

			if !captureParams.FormatIsSupported(tf.format) {
				t.Skipf("Capture device does not support format %s, skipping", alsa.PcmParamFormatNames[tf.format])
			}

			// Use separate configs for playback and capture to handle start thresholds correctly.
			playbackConfig := defaultConfig
			playbackConfig.Format = tf.format
			// Set a large start threshold to prevent underruns at the beginning of the stream.
			if playbackConfig.PeriodCount > 1 {
				playbackConfig.StartThreshold = playbackConfig.PeriodSize * (playbackConfig.PeriodCount - 1)
			} else {
				playbackConfig.StartThreshold = playbackConfig.PeriodSize
			}

			captureConfig := defaultConfig
			captureConfig.Format = tf.format
			// Use the default start threshold for capture (1), by setting it to 0 here.
			captureConfig.StartThreshold = 0

			// Open playback stream
			pcmOut, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &playbackConfig)
			require.NoError(t, err, "PcmOpen(playback) failed")
			defer pcmOut.Close()

			// Open capture stream
			pcmIn, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &captureConfig)
			require.NoError(t, err, "PcmOpen(capture) failed")
			defer pcmIn.Close()

			// Verify that the hardware parameters match exactly.
			finalConfigOut := pcmOut.Config()
			finalConfigIn := pcmIn.Config()

			// We only compare parameters relevant for the hardware configuration.
			if finalConfigOut.Channels != finalConfigIn.Channels ||
				finalConfigOut.Rate != finalConfigIn.Rate ||
				finalConfigOut.Format != finalConfigIn.Format ||
				finalConfigOut.PeriodSize != finalConfigIn.PeriodSize ||
				finalConfigOut.PeriodCount != finalConfigIn.PeriodCount {
				t.Fatalf("Loopback device parameters do not match after configuration. Out: C=%d R=%d F=%v PS=%d PC=%d, In: C=%d R=%d F=%v PS=%d PC=%d",
					finalConfigOut.Channels, finalConfigOut.Rate, finalConfigOut.Format, finalConfigOut.PeriodSize, finalConfigOut.PeriodCount,
					finalConfigIn.Channels, finalConfigIn.Rate, finalConfigIn.Format, finalConfigIn.PeriodSize, finalConfigIn.PeriodCount)
			}

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

				var energyFound = false
				for {
					select {
					case <-done:
						if !energyFound {
							setCaptureErr(fmt.Errorf("test finished but no signal energy was ever detected"))
						}

						return
					default:
						// This call will block until the linked playback stream starts and provides data.
						read, err := pcmIn.Read(buffer)
						if err != nil {
							// EPIPE means XRUN (overrun), EBADF can happen on close. These are expected during a racy shutdown/teardown.
							if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EBADF) || errors.Is(err, unix.EBADFD) {
								return
							}

							setCaptureErr(fmt.Errorf("the Read failed: %w", err))

							return
						}

						if read != int(frames) {
							setCaptureErr(fmt.Errorf("short read: got %d frames, want %d", read, frames))

							return
						}

						// Once data is flowing, we should detect the signal.
						if !energyFound {
							if energy(buffer, playbackConfig.Format) > 0.0 {
								energyFound = true
							}
						}
					}
				}
			}()

			// Playback goroutine
			wg.Add(1)

			go func() {
				defer wg.Done()

				generator := newSineToneGenerator(playbackConfig, 1000, 0) // 1kHz tone, 0dB
				bufferSize := alsa.PcmFramesToBytes(pcmOut, pcmOut.PeriodSize())
				frames := pcmOut.PeriodSize()
				buffer := make([]byte, bufferSize)
				counter := 0

				for {
					select {
					case <-done:
						return
					default:
						generator.Read(buffer)
						written, err := pcmOut.Write(buffer)
						if err != nil {
							// EPIPE means XRUN (underrun), EBADF can happen on close. These are expected during a racy shutdown/teardown.
							if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EBADF) || errors.Is(err, unix.EBADFD) {
								return
							}

							setPlaybackErr(fmt.Errorf("the Write failed on iteration %d: %w", counter, err))

							return
						}
						if written != int(frames) {
							setPlaybackErr(fmt.Errorf("short write on iteration %d: got %d frames, want %d", counter, written, frames))

							return
						}

						counter++
					}
				}
			}()

			// Run for a period to allow playback and capture.
			time.Sleep(250 * time.Millisecond)
			close(done)
			wg.Wait()

			// Now that the I/O goroutines are finished, it's safe to stop the streams.
			// This prevents a race condition where a deferred Close() could run while a goroutine is still mid-syscall.
			_ = pcmOut.Stop()
			_ = pcmIn.Stop()

			// Check for errors that occurred in the goroutines.
			captureErrMtx.Lock()
			require.NoError(t, captureErr, "Capture goroutine failed")
			captureErrMtx.Unlock()

			playbackErrMtx.Lock()
			require.NoError(t, playbackErr, "Playback goroutine failed")
			playbackErrMtx.Unlock()
		})
	}
}

func testPcmNonBlocking(t *testing.T) {
	t.Run("Write", func(t *testing.T) {
		pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_NONBLOCK, &defaultConfig)
		require.NoError(t, err)
		defer pcm.Close()

		// The buffer size of the PCM device.
		bufferSizeInFrames := pcm.BufferSize()
		periodSizeInBytes := alsa.PcmFramesToBytes(pcm, pcm.PeriodSize())
		writeBuffer := make([]byte, periodSizeInBytes)

		var writeErr error
		// Write more data than the buffer can hold to force a non-blocking error.
		// We'll write up to 2x the buffer size. The first few writes should succeed.
		// Eventually, the buffer will fill up and a write should return EAGAIN.
		for i := 0; i < int(bufferSizeInFrames/pcm.PeriodSize())*2; i++ {
			_, err := pcm.Write(writeBuffer)
			if err != nil {
				writeErr = err

				break
			}
		}

		require.NotNil(t, writeErr, "Write loop finished without any error, expected EAGAIN")
		assert.ErrorIs(t, writeErr, syscall.EAGAIN, "Expected EAGAIN when writing to a full non-blocking buffer")
	})

	t.Run("Read", func(t *testing.T) {
		// Open a capture stream in non-blocking mode.
		pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_NONBLOCK, &defaultConfig)
		require.NoError(t, err)
		defer pcm.Close()

		// Attempt to read when no data is available.
		buffer := make([]byte, alsa.PcmFramesToBytes(pcm, pcm.PeriodSize()))
		read, err := pcm.Read(buffer)

		// Should return 0 frames read and EAGAIN.
		assert.Equal(t, 0, read, "Read should return 0 frames when no data is available")
		assert.ErrorIs(t, err, syscall.EAGAIN, "Expected EAGAIN when reading from an empty non-blocking buffer")
	})
}

func testPcmMmapLoopback(t *testing.T) {
	// Use separate configs for playback and capture to handle start thresholds correctly.
	playbackConfig := defaultConfig
	playbackConfig.Format = alsa.SNDRV_PCM_FORMAT_S16_LE

	// Set a large start threshold to prevent underruns at the beginning of the stream.
	if playbackConfig.PeriodCount > 1 {
		playbackConfig.StartThreshold = playbackConfig.PeriodSize * (playbackConfig.PeriodCount - 1)
	} else {
		playbackConfig.StartThreshold = playbackConfig.PeriodSize
	}

	captureConfig := defaultConfig
	captureConfig.Format = alsa.SNDRV_PCM_FORMAT_S16_LE

	// By setting the start threshold to be larger than the buffer, we ensure that
	// the explicit Start() call within MmapRead is never triggered.
	// Instead, the stream will be started implicitly by the linked playback stream.
	captureConfig.StartThreshold = captureConfig.PeriodSize*captureConfig.PeriodCount + 1

	// Ensure the loopback device supports the required format and MMAP access.
	playbackParams, err := alsa.PcmParamsGet(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT)
	if err != nil {
		require.NoError(t, err, "Failed to get params for loopback playback device")
	}

	if !playbackParams.FormatIsSupported(playbackConfig.Format) {
		t.Skipf("Playback device does not support format %s", alsa.PcmParamFormatNames[playbackConfig.Format])
	}

	accessMask, err := playbackParams.Mask(alsa.SNDRV_PCM_HW_PARAM_ACCESS)
	require.NoError(t, err)
	if !accessMask.Test(uint(alsa.SNDRV_PCM_ACCESS_MMAP_INTERLEAVED)) {
		t.Skip("Playback device does not support MMAP access")
	}

	// Open playback stream with MMAP
	pcmOut, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_MMAP, &playbackConfig)
	require.NoError(t, err, "PcmOpen(playback, mmap) failed")
	defer pcmOut.Close()

	// Open capture stream with MMAP
	pcmIn, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_MMAP, &captureConfig)
	require.NoError(t, err, "PcmOpen(capture, mmap) failed")
	defer pcmIn.Close()

	// Verify that the hardware parameters match exactly.
	finalConfigOut := pcmOut.Config()
	finalConfigIn := pcmIn.Config()

	// We only compare parameters relevant for the hardware configuration.
	if finalConfigOut.Channels != finalConfigIn.Channels ||
		finalConfigOut.Rate != finalConfigIn.Rate ||
		finalConfigOut.Format != finalConfigIn.Format ||
		finalConfigOut.PeriodSize != finalConfigIn.PeriodSize ||
		finalConfigOut.PeriodCount != finalConfigIn.PeriodCount {
		t.Fatalf("Loopback device parameters do not match after configuration. Out: C=%d R=%d F=%v PS=%d PC=%d, In: C=%d R=%d F=%v PS=%d PC=%d",
			finalConfigOut.Channels, finalConfigOut.Rate, finalConfigOut.Format, finalConfigOut.PeriodSize, finalConfigOut.PeriodCount,
			finalConfigIn.Channels, finalConfigIn.Rate, finalConfigIn.Format, finalConfigIn.PeriodSize, finalConfigIn.PeriodCount)
	}

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

	var energyFound bool
	var energyMtx sync.Mutex

	// Capture goroutine
	wg.Add(1)

	go func() {
		defer wg.Done()
		buffer := make([]byte, alsa.PcmFramesToBytes(pcmIn, pcmIn.PeriodSize()))

		for {
			select {
			case <-done:
				// When shutting down, check if we ever found energy. If not, report it as an error.
				energyMtx.Lock()
				if !energyFound {
					setCaptureErr(fmt.Errorf("test finished but no signal energy was ever detected"))
				}
				energyMtx.Unlock()

				return
			default:
				// MmapRead will block internally via p.Wait() until data is ready.
				read, err := pcmIn.MmapRead(buffer)
				if err != nil {
					// EBADF is expected on shutdown.
					if errors.Is(err, syscall.EBADF) {
						return
					}

					// EPIPE or EBADFD means an XRUN (overrun) occurred and recovery failed.
					// This is a failure unless we are shutting down.
					if errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
						select {
						case <-done:
							// Shutdown in progress, ignore the error.
							return
						default:
							// Not shutting down, report the failure.
							setCaptureErr(fmt.Errorf("MmapRead failed with unrecoverable XRUN (EPIPE/EBADFD): %w", err))

							return
						}
					}

					// EAGAIN might happen if the buffer is momentarily empty.
					if errors.Is(err, syscall.EAGAIN) {
						time.Sleep(1 * time.Millisecond) // Avoid busy-waiting

						continue
					}

					setCaptureErr(fmt.Errorf("MmapRead failed: %w", err))

					return
				}
				if read == 0 {
					continue
				}

				energyMtx.Lock()
				if !energyFound {
					bytesRead := alsa.PcmFramesToBytes(pcmIn, uint32(read))
					if energy(buffer[:bytesRead], playbackConfig.Format) > 0.0 {
						energyFound = true
					}
				}
				energyMtx.Unlock()
			}
		}
	}()

	// Playback goroutine
	wg.Add(1)

	go func() {
		defer wg.Done()

		generator := newSineToneGenerator(playbackConfig, 1000, 0) // 1kHz tone, 0dB
		buffer := make([]byte, alsa.PcmFramesToBytes(pcmOut, pcmOut.PeriodSize()))
		counter := 0

		for {
			select {
			case <-done:
				return
			default:
				generator.Read(buffer)
				written, err := pcmOut.MmapWrite(buffer)
				if err != nil {
					// EPIPE or EBADFD means an XRUN (underrun) occurred and recovery failed.
					// This is a failure unless we are shutting down.
					if errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
						select {
						case <-done:
							// Shutdown in progress, ignore the error.
							return
						default:
							// Not shutting down, report the failure.
							setPlaybackErr(fmt.Errorf("MmapWrite failed with unrecoverable XRUN (EPIPE/EBADFD) on iteration %d: %w", counter, err))
							return
						}
					}

					// EAGAIN might happen if the buffer is momentarily full.
					if errors.Is(err, syscall.EAGAIN) {
						continue
					}

					setPlaybackErr(fmt.Errorf("MmapWrite failed on iteration %d: %w", counter, err))

					return
				}

				expectedFrames := int(pcmOut.PeriodSize())
				if written != expectedFrames {
					setPlaybackErr(fmt.Errorf("short mmap write on iteration %d: got %d frames, want %d", counter, written, expectedFrames))

					return
				}

				counter++
			}
		}
	}()

	// Run for a longer period to allow playback and capture.
	time.Sleep(500 * time.Millisecond)
	// Signal goroutines to stop, then wait for them before stopping the PCM stream.
	close(done)
	wg.Wait()
	// Now that the I/O goroutines are finished, it's safe to stop the stream.
	_ = pcmOut.Stop()

	// Check for errors that occurred in the goroutines.
	captureErrMtx.Lock()
	require.NoError(t, captureErr, "Capture goroutine failed")
	captureErrMtx.Unlock()

	playbackErrMtx.Lock()
	require.NoError(t, playbackErr, "Playback goroutine failed")
	playbackErrMtx.Unlock()

	energyMtx.Lock()
	assert.True(t, energyFound, "Did not detect any signal energy in the captured audio")
	energyMtx.Unlock()
}

func testPcmMmapNonBlocking(t *testing.T) {
	t.Run("Write", func(t *testing.T) {
		pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_MMAP|alsa.PCM_NONBLOCK, &defaultConfig)
		require.NoError(t, err)
		defer pcm.Close()

		require.NoError(t, pcm.Prepare())

		// A buffer larger than the device buffer to guarantee we can trigger EAGAIN.
		bufferSizeInBytes := alsa.PcmFramesToBytes(pcm, pcm.BufferSize())
		writeBuffer := make([]byte, bufferSizeInBytes*2)

		// This write should fill the device buffer and return EAGAIN.
		// It will write up to bufferSizeInBytes and then fail.
		written, err := pcm.MmapWrite(writeBuffer)

		require.ErrorIs(t, err, syscall.EAGAIN, "Expected EAGAIN when MmapWrite fills the buffer")
		// It should have written some data, but not more than the internal buffer size.
		assert.Greater(t, written, 0, "MmapWrite should have written some data before returning EAGAIN")
		assert.LessOrEqual(t, written, int(pcm.BufferSize()), "MmapWrite should not write more frames than the buffer size")
	})

	t.Run("Read", func(t *testing.T) {
		// Open a capture stream in non-blocking mode.
		pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_MMAP|alsa.PCM_NONBLOCK, &defaultConfig)
		require.NoError(t, err)
		defer pcm.Close()

		require.NoError(t, pcm.Prepare())

		// Attempt to read when no data is available.
		buffer := make([]byte, alsa.PcmFramesToBytes(pcm, pcm.PeriodSize()))
		read, err := pcm.MmapRead(buffer)

		// Should return 0 frames read and EAGAIN.
		assert.Equal(t, 0, read, "MmapRead should return 0 frames when no data is available")
		assert.ErrorIs(t, err, syscall.EAGAIN, "Expected EAGAIN when reading from an empty non-blocking mmap buffer")
	})
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
			case alsa.SNDRV_PCM_FORMAT_S16_LE:
				var sample int16
				if sine >= 1.0 {
					sample = 32767
				} else if sine <= -1.0 {
					sample = -32768
				} else {
					sample = int16(sine * 32767)
				}

				binary.LittleEndian.PutUint16(buffer[offset:], uint16(sample))
			case alsa.SNDRV_PCM_FORMAT_FLOAT_LE:
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
	case alsa.SNDRV_PCM_FORMAT_S16_LE:
		samples := unsafe.Slice((*int16)(unsafe.Pointer(&buffer[0])), len(buffer)/2)
		for _, sample := range samples {
			val := float64(sample)
			sum += val * val
		}
	case alsa.SNDRV_PCM_FORMAT_FLOAT_LE:
		samples := unsafe.Slice((*float32)(unsafe.Pointer(&buffer[0])), len(buffer)/4)
		for _, sample := range samples {
			val := float64(sample)
			sum += val * val
		}
	}

	return sum
}

func testPcmMmapWriteTiming(t *testing.T) {
	config := defaultConfig
	// Handle Close/Stop/Wait in a centralized defer block.
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_MMAP, &config)
	require.NoError(t, err)

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_MMAP, &config)
	require.NoError(t, err, "Could not open capture side of device")

	// Link the streams to start them synchronously.
	err = pcm.Link(capturePcm)
	if err != nil {
		_ = pcm.Close()
		_ = capturePcm.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	// Prepare explicitly.
	require.NoError(t, pcm.Prepare())
	require.NoError(t, capturePcm.Prepare())

	var wg sync.WaitGroup
	done := make(chan struct{})
	readyToRead := make(chan struct{})

	// Thread-safe error reporting from goroutine.
	var captureErr error
	var captureErrMtx sync.Mutex
	setCaptureErr := func(e error) {
		// Only record errors if we are NOT shutting down.
		select {
		case <-done:
			return
		default:
		}

		captureErrMtx.Lock()
		defer captureErrMtx.Unlock()

		if captureErr == nil {
			captureErr = e
		}
	}

	wg.Add(1)

	go func() {
		defer wg.Done()
		captureBuffer := make([]byte, alsa.PcmFramesToBytes(capturePcm, capturePcm.PeriodSize()))

		close(readyToRead) // Signal that the reader is about to start its loop

		for {
			select {
			case <-done:
				return
			default:
				// Continue to Read.
			}

			// This will block. Relies on Stop() for shutdown.
			_, err := capturePcm.MmapRead(captureBuffer)
			if err != nil {
				// EBADF, EPIPE, or EBADFD are expected errors when the stream is
				// stopped during shutdown. This is the signal to exit the goroutine.
				if errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
					return
				}

				// Report any other unexpected errors.
				setCaptureErr(fmt.Errorf("capturePcm.MmapRead failed: %w", err))

				return
			}
		}
	}()

	// Centralized cleanup (Stop before Wait).
	defer func() {
		close(done)
		// Stop streams to unblock the goroutine before waiting.
		_ = pcm.Stop()
		_ = capturePcm.Stop()
		wg.Wait()

		// Check errors after wait.
		captureErrMtx.Lock()
		if captureErr != nil {
			t.Logf("Capture goroutine reported an error: %v", captureErr)
		}
		captureErrMtx.Unlock()

		// Cleanup resources.
		_ = pcm.Unlink()
		_ = pcm.Close()
		_ = capturePcm.Close()
	}()

	// Wait until the consumer is ready to read.
	<-readyToRead

	const writeCount = 20
	bufferSize := alsa.PcmFramesToBytes(pcm, config.PeriodSize)
	buffer := make([]byte, bufferSize)
	frames := alsa.PcmBytesToFrames(pcm, bufferSize)

	start := time.Now()
	writesCompleted := 0
	for i := 0; i < writeCount; i++ {
		// Check if peer failed before writing.
		captureErrMtx.Lock()
		if captureErr != nil {
			captureErrMtx.Unlock()
			t.Fatalf("Aborting write loop because capture goroutine failed: %v", captureErr)
		}
		captureErrMtx.Unlock()

		// The first call to MmapWrite will implicitly start both linked streams (as they are prepared).
		written, err := pcm.MmapWrite(buffer)
		if err != nil {
			// Allow EPIPE/EBADFD here as it can happen during concurrent tests if the stream stops unexpectedly (e.g. underrun).
			if errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
				t.Logf("MmapWrite encountered expected error (EPIPE/EBADFD) on iteration %d, stopping write loop: %v", i, err)

				break
			}

			t.Fatalf("MmapWrite failed on iteration %d: %v", i, err)
		}

		if written != int(frames) {
			t.Fatalf("MmapWrite wrote %d frames, want %d", written, frames)
		}

		writesCompleted++
	}

	// Check if peer failed before Drain.
	captureErrMtx.Lock()
	if captureErr != nil {
		captureErrMtx.Unlock()
		// If the capture side failed, Drain will likely hang or return EPIPE.
		t.Logf("Skipping Drain because capture goroutine failed: %v", captureErr)
	} else {
		captureErrMtx.Unlock()
		// After writing all data, call Drain to wait for playback to complete.
		err = pcm.Drain()
		// An xrun (EPIPE) can occur in Drain if the consumer stops unexpectedly or an underrun happened earlier. This is acceptable.
		if err != nil && !errors.Is(err, syscall.EPIPE) {
			require.NoError(t, err, "Drain failed after writing")
		}
	}

	duration := time.Since(start)

	// The total time should be roughly the time it takes to play the data actually written.
	expectedFrames := uint32(writesCompleted) * config.PeriodSize
	expectedDurationMs := math.Ceil(float64(expectedFrames) * 1000.0 / float64(config.Rate))
	durationMs := float64(duration.Milliseconds())

	// Allow a generous tolerance for timing assertions.
	tolerance := 200.0
	if (durationMs-expectedDurationMs) > tolerance || (expectedDurationMs-durationMs) > tolerance {
		t.Logf("MmapWrite+Drain timing test: got %.2f ms, want ~%.2f ms. This can be flaky.", durationMs, expectedDurationMs)
	}
}

func testPcmMmapReadTiming(t *testing.T) {
	// Handle Close/Stop/Wait in a centralized defer block.
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_MMAP, &defaultConfig)
	require.NoError(t, err)

	playbackPcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_MMAP, &defaultConfig)
	require.NoError(t, err)

	// Link the streams to start them synchronously.
	err = playbackPcm.Link(pcm)
	if err != nil {
		_ = pcm.Close()
		_ = playbackPcm.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	// Prepare explicitly.
	require.NoError(t, playbackPcm.Prepare())
	require.NoError(t, pcm.Prepare())

	var wg sync.WaitGroup
	done := make(chan struct{})
	readyToWrite := make(chan struct{})

	// Thread-safe error reporting from goroutine.
	var writerErr error
	var writerErrMtx sync.Mutex
	setWriterErr := func(e error) {
		// Only record errors if we are NOT shutting down.
		select {
		case <-done:
			return
		default:
		}

		writerErrMtx.Lock()
		defer writerErrMtx.Unlock()

		if writerErr == nil {
			writerErr = e
		}
	}

	wg.Add(1)

	go func() {
		defer wg.Done()
		playbackBuffer := make([]byte, alsa.PcmFramesToBytes(playbackPcm, playbackPcm.PeriodSize()))

		close(readyToWrite) // Signal that the writer is ready

		for {
			select {
			case <-done:
				return
			default:
				// Continue to Write.
			}

			// The first MmapWrite call on the prepared stream will start both linked streams.
			// This blocks. Relies on Stop() for shutdown.
			_, err := playbackPcm.MmapWrite(playbackBuffer)
			if err != nil {
				// EBADF, EPIPE, or EBADFD are expected errors when the stream is
				// stopped during shutdown. This is the signal to exit the goroutine.
				if errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
					return
				}

				// Report any other unexpected errors.
				setWriterErr(fmt.Errorf("playbackPcm.MmapWrite failed: %w", err))

				return
			}
		}
	}()

	// Centralized cleanup (Stop before Wait).
	defer func() {
		close(done)
		// Stop streams to unblock the goroutine before waiting.
		_ = pcm.Stop()
		_ = playbackPcm.Stop()
		wg.Wait()

		// Check errors after wait.
		writerErrMtx.Lock()
		if writerErr != nil {
			t.Logf("Writer goroutine reported an error: %v", writerErr)
		}
		writerErrMtx.Unlock()

		// Cleanup resources.
		_ = playbackPcm.Unlink()
		_ = pcm.Close()
		_ = playbackPcm.Close()
	}()

	// Wait for the producer to be ready before we start reading.
	<-readyToWrite

	const readCount = 20
	bufferSize := alsa.PcmFramesToBytes(pcm, defaultConfig.PeriodSize)
	buffer := make([]byte, bufferSize)
	frames := alsa.PcmBytesToFrames(pcm, bufferSize)

	start := time.Now()
	readsCompleted := 0
	for i := 0; i < readCount; i++ {
		// Check if peer failed before reading.
		writerErrMtx.Lock()
		if writerErr != nil {
			writerErrMtx.Unlock()
			t.Fatalf("Aborting read loop because writer goroutine failed: %v", writerErr)
		}
		writerErrMtx.Unlock()

		// The MmapRead call will block until data is available.
		read, err := pcm.MmapRead(buffer)
		if err != nil {
			// EPIPE/EBADFD might happen if the writer underruns and stops.
			if errors.Is(err, syscall.EPIPE) || errors.Is(err, unix.EBADFD) {
				t.Logf("MmapRead encountered expected error (EPIPE/EBADFD) on iteration %d, stopping read loop: %v", i, err)

				break
			}

			t.Fatalf("MmapRead failed on iteration %d: %v", i, err)
		}

		if read != int(frames) {
			t.Fatalf("MmapRead read %d frames, want %d", read, frames)
		}

		readsCompleted++
	}

	duration := time.Since(start)

	expectedDurationMs := math.Ceil(float64(uint32(readsCompleted)*frames) * 1000.0 / float64(defaultConfig.Rate))
	durationMs := float64(duration.Milliseconds())

	tolerance := 200.0
	if (durationMs-expectedDurationMs) > tolerance || (expectedDurationMs-durationMs) > tolerance {
		t.Logf("MmapRead timing test: got %.2f ms, want ~%.2f ms. This can be flaky.", durationMs, expectedDurationMs)
	}
}
