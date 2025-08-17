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

	"github.com/gen2brain/alsa"
)

// To run these tests, the 'snd-aloop' kernel module must be loaded:
//
// sudo modprobe snd-aloop
//
// This creates virtual loopback sound cards that allow testing playback and capture.

var (
	// defaultConfig mirrors the configuration used in the C++ tests.
	defaultConfig = alsa.Config{
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

func TestPcmInvalidBuffers(t *testing.T) {
	t.Parallel()
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
	require.NoError(t, err, "Failed to open PCM for invalid buffer tests")
	defer pcm.Close()

	// Test WriteI with various invalid inputs
	_, err = pcm.WriteI(nil, 100)
	assert.Error(t, err, "WriteI with nil buffer should fail")

	_, err = pcm.WriteI(123, 100)
	assert.Error(t, err, "WriteI with non-slice buffer should fail")

	var unsupportedSlice []struct{}
	_, err = pcm.WriteI(unsupportedSlice, 100)
	assert.Error(t, err, "WriteI with unsupported slice type should fail")

	smallBuffer := make([]int16, 10)
	_, err = pcm.WriteI(smallBuffer, 20) // Request more frames than buffer holds
	assert.Error(t, err, "WriteI with buffer smaller than requested frames should fail")

	// Test WriteN with invalid inputs
	_, err = pcm.WriteN(nil, 100)
	assert.Error(t, err, "WriteN with nil buffer should fail")

	notSliceOfSlices := []int{1, 2, 3}
	_, err = pcm.WriteN(notSliceOfSlices, 100)
	assert.Error(t, err, "WriteN with non-slice-of-slices buffer should fail")

	wrongChannelCount := make([][]int16, 1)
	wrongChannelCount[0] = make([]int16, 1024)
	_, err = pcm.WriteN(wrongChannelCount, 100)
	assert.Error(t, err, "WriteN with wrong number of channels should fail")

	smallChannelBuffer := make([][]int16, 2)
	smallChannelBuffer[0] = make([]int16, 128)
	smallChannelBuffer[1] = make([]int16, 10) // This channel is too small
	_, err = pcm.WriteN(smallChannelBuffer, 128)
	assert.Error(t, err, "WriteN with undersized channel buffer should fail")
}

// TestPcmHardware runs all hardware-related tests sequentially to avoid race conditions
// from multiple tests trying to access the same ALSA device concurrently.
func TestPcmHardware(t *testing.T) {
	t.Run("PcmOpenAndClose", testPcmOpenAndClose)
	t.Run("PcmOpenByName", testPcmOpenByName)
	t.Run("PcmPlaybackStartup", testPcmPlaybackStartup)
	t.Run("PcmGetters", testPcmGetters)
	t.Run("PcmFramesBytesConvert", testPcmFramesBytesConvert)
	t.Run("PcmTimestampFailsOnNonMmap", testPcmTimestampFailsOnNonMmap)
	t.Run("PcmHWPtrFailsOnNonMmap", testPcmHWPtrFailsOnNonMmap)
	t.Run("PcmWriteiFailsOnCapture", testPcmWriteiFailsOnCapture)
	t.Run("PcmReadiFailsOnPlayback", testPcmReadiFailsOnPlayback)
	t.Run("PcmWriteiTiming", testPcmWriteiTiming)
	t.Run("PcmWriteNFailsOnCapture", testPcmWriteNFailsOnCapture)
	t.Run("PcmReadNFailsOnPlayback", testPcmReadNFailsOnPlayback)
	t.Run("PcmGetDelay", testPcmGetDelay)
	t.Run("PcmReadiTiming", testPcmReadiTiming)
	t.Run("PcmReadWriteSample", testPcmReadWriteSimple)
	t.Run("PcmMmapWrite", testPcmMmapWrite)
	t.Run("PcmMmapRead", testPcmMmapRead)
	t.Run("PcmState", testPcmState)
	t.Run("PcmStop", testPcmStop)
	t.Run("PcmWait", testPcmWait)
	t.Run("PcmXrunNoRestart", testPcmXrunNoRestart)
	t.Run("PcmParams", testPcmParams)
	t.Run("SetConfig", testSetConfig)
	t.Run("PcmLink", testPcmLink)
	t.Run("PcmDrain", testPcmDrain)
	t.Run("PcmPause", testPcmPause)
	t.Run("PcmLoopback", testPcmLoopback)
	t.Run("PcmMmapLoopback", testPcmMmapLoopback)
	t.Run("PcmLoopbackNonInterleaved", testPcmLoopbackNonInterleaved)
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
				// If it fails with ENOTTY or EINVAL, skip the test gracefully.
				if tc.flags&alsa.PCM_MONOTONIC != 0 {
					if errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.EINVAL) {
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
	t.Run("KernelStateNonMmap", func(t *testing.T) {
		pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
		require.NoError(t, err)
		defer pcm.Close()

		// Initial cached state is SETUP. KernelState should report this (or OPEN, depending on the driver).
		// The function gracefully falls back to the cached state if the underlying ioctl isn't supported.
		initialState := pcm.KernelState()
		assert.Contains(t, []alsa.PcmState{alsa.PCM_STATE_OPEN, alsa.PCM_STATE_SETUP}, initialState)

		require.NoError(t, pcm.Prepare())
		// After Prepare, cached state is PREPARED. Kernel should report the same.
		assert.Equal(t, alsa.PCM_STATE_PREPARED, pcm.KernelState(), "KernelState should be PREPARED after prepare")

		// Starting an empty stream is expected to cause an immediate underrun (EPIPE).
		// We call Start() but don't check the error, as EPIPE is the correct behavior here.
		_ = pcm.Start()

		// On a playback-only stream with no data, this will immediately underrun.
		// Give it a moment for the state to be detectable.
		time.Sleep(50 * time.Millisecond)

		// KernelState should now report XRUN, or PREPARED if the driver auto-stops on underrun (due to stop_threshold).
		finalState := pcm.KernelState()
		assert.Contains(t, []alsa.PcmState{alsa.PCM_STATE_XRUN, alsa.PCM_STATE_PREPARED}, finalState, "KernelState should be XRUN or PREPARED after starting an empty stream")
	})

	t.Run("KernelStateMmap", func(t *testing.T) {
		pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_MMAP, &defaultConfig)
		require.NoError(t, err)
		defer pcm.Close()

		// For MMAP streams, the status struct is mmapped, so KernelState should be accurate.
		// After setup, the state in the kernel's mmap region should be SETUP.
		initialState := pcm.KernelState()
		assert.Contains(t, []alsa.PcmState{alsa.PCM_STATE_OPEN, alsa.PCM_STATE_SETUP}, initialState)

		require.NoError(t, pcm.Prepare())
		assert.Equal(t, alsa.PCM_STATE_PREPARED, pcm.KernelState(), "KernelState should be PREPARED after prepare")

		// Starting an empty stream is expected to cause an immediate underrun (EPIPE).
		// We call Start() but don't check the error, as EPIPE is the correct behavior here.
		_ = pcm.Start()

		time.Sleep(50 * time.Millisecond) // Allow time for underrun

		// KernelState should now report XRUN, or PREPARED if the driver auto-stops on underrun.
		finalState := pcm.KernelState()
		assert.Contains(t, []alsa.PcmState{alsa.PCM_STATE_XRUN, alsa.PCM_STATE_PREPARED}, finalState, "KernelState should be XRUN or PREPARED after starting an empty mmap stream")
	})
}

func testPcmStop(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &defaultConfig)
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
				err := capturePcm.Read(buffer)
				if err != nil {
					// Expect an error when the stream is closed.
					return
				}
			}
		}
	}()

	// Write some data to start the stream
	buffer := make([]byte, alsa.PcmFramesToBytes(pcm, pcm.PeriodSize()))
	err = pcm.Write(buffer)
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

func testPcmWait(t *testing.T) {
	t.Run("Timeout", func(t *testing.T) {
		// Use a capture stream (PCM_IN) for the timeout test.
		pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &defaultConfig)
		require.NoError(t, err)
		defer pcm.Close()

		// A stream must be prepared before a meaningful wait for I/O can occur.
		require.NoError(t, pcm.Prepare())

		// On a prepared but non-running capture stream, Wait should time out as no data is available.
		ready, err := pcm.Wait(10) // 10ms timeout
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

	t.Run("Xrun", func(t *testing.T) {
		pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
		require.NoError(t, err)
		defer pcm.Close()

		require.NoError(t, pcm.Prepare(), "Failed to prepare PCM stream")

		// Start the stream without writing any data to force an underrun (XRUN).
		// The Start call itself might return EPIPE, but the Wait call after is the key test.
		_ = pcm.Start()

		// Give the hardware a moment to enter the XRUN state.
		// The sleep must be longer than the buffer duration to guarantee an underrun.
		bufferDurationMs := float64(pcm.BufferSize()) * 1000.0 / float64(pcm.Rate())
		// Add a margin to be safe.
		sleepDuration := time.Duration(bufferDurationMs+20) * time.Millisecond
		time.Sleep(sleepDuration)

		// Now, Wait should report the XRUN state by returning an EPIPE error.
		ready, err := pcm.Wait(100)
		assert.False(t, ready, "Ready should be false when an XRUN has occurred")
		require.Error(t, err, "Wait should return an error for an XRUN")
		assert.True(t, errors.Is(err, syscall.EPIPE), "Wait should return EPIPE for an XRUN, but got: %v", err)
		// The checkState function called by Wait should have detected the XRUN and incremented the counter.
		assert.GreaterOrEqual(t, pcm.Xruns(), 1, "Xruns counter should increment after an underrun")
	})
}

func testPcmXrunNoRestart(t *testing.T) {
	flags := alsa.PCM_OUT | alsa.PCM_NORESTART
	config := defaultConfig

	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), flags, &config)
	require.NoError(t, err)
	defer pcm.Close()

	require.NoError(t, pcm.Prepare())
	// Starting an empty stream will immediately cause an underrun (xrun)
	err = pcm.Start()
	// This should fail with EPIPE, indicating an xrun
	require.Error(t, err, "Start() should have failed with an xrun")
	assert.True(t, errors.Is(err, syscall.EPIPE), "Expected EPIPE on xrun, got %v", err)

	// Now try to write. Because the stream was opened with PCM_NORESTART,
	// it should remain in the XRUN state and not recover automatically. The write should fail.
	buffer := make([]byte, 128)
	frames := alsa.PcmBytesToFrames(pcm, uint32(len(buffer)))
	_, err = pcm.WriteI(buffer, frames)
	require.Error(t, err, "WriteI after an xrun with NORESTART should fail")

	// The error should again be EPIPE, because the stream is stuck in the xrun state.
	assert.True(t, errors.Is(err, syscall.EPIPE), "WriteI error should be EPIPE, got %v", err)
}

func testPcmPlaybackStartup(t *testing.T) {
	// This test ensures a playback-only stream can be started correctly by the first write,
	// without an explicit Start() call which would cause an immediate XRUN.
	config := defaultConfig
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &config)
	require.NoError(t, err, "Failed to open PCM for playback")
	defer pcm.Close()

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &config)
	require.NoError(t, err, "Could not open capture side of loopback")

	// Link them to ensure they start together. This is good practice for loopback tests.
	err = pcm.Link(capturePcm)
	if err != nil {
		capturePcm.Close() // Manually close if link fails before skipping.
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
		// But for robustness, we check for EBADF or EPIPE, which can happen during teardown.
		if readErr != nil && (!errors.Is(readErr, syscall.EBADF) && !errors.Is(readErr, syscall.EPIPE)) {
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

	// Wait for the capture goroutine to complete its read operation.
	wg.Wait()

	// Now that the goroutine is guaranteed to be finished, it's safe to close the capture PCM.
	capturePcm.Close()
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
	require.Equal(t, "", pcm.Error())

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

func testPcmTimestampFailsOnNonMmap(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	// This test is expected to fail on non-mmap streams.
	_, _, err = pcm.Timestamp()
	require.Error(t, err, "Timestamp() should fail on a non-MMAP stream")
	require.Contains(t, err.Error(), "only available for MMAP streams")
}

func testPcmHWPtrFailsOnNonMmap(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	// This should fail as HWPtr is only for MMAP streams.
	_, _, err = pcm.HWPtr()
	require.Error(t, err, "HWPtr() should fail on a non-MMAP stream")
	require.Contains(t, err.Error(), "only available for MMAP streams")
}

func testPcmWriteiFailsOnCapture(t *testing.T) {
	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &defaultConfig)
	require.NoError(t, err)
	defer capturePcm.Close()

	buffer := make([]byte, 128)
	_, err = capturePcm.WriteI(buffer, alsa.PcmBytesToFrames(capturePcm, uint32(len(buffer))))
	require.Error(t, err, "expected error when calling WriteI on a capture stream")
}

func testPcmReadiFailsOnPlayback(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	buffer := make([]byte, 128)
	_, err = pcm.ReadI(buffer, alsa.PcmBytesToFrames(pcm, uint32(len(buffer))))

	require.Error(t, err, "expected error when calling ReadI on a playback stream")
	require.Contains(t, err.Error(), "cannot read from a playback device")
}

func testPcmWriteNFailsOnCapture(t *testing.T) {
	config := defaultConfig
	config.Format = alsa.PCM_FORMAT_S16_LE // Use a specific format for typed slice

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_NONINTERLEAVED, &config)
	if err != nil {
		if errors.Is(err, syscall.EINVAL) {
			t.Skipf("Device does not support non-interleaved access, skipping test: %v", err)
		}
		require.NoError(t, err)
	}
	defer capturePcm.Close()

	// Use a typed slice (e.g., [][]int16) matching the format
	buffers := make([][]int16, config.Channels)
	samplesPerChannel := 128
	for i := range buffers {
		buffers[i] = make([]int16, samplesPerChannel)
	}

	_, err = capturePcm.WriteN(buffers, uint32(samplesPerChannel))
	require.Error(t, err, "expected error when calling WriteN on a capture stream")
}

func testPcmReadNFailsOnPlayback(t *testing.T) {
	config := defaultConfig
	config.Format = alsa.PCM_FORMAT_S16_LE // Use a specific format for typed slice

	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_NONINTERLEAVED, &config)
	if err != nil {
		if errors.Is(err, syscall.EINVAL) {
			t.Skipf("Device does not support non-interleaved access, skipping test: %v", err)
		}
		require.NoError(t, err)
	}
	defer pcm.Close()

	// Use a typed slice (e.g., [][]int16) matching the format
	buffers := make([][]int16, config.Channels)
	samplesPerChannel := 128
	for i := range buffers {
		buffers[i] = make([]int16, samplesPerChannel)
	}

	_, err = pcm.ReadN(buffers, uint32(samplesPerChannel))
	require.Error(t, err, "expected error when calling ReadN on a playback stream")
	require.Contains(t, err.Error(), "cannot read from a playback device")
}

func testPcmLoopbackNonInterleaved(t *testing.T) {
	// 1. Check if the hardware supports non-interleaved access.
	params, err := alsa.PcmParamsGetRefined(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT)
	if err != nil {
		t.Skipf("Could not get refined params to check for non-interleaved support, skipping: %v", err)
	}

	accessMask, err := params.Mask(alsa.PCM_PARAM_ACCESS)
	require.NoError(t, err)

	if !accessMask.Test(alsa.SNDRV_PCM_ACCESS_RW_NONINTERLEAVED) {
		t.Skip("Device does not support non-interleaved RW access")
	}

	// 2. Setup PCMs with non-interleaved flag.
	config := defaultConfig
	// The test uses S16_LE format, which is important for the typed slices.
	config.Format = alsa.PCM_FORMAT_S16_LE
	// Set a large start threshold to prevent underruns at the beginning of the stream.
	if config.PeriodCount > 1 {
		config.StartThreshold = config.PeriodSize * (config.PeriodCount - 1)
	} else {
		config.StartThreshold = config.PeriodSize
	}

	flagsOut := alsa.PCM_OUT | alsa.PCM_NONINTERLEAVED
	pcmOut, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), flagsOut, &config)
	require.NoError(t, err, "PcmOpen(playback, non-interleaved) failed")
	defer pcmOut.Close()

	// Capture side config must match
	flagsIn := alsa.PCM_IN | alsa.PCM_NONINTERLEAVED
	pcmIn, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), flagsIn, &config)
	require.NoError(t, err, "PcmOpen(capture, non-interleaved) failed")
	defer pcmIn.Close()

	// Link for synchronous start
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
	var readErr, writeErr error
	var readErrMtx, writeErrMtx sync.Mutex

	setReadErr := func(e error) {
		readErrMtx.Lock()
		defer readErrMtx.Unlock()
		if readErr == nil {
			readErr = e
		}
	}

	setWriteErr := func(e error) {
		writeErrMtx.Lock()
		defer writeErrMtx.Unlock()
		if writeErr == nil {
			writeErr = e
		}
	}

	wg.Add(2)

	// Reader goroutine
	go func() {
		defer wg.Done()
		framesPerPeriod := pcmIn.PeriodSize()
		readBuffer := make([][]int16, config.Channels)
		for c := range readBuffer {
			readBuffer[c] = make([]int16, framesPerPeriod)
		}

		for {
			select {
			case <-done:
				return
			default:
				// Read one period of data
				read, err := pcmIn.ReadN(readBuffer, framesPerPeriod)
				if err != nil {
					// EBADF can happen on shutdown, which is not an error.
					if errors.Is(err, syscall.EBADF) {
						return
					}
					setReadErr(fmt.Errorf("ReadN failed inside loop: %w", err))
					return
				}
				if read > 0 && uint32(read) != framesPerPeriod {
					setReadErr(fmt.Errorf("short read from ReadN: got %d, want %d", read, framesPerPeriod))
				}
			}
		}
	}()

	// Writer goroutine
	go func() {
		defer wg.Done()
		framesPerPeriod := pcmOut.PeriodSize()

		generator := newSineToneGenerator(config, 440, 0)
		interleavedSource := make([]byte, alsa.PcmFramesToBytes(pcmOut, framesPerPeriod))
		// Cast the byte slice to an int16 slice for easier manipulation
		interleavedSourceS16 := unsafe.Slice((*int16)(unsafe.Pointer(&interleavedSource[0])), len(interleavedSource)/2)

		nonInterleavedSource := make([][]int16, config.Channels)
		for c := range nonInterleavedSource {
			nonInterleavedSource[c] = make([]int16, framesPerPeriod)
		}

		for {
			select {
			case <-done:
				return
			default:
				// Generate and de-interleave one period of data
				generator.Read(interleavedSource)
				for f := uint32(0); f < framesPerPeriod; f++ {
					for c := uint32(0); c < config.Channels; c++ {
						srcIndex := f*config.Channels + c
						dstIndex := f
						nonInterleavedSource[c][dstIndex] = interleavedSourceS16[srcIndex]
					}
				}

				// Write one period of data
				written, err := pcmOut.WriteN(nonInterleavedSource, framesPerPeriod)
				if err != nil {
					if errors.Is(err, syscall.EBADF) {
						return
					}
					setWriteErr(fmt.Errorf("WriteN failed inside loop: %w", err))
					return
				}
				if uint32(written) != framesPerPeriod {
					setWriteErr(fmt.Errorf("short write from WriteN: got %d, want %d", written, framesPerPeriod))
				}
			}
		}
	}()

	// Run the producer-consumer test for a short duration
	time.Sleep(200 * time.Millisecond)
	close(done)
	wg.Wait()

	require.NoError(t, writeErr, "Write goroutine failed")
	require.NoError(t, readErr, "Read goroutine failed")
}

func testPcmWriteiTiming(t *testing.T) {
	config := defaultConfig
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
					// EBADF is expected if the main test closes the PCM before this goroutine exits.
					if errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.EPIPE) {
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
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &defaultConfig)
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
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &defaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	playbackPcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
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
					if errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.EPIPE) {
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
	bufferSize := alsa.PcmFramesToBytes(pcm, defaultConfig.PeriodSize)
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

	expectedDurationMs := float64(frames*readCount) * 1000.0 / float64(defaultConfig.Rate)
	durationMs := float64(duration.Milliseconds())

	tolerance := 150.0 // ms
	if (durationMs-expectedDurationMs) > tolerance || (expectedDurationMs-durationMs) > tolerance {
		t.Logf("ReadI timing test: got %.2f ms, want ~%.2f ms. This can be flaky.", durationMs, expectedDurationMs)
	}
}

func testPcmReadWriteSimple(t *testing.T) {
	config := defaultConfig
	pcmOut, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &config)
	require.NoError(t, err)
	defer pcmOut.Close() // Use defer for robust cleanup

	pcmIn, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &config)
	require.NoError(t, err)
	defer pcmIn.Close() // Use defer for robust cleanup

	err = pcmOut.Link(pcmIn)
	if err != nil {
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}
	defer pcmOut.Unlink()

	// Prepare streams before I/O to prevent race conditions on startup.
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

	// Reader goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		readBuffer := make([]byte, alsa.PcmFramesToBytes(pcmIn, pcmIn.PeriodSize()))
		for {
			select {
			case <-done:
				return
			default:
				err := pcmIn.Read(readBuffer)
				if err != nil {
					// EPIPE or EBADF are expected on shutdown
					if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EBADF) {
						return
					}
					setReaderErr(err)
					return
				}
			}
		}
	}()

	// Writer goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		writeBuffer := make([]byte, alsa.PcmFramesToBytes(pcmOut, pcmOut.PeriodSize()))
		for {
			select {
			case <-done:
				return
			default:
				err := pcmOut.Write(writeBuffer)
				if err != nil {
					// EPIPE or EBADF are expected on shutdown
					if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EBADF) {
						return
					}
					setWriterErr(err)
					return
				}
			}
		}
	}()

	// Run for a short period
	time.Sleep(200 * time.Millisecond)
	close(done)
	wg.Wait()

	// Now that the goroutines are finished, the deferred closes will happen.
	// Check for errors.
	readerErrMtx.Lock()
	require.NoError(t, readerErr, "Reader goroutine failed")
	readerErrMtx.Unlock()

	writerErrMtx.Lock()
	require.NoError(t, writerErr, "Writer goroutine failed")
	writerErrMtx.Unlock()
}

func testPcmMmapWrite(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_MMAP, &defaultConfig)
	require.NoError(t, err, "PcmOpen with MMAP failed")
	defer pcm.Close()

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_MMAP, &defaultConfig)
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

	// Test AvailMax on the prepared playback stream. Available space should be the full buffer,
	// so the "max" available frames in the buffer (pcm_avail_max) should be 0.
	availPlayback, err := pcm.AvailMax()
	if assert.NoError(t, err, "AvailMax failed on prepared playback stream") {
		assert.Equal(t, 0, availPlayback, "AvailMax should be 0 for a prepared, empty playback stream")
	}

	// Test AvailMax on the prepared capture stream. No data is available to read,
	// so the free space (returned by AvailMax for capture) should be the full buffer size.
	availCapture, errCapture := capturePcm.AvailMax()
	if assert.NoError(t, errCapture, "AvailMax failed on prepared capture stream") {
		assert.Equal(t, int(capturePcm.BufferSize()), availCapture, "AvailMax should be buffer size for a prepared, empty capture stream")
	}

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
					if errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.EPIPE) {
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
	bufferSize := alsa.PcmFramesToBytes(pcm, defaultConfig.PeriodSize)
	buffer := make([]byte, bufferSize)

	start := time.Now()
	for i := 0; i < writeCount; i++ {
		// The first MmapWrite will auto-start the linked streams.
		written, err := pcm.MmapWrite(buffer)
		if err != nil {
			if errors.Is(err, syscall.ENOTTY) {
				t.Skip("Skipping MMAP test: device does not support HWSYNC/SYNC_PTR (ENOTTY)")
			}

			// EAGAIN is okay, just continue the loop
			if errors.Is(err, syscall.EAGAIN) {
				i-- // Retry this iteration

				continue
			}

			t.Fatalf("MmapWrite failed on iteration %d: %v", i, err)
		}

		if written != len(buffer) {
			t.Fatalf("MmapWrite wrote %d bytes, want %d", written, len(buffer))
		}

		// After a successful write, the stream is guaranteed to be running.
		// Test Timestamp and HWPtr here, but only once to avoid spamming logs/overhead.
		if i == 1 {
			availFrames, _, err := pcm.Timestamp()
			if assert.NoError(t, err, "Timestamp() should succeed on a running MMAP stream") {
				assert.LessOrEqual(t, availFrames, pcm.BufferSize(), "Available frames should not exceed buffer size")
			}

			hwPtr, _, err := pcm.HWPtr()
			if assert.NoError(t, err, "HWPtr() should succeed on a running MMAP stream") {
				assert.Less(t, hwPtr, alsa.SndPcmUframesT(pcm.BufferSize()), "Hardware pointer should be within buffer size")
			}
		}
	}

	duration := time.Since(start)

	require.NoError(t, pcm.Stop())

	expectedFrames := uint32(writeCount) * defaultConfig.PeriodSize
	expectedDurationMs := float64(expectedFrames) * 1000.0 / float64(defaultConfig.Rate)
	durationMs := float64(duration.Milliseconds())

	tolerance := 250.0 // MMAP tests can have higher latency, increase tolerance
	if (durationMs-expectedDurationMs) > tolerance || (expectedDurationMs-durationMs) > tolerance {
		t.Logf("MmapWrite timing test: got %.2f ms, want ~%.2f ms. This can be flaky.", durationMs, expectedDurationMs)
	}
}

func testPcmMmapRead(t *testing.T) {
	config := defaultConfig
	config.Format = alsa.PCM_FORMAT_S16_LE
	// Set a large start threshold to prevent underruns at the beginning of the stream.
	if config.PeriodCount > 1 {
		config.StartThreshold = config.PeriodSize * (config.PeriodCount - 1)
	} else {
		config.StartThreshold = config.PeriodSize
	}

	// Robustness check: Ensure the loopback device supports the required format and MMAP access using PcmParamsGetRefined.
	playbackParams, err := alsa.PcmParamsGetRefined(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT)
	if err == nil {
		if !playbackParams.FormatIsSupported(config.Format) {
			t.Skipf("Playback device does not support format %s", alsa.PcmParamFormatNames[config.Format])
		}
		accessMask, err := playbackParams.Mask(alsa.PCM_PARAM_ACCESS)
		// Check for SNDRV_PCM_ACCESS_MMAP_INTERLEAVED (which is 0)
		if err == nil && !accessMask.Test(uint(0)) {
			t.Skip("Playback device does not support MMAP access")
		}
	}

	flagsOut := alsa.PCM_OUT | alsa.PCM_MMAP
	pcmOut, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), flagsOut, &config)
	require.NoError(t, err, "PcmOpen(playback, mmap) failed")
	defer pcmOut.Close() // Use defer for robust cleanup

	flagsIn := alsa.PCM_IN | alsa.PCM_MMAP
	pcmIn, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), flagsIn, &config)
	require.NoError(t, err, "PcmOpen(capture, mmap) failed")
	defer pcmIn.Close() // Use defer for robust cleanup

	err = pcmOut.Link(pcmIn)
	if err != nil {
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}
	defer pcmOut.Unlink()

	require.NoError(t, pcmOut.Prepare(), "playback stream prepare failed")
	require.NoError(t, pcmIn.Prepare(), "capture stream prepare failed")

	var wg sync.WaitGroup
	done := make(chan struct{})
	playbackStarted := make(chan struct{}) // Synchronization channel

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
		generator := newSineToneGenerator(config, 440, 0)
		buffer := make([]byte, alsa.PcmFramesToBytes(pcmOut, pcmOut.PeriodSize()))
		framesWrittenTotal := uint32(0)
		var signaled bool
		for {
			select {
			case <-done:
				return
			default:
				generator.Read(buffer)
				writtenBytes, err := pcmOut.MmapWrite(buffer)
				if err != nil {
					if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EBADF) {
						return
					}
					// EAGAIN means buffer is full, which is okay; just continue.
					if errors.Is(err, syscall.EAGAIN) {
						continue
					}
					setWriterErr(err)
					return
				}

				// Update total frames written
				framesWritten := alsa.PcmBytesToFrames(pcmOut, uint32(writtenBytes))
				framesWrittenTotal += framesWritten

				// Signal only when the start threshold is met, ensuring the stream is actually running the signal.
				if !signaled && framesWrittenTotal >= config.StartThreshold {
					close(playbackStarted)
					signaled = true
				}
			}
		}
	}()

	// Reader goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		readBuffer := make([]byte, alsa.PcmFramesToBytes(pcmIn, pcmIn.PeriodSize()))

		// Wait for the writer to signal that playback has actually started
		// before we begin polling for data. This avoids the race condition.
		select {
		case <-playbackStarted:
			// OK, writer has filled the buffer, we can start reading.
		case <-done:
			// Test finished before writer even started, nothing to do.
			return
		}

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
					// EAGAIN is not an error, just means no data is ready.
					if errors.Is(err, syscall.EAGAIN) {
						time.Sleep(1 * time.Millisecond) // Avoid busy-waiting
						continue
					}
					// EPIPE, EBADF are expected on shutdown
					if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EBADF) {
						return
					}
					setReaderErr(err)
					return
				}

				if read == 0 {
					continue
				}

				// Since we waited for playbackStarted, any data we now read
				// should contain the generated signal.
				energyMtx.Lock()
				if !energyFound {
					if energy(readBuffer[:read], config.Format) > 0 {
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
	pcmOut.Stop()

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
	t.Run("GetRefined", func(t *testing.T) {
		// Test getting params for a non-existent device
		params, err := alsa.PcmParamsGetRefined(1000, 1000, alsa.PCM_IN)
		require.Error(t, err, "expected error when getting params for non-existent device")
		require.Nil(t, params)

		// Test getting params for a valid device
		params, err = alsa.PcmParamsGetRefined(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT)
		require.NoError(t, err, "PcmParamsGetRefined failed for valid device")
		require.NotNil(t, params, "PcmParamsGetRefined returned nil params for valid device")

		// Test GetMask
		mask, err := params.Mask(alsa.PCM_PARAM_ACCESS)
		require.NoError(t, err, "Mask(PCM_PARAM_ACCESS) failed")
		assert.False(t, mask.Test(256), "mask.Test() should return false for bit >= 256")

		_, err = params.Mask(alsa.PCM_PARAM_SAMPLE_BITS) // Invalid param type for Mask
		require.Error(t, err, "expected error when getting mask for an interval parameter")

		// Test GetRange
		minVal, errMin := params.RangeMin(alsa.PCM_PARAM_RATE)
		maxVal, errMax := params.RangeMax(alsa.PCM_PARAM_RATE)
		require.NoError(t, errMin)
		require.NoError(t, errMax)
		// A device may only support a single rate, so check for >= instead of >.
		assert.GreaterOrEqual(t, maxVal, minVal, "expected rate max >= min for refined params")
		_, err = params.RangeMin(alsa.PCM_PARAM_ACCESS) // Invalid param type for Range
		require.Error(t, err, "expected error when getting range for a mask parameter")

		// Test FormatIsSupported for a common format.
		// The snd-aloop device may only support S16_LE by default, so we don't test for S32_LE.
		assert.True(t, params.FormatIsSupported(alsa.PCM_FORMAT_S16_LE), "expected PCM_FORMAT_S16_LE to be supported")

		// Test ToString
		s := params.String()
		require.NotEmpty(t, s)
		assert.NotEqual(t, "<nil>", s)
		assert.Contains(t, s, "PCM device capabilities", "String() output missing expected header")
		assert.Contains(t, s, "Access", "String() output missing 'Access' parameter")
		assert.Contains(t, s, "Rate", "String() output missing 'Rate' parameter")
		t.Log("\n" + s)
	})

	t.Run("GetDefaultsWithHwParams", func(t *testing.T) {
		// Test getting params for a non-existent device
		params, err := alsa.PcmParamsGet(1000, 1000, alsa.PCM_IN)
		require.Error(t, err, "expected error when getting params for non-existent device")
		require.Nil(t, params)

		// Test valid device for PcmParamsGet to get defaults
		params, err = alsa.PcmParamsGet(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT)
		if err != nil {
			if errors.Is(err, syscall.EINVAL) {
				t.Skipf("Skipping PcmParamsGet test: kernel does not support HW_PARAMS on zeroed struct (returned EINVAL): %v", err)
			}
			require.NoError(t, err, "PcmParamsGet failed for valid device")
		}
		require.NotNil(t, params, "PcmParamsGet returned nil params for valid device")

		// For default params, the range min and max should be equal, representing the single default value.
		rate, errMin := params.RangeMin(alsa.PCM_PARAM_RATE)
		rateMax, errMax := params.RangeMax(alsa.PCM_PARAM_RATE)
		require.NoError(t, errMin)
		require.NoError(t, errMax)
		assert.Equal(t, rate, rateMax, "Default params should have a single rate (min==max)")
		assert.NotZero(t, rate, "Default rate should not be zero")

		channels, err := params.RangeMin(alsa.PCM_PARAM_CHANNELS)
		require.NoError(t, err)
		assert.NotZero(t, channels, "Default channels should not be zero")

		// The mask for format will now only have one bit set for the default format.
		formatMask, err := params.Mask(alsa.PCM_PARAM_FORMAT)
		require.NoError(t, err)
		setBits := 0
		for i := 0; i < int(alsa.PCM_FORMAT_U18_3BE)+1; i++ {
			if formatMask.Test(uint(i)) {
				setBits++
			}
		}
		assert.Equal(t, 1, setBits, "Default params should specify exactly one format")

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

	// Try setting a new config.
	// We use a config that is likely to be supported by snd-aloop,
	// changing only the buffer geometry, as the device may have a fixed rate/channel count.
	// This tests the reconfiguration logic without failing on hardware limitations.
	newConfig := alsa.Config{
		Channels:    defaultConfig.Channels,
		Rate:        defaultConfig.Rate,
		PeriodSize:  512,
		PeriodCount: 2,
		Format:      alsa.PCM_FORMAT_S16_LE,
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
		// We log instead of failing hard.
		t.Logf("pcm1.Link(pcm2) failed: %v. This may not be supported on this system.", err)

		return
	}

	// If linking succeeds, unlinking should also succeed.
	require.NoError(t, pcm1.Unlink(), "pcm1.Unlink() failed")
}

func testPcmDrain(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &defaultConfig)
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
					if errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.EPIPE) {
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
	config := defaultConfig
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
		if errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.ENOSYS) {
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
	// Use PcmParamsGetRefined for a comprehensive check of capabilities.
	playbackParams, err := alsa.PcmParamsGetRefined(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT)
	if err != nil {
		// If refined fails, try the basic Get. If that fails, we must fail the test setup.
		playbackParams, err = alsa.PcmParamsGet(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT)
		require.NoError(t, err, "Failed to get params for loopback playback device")
	}

	captureParams, err := alsa.PcmParamsGetRefined(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN)
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

			config := defaultConfig
			config.Format = tf.format
			// Set a large start threshold to prevent underruns at the beginning of the stream.
			// This gives the writer goroutine time to run and fill the buffer before the
			// hardware needs the first sample. A threshold of buffer_size - 1 period is robust.
			if config.PeriodCount > 1 {
				config.StartThreshold = config.PeriodSize * (config.PeriodCount - 1)
			} else {
				config.StartThreshold = config.PeriodSize
			}

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
						read, err := pcmIn.ReadI(buffer, frames)
						if err != nil {
							// EPIPE means XRUN (overrun), EBADF can happen on close. These are expected during a racy shutdown/teardown.
							if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EBADF) {
								return
							}

							setCaptureErr(fmt.Errorf("the ReadI failed: %w", err))

							return
						}

						if read != int(frames) {
							setCaptureErr(fmt.Errorf("short read: got %d frames, want %d", read, frames))

							return
						}

						// Once data is flowing, we should detect the signal.
						if !energyFound {
							if energy(buffer, config.Format) > 0.0 {
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

				generator := newSineToneGenerator(config, 1000, 0) // 1kHz tone, 0dB
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
						written, err := pcmOut.WriteI(buffer, frames)
						if err != nil {
							// EPIPE means XRUN (underrun), EBADF can happen on close. These are expected during a racy shutdown/teardown.
							if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EBADF) {
								return
							}

							setPlaybackErr(fmt.Errorf("the WriteI failed on iteration %d: %w", counter, err))

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

			// Check for errors that occurred in the goroutines.
			require.NoError(t, captureErr, "Capture goroutine failed")
			require.NoError(t, playbackErr, "Playback goroutine failed")
		})
	}
}

func testPcmMmapLoopback(t *testing.T) {
	config := defaultConfig
	config.Format = alsa.PCM_FORMAT_S16_LE
	// Set a large start threshold to prevent underruns at the beginning of the stream.
	if config.PeriodCount > 1 {
		config.StartThreshold = config.PeriodSize * (config.PeriodCount - 1)
	} else {
		config.StartThreshold = config.PeriodSize
	}

	// Ensure the loopback device supports the required format and MMAP access.
	playbackParams, err := alsa.PcmParamsGet(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT)
	require.NoError(t, err)

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
		buffer := make([]byte, alsa.PcmFramesToBytes(pcmIn, pcmIn.PeriodSize()))
		var energyFound bool

		for {
			select {
			case <-done:
				if !energyFound {
					setCaptureErr(fmt.Errorf("test finished but no signal energy was ever detected"))
				}
				return
			default:
				// MmapRead will block internally via p.Wait() until data is ready.
				read, err := pcmIn.MmapRead(buffer)
				if err != nil {
					// These errors are expected during concurrent operation or shutdown.
					if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EBADF) {
						return
					}
					// EAGAIN might happen if the buffer is momentarily empty.
					if errors.Is(err, syscall.EAGAIN) {
						continue
					}

					setCaptureErr(fmt.Errorf("MmapRead failed: %w", err))
					return
				}
				if read == 0 {
					continue
				}

				if !energyFound {
					if energy(buffer[:read], config.Format) > 0.0 {
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

		generator := newSineToneGenerator(config, 1000, 0) // 1kHz tone, 0dB
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
					// These errors are expected during concurrent operation or shutdown.
					if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.EAGAIN) {
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

				counter++
			}
		}
	}()

	// Run for a short period to allow playback and capture.
	time.Sleep(250 * time.Millisecond)
	// Signal goroutines to stop, then wait for them before stopping the PCM stream.
	close(done)
	wg.Wait()
	// Now that the I/O goroutines are finished, it's safe to stop the stream.
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
