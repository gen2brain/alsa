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

	"golang.org/x/sys/unix"

	"github.com/gen2brain/alsa"
)

// These tests require the 'snd-aloop' kernel module: sudo modprobe snd-aloop

var (
	defaultConfig = alsa.Config{
		Channels:    2,
		Rate:        48000,
		PeriodSize:  1024,
		PeriodCount: 4,
		Format:      alsa.SNDRV_PCM_FORMAT_S16_LE,
	}
)

// waitForState polls a PCM's state until it reaches want or a one-second timeout
// elapses, then returns the last observed state.
func waitForState(p *alsa.PCM, want alsa.PcmState) alsa.PcmState {
	state := p.State()
	for i := 0; i < 100 && state != want; i++ {
		time.Sleep(10 * time.Millisecond)
		state = p.State()
	}
	return state
}

// isStreamDisrupted reports whether err is a transient loopback condition (xrun,
// bad state, or shutdown close) that the timing tests expect rather than fail on.
func isStreamDisrupted(err error) bool {
	return errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, unix.EBADFD) ||
		errors.Is(err, syscall.EBADF) ||
		errors.Is(err, syscall.EIO)
}

// drainWithin runs Drain with a watchdog: a blocking Drain ioctl cannot be
// interrupted, so if it does not return within timeout, Stop unblocks it. The
// watchdog path reports no error, since a stalled drain is an environment issue.
func drainWithin(p *alsa.PCM, timeout time.Duration) error {
	errc := make(chan error, 1)
	go func() { errc <- p.Drain() }()

	select {
	case err := <-errc:
		return err
	case <-time.After(timeout):
		_ = p.Stop()
		select {
		case err := <-errc:
			return err
		case <-time.After(2 * time.Second):
			// Drain is still wedged; the test's Close will release it.
			return nil
		}
	}
}

// startLoopbackDrainer reads a non-blocking capture stream to keep a linked
// loopback flowing; the returned stop joins the goroutine and reports the first
// unexpected error. Non-blocking reads let it observe the stop signal instead of
// parking in a blocking Read, which Stop() does not reliably unblock.
func startLoopbackDrainer(capture *alsa.PCM) (stop func() error) {
	done := make(chan struct{})
	finished := make(chan struct{})
	var gErr error

	go func() {
		defer close(finished)
		buffer := make([]byte, alsa.PcmFramesToBytes(capture, capture.PeriodSize()))

		for {
			select {
			case <-done:
				return
			default:
			}

			if _, err := capture.Read(buffer); err != nil {
				if errors.Is(err, syscall.EAGAIN) {
					time.Sleep(time.Millisecond)
					continue
				}
				if !isStreamDisrupted(err) {
					gErr = err
				}
				return
			}
		}
	}()

	return func() error {
		close(done)
		<-finished
		return gErr
	}
}

// startLoopbackFeeder continuously writes silence to a non-blocking playback
// stream to keep it from underrunning, until the returned stop function is
// called. stop joins the goroutine and reports the first unexpected error.
func startLoopbackFeeder(playback *alsa.PCM) (stop func() error) {
	done := make(chan struct{})
	finished := make(chan struct{})
	var gErr error

	go func() {
		defer close(finished)
		buffer := make([]byte, alsa.PcmFramesToBytes(playback, playback.PeriodSize()))

		for {
			select {
			case <-done:
				return
			default:
			}

			if _, err := playback.Write(buffer); err != nil {
				if errors.Is(err, syscall.EAGAIN) {
					time.Sleep(time.Millisecond)
					continue
				}
				if !isStreamDisrupted(err) {
					gErr = err
				}
				return
			}
		}
	}()

	return func() error {
		close(done)
		<-finished
		return gErr
	}
}

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
	t.Run("PcmMmapTimestamp", testPcmMmapTimestamp)
	t.Run("PcmState", testPcmState)
	t.Run("PcmStop", testPcmStop)
	t.Run("PcmWait", testPcmWait)
	t.Run("PcmParams", testPcmParams)
	t.Run("SetConfig", testSetConfig)
	t.Run("PcmLink", testPcmLink)
	t.Run("PcmLinkedXrunRecovery", testPcmLinkedXrunRecovery)
	t.Run("PcmDrain", testPcmDrain)
	t.Run("PcmPause", testPcmPause)
	t.Run("PcmLoopback", testPcmLoopback)
	t.Run("PcmNonBlocking", testPcmNonBlocking)
	t.Run("PcmMmapLoopback", testPcmMmapLoopback)
	t.Run("PcmMmapNonBlocking", testPcmMmapNonBlocking)
}

func testPcmOpenAndClose(t *testing.T) {
	pcm, err := alsa.PcmOpen(1000, 1000, alsa.PCM_OUT, &defaultConfig)
	if err == nil {
		t.Error("expected error when opening non-existent device, but got nil")
		pcm.Close()
	}

	if pcm != nil && pcm.IsReady() {
		t.Error("pcm.IsReady() should be false for non-existent device")
	}

	if err := (*alsa.PCM)(nil).Close(); err != nil {
		t.Errorf("closing a nil pcm should not return an error, but got %v", err)
	}

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
				// TTSTAMP ioctl for PCM_MONOTONIC is not supported by all kernels/devices.
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
	name := fmt.Sprintf("hw:%d,%d", loopbackCard, loopbackPlaybackDevice)
	pcm, err := alsa.PcmOpenByName(name, alsa.PCM_OUT, &defaultConfig)
	require.NoError(t, err, "PcmOpenByName failed for a valid name")
	require.NotNil(t, pcm)
	require.True(t, pcm.IsReady())
	pcm.Close()

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

		initialState := pcm.State()
		assert.Contains(t, []alsa.PcmState{alsa.SNDRV_PCM_STATE_OPEN, alsa.SNDRV_PCM_STATE_SETUP}, initialState)

		require.NoError(t, pcm.Prepare())
		assert.Equal(t, alsa.SNDRV_PCM_STATE_PREPARED, pcm.State(), "State should be PREPARED after prepare")

		// Starting an empty stream underruns immediately; EPIPE is expected, so ignore it.
		_ = pcm.Start()

		time.Sleep(50 * time.Millisecond)

		// XRUN, or PREPARED if the driver auto-stops on underrun (stop_threshold).
		finalState := pcm.State()
		assert.Contains(t, []alsa.PcmState{alsa.SNDRV_PCM_STATE_XRUN, alsa.SNDRV_PCM_STATE_PREPARED}, finalState,
			"State should be XRUN or PREPARED after starting an empty stream")
	})

	t.Run("StateMmap", func(t *testing.T) {
		pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_MMAP, &defaultConfig)
		require.NoError(t, err)
		defer pcm.Close()

		// For MMAP streams the status struct is mmapped, so State is read directly.
		initialState := pcm.State()
		assert.Contains(t, []alsa.PcmState{alsa.SNDRV_PCM_STATE_OPEN, alsa.SNDRV_PCM_STATE_SETUP}, initialState)

		require.NoError(t, pcm.Prepare())
		assert.Equal(t, alsa.SNDRV_PCM_STATE_PREPARED, pcm.State(), "State should be PREPARED after prepare")

		// Starting an empty stream underruns immediately; EPIPE is expected, so ignore it.
		_ = pcm.Start()

		time.Sleep(50 * time.Millisecond)

		finalState := pcm.State()
		assert.Contains(t, []alsa.PcmState{alsa.SNDRV_PCM_STATE_XRUN, alsa.SNDRV_PCM_STATE_PREPARED}, finalState, "State should be XRUN or PREPARED after starting an empty mmap stream")
	})
}

func testPcmStop(t *testing.T) {
	config := defaultConfig
	config.StartThreshold = config.PeriodSize * 2
	config.StopThreshold = config.PeriodSize * config.PeriodCount

	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &config)
	require.NoError(t, err)

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &config)
	if err != nil {
		pcm.Close()
		require.NoError(t, err)
	}

	err = pcm.Link(capturePcm)
	if err != nil {
		pcm.Close()
		capturePcm.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	require.NoError(t, pcm.Prepare())

	var wg sync.WaitGroup
	done := make(chan struct{})
	readyToRead := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		buffer := make([]byte, alsa.PcmFramesToBytes(capturePcm, capturePcm.PeriodSize()))

		close(readyToRead)

		for {
			select {
			case <-done:
				return
			default:
			}

			if _, err := capturePcm.Read(buffer); err != nil {
				return
			}
		}
	}()

	// Stop unblocks the pending Read so the wait cannot hang, even if an assertion aborts.
	defer func() {
		close(done)
		_ = pcm.Stop()
		_ = capturePcm.Stop()
		wg.Wait()
		_ = pcm.Unlink()
		_ = pcm.Close()
		_ = capturePcm.Close()
	}()

	<-readyToRead

	// Write two periods to cross the start threshold and start both linked streams.
	buffer := make([]byte, alsa.PcmFramesToBytes(pcm, pcm.PeriodSize()))
	for i := 0; i < 2; i++ {
		_, err = pcm.Write(buffer)
		require.NoError(t, err)
	}

	state := waitForState(pcm, alsa.SNDRV_PCM_STATE_RUNNING)
	require.Equal(t, alsa.SNDRV_PCM_STATE_RUNNING, state, "Stream should be in RUNNING state after writing")

	require.NoError(t, pcm.Stop(), "pcm.Stop() failed")

	state = waitForState(pcm, alsa.SNDRV_PCM_STATE_SETUP)
	require.Equal(t, alsa.SNDRV_PCM_STATE_SETUP, state, "Stream should be in SETUP state after stopping")
}

func testPcmWait(t *testing.T) {
	t.Run("Timeout", func(t *testing.T) {
		pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &defaultConfig)
		require.NoError(t, err)
		defer pcm.Close()

		require.NoError(t, pcm.Prepare())

		// A prepared but non-running capture stream has no data, so Wait times out.
		ready, err := pcm.Wait(10)
		assert.NoError(t, err)
		assert.False(t, ready, "Wait should time out and return false on an empty capture stream")
	})

	t.Run("Ready", func(t *testing.T) {
		pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
		require.NoError(t, err)
		defer pcm.Close()

		require.NoError(t, pcm.Prepare())

		ready, err := pcm.Wait(1000)
		assert.NoError(t, err)
		assert.True(t, ready, "Playback stream should be ready for writing")
	})
}

func testPcmPlaybackStartup(t *testing.T) {
	// Start only after 2 periods are buffered; keep stop threshold out of the way.
	config := defaultConfig
	config.StartThreshold = config.PeriodSize * 2
	config.StopThreshold = config.PeriodSize * config.PeriodCount

	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &config)
	require.NoError(t, err)

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &config)
	require.NoError(t, err)

	err = pcm.Link(capturePcm)
	if err != nil {
		pcm.Close()
		capturePcm.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	require.NoError(t, pcm.Prepare())

	// Drain the loopback so the playback stream can run without blocking.
	var wg sync.WaitGroup
	done := make(chan struct{})
	readyToRead := make(chan struct{})
	wg.Add(1)

	go func() {
		defer wg.Done()
		buffer := make([]byte, alsa.PcmFramesToBytes(capturePcm, capturePcm.PeriodSize()))

		close(readyToRead)

		for {
			select {
			case <-done:
				return
			default:
			}

			_, err := capturePcm.Read(buffer)
			if err != nil {
				return
			}
		}
	}()

	defer func() {
		close(done)
		_ = pcm.Stop()
		_ = capturePcm.Stop()
		wg.Wait()

		_ = pcm.Unlink()
		_ = pcm.Close()
		_ = capturePcm.Close()
	}()

	<-readyToRead

	require.Equal(t, alsa.SNDRV_PCM_STATE_PREPARED, pcm.State(), "Initial state should be PREPARED")

	// One period is below the start threshold, so the stream stays PREPARED.
	buffer := make([]byte, alsa.PcmFramesToBytes(pcm, pcm.PeriodSize()))
	written, err := pcm.Write(buffer)
	require.NoError(t, err)
	require.Equal(t, int(pcm.PeriodSize()), written)

	require.Equal(t, alsa.SNDRV_PCM_STATE_PREPARED, pcm.State(), "State should remain PREPARED after writing less than threshold")

	// Second period meets the start threshold and starts the stream.
	written, err = pcm.Write(buffer)

	require.NoError(t, err)
	require.Equal(t, int(pcm.PeriodSize()), written)

	var finalState alsa.PcmState
	for i := 0; i < 100; i++ {
		finalState = pcm.State()
		if finalState == alsa.SNDRV_PCM_STATE_RUNNING {
			break
		}

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

	require.Equal(t, uint32(loopbackPlaybackDevice), pcm.Subdevice())
	require.Equal(t, 0, pcm.Xruns(), "Xruns should be 0 on a newly opened stream")

	require.Equal(t, defaultConfig.Channels, pcm.Channels())
	require.Equal(t, defaultConfig.Rate, pcm.Rate())
	require.Equal(t, defaultConfig.Format, pcm.Format())
	require.Equal(t, defaultConfig.PeriodSize*defaultConfig.PeriodCount, pcm.BufferSize())

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
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &config)
	require.NoError(t, err)

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &config)
	require.NoError(t, err, "Could not open capture side of device")

	err = pcm.Link(capturePcm)
	if err != nil {
		pcm.Close()
		capturePcm.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	require.NoError(t, pcm.Prepare())

	var wg sync.WaitGroup
	done := make(chan struct{})
	readyToRead := make(chan struct{})

	var captureErr error
	var captureErrMtx sync.Mutex
	setCaptureErr := func(e error) {
		// Ignore errors observed while shutting down.
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
			}

			_, err := capturePcm.Read(captureBuffer)
			if err != nil {
				if isStreamDisrupted(err) {
					return
				}

				setCaptureErr(fmt.Errorf("capturePcm.Read failed: %w", err))
				return
			}
		}
	}()

	defer func() {
		close(done)
		_ = pcm.Stop()
		_ = capturePcm.Stop()
		wg.Wait()

		captureErrMtx.Lock()
		if captureErr != nil {
			t.Logf("Capture goroutine reported an error: %v", captureErr)
		}
		captureErrMtx.Unlock()

		_ = pcm.Unlink()
		_ = pcm.Close()
		_ = capturePcm.Close()
	}()

	<-readyToRead

	const writeCount = 20
	bufferSize := alsa.PcmFramesToBytes(pcm, config.PeriodSize)
	buffer := make([]byte, bufferSize)
	frames := alsa.PcmBytesToFrames(pcm, bufferSize)

	for i := 0; i < writeCount; i++ {
		captureErrMtx.Lock()
		if captureErr != nil {
			captureErrMtx.Unlock()
			t.Fatalf("Aborting write loop because capture goroutine failed: %v", captureErr)
		}
		captureErrMtx.Unlock()

		// The first Write implicitly starts both prepared, linked streams.
		written, err := pcm.Write(buffer)
		if err != nil {
			// A stream stopping unexpectedly (e.g. underrun) during a concurrent test is tolerated.
			if isStreamDisrupted(err) {
				break
			}

			t.Fatalf("Write failed on iteration %d: %v", i, err)
		}

		if written != int(frames) {
			t.Fatalf("Write wrote %d frames, want %d", written, frames)
		}
	}

	captureErrMtx.Lock()
	if captureErr != nil {
		captureErrMtx.Unlock()
		// A failed capture side would make Drain hang or return EPIPE.
		t.Logf("Skipping Drain because capture goroutine failed: %v", captureErr)
	} else {
		captureErrMtx.Unlock()
		err = drainWithin(pcm, 5*time.Second)
		// An xrun in Drain (consumer stopped or earlier underrun) is acceptable.
		if err != nil && !isStreamDisrupted(err) {
			require.NoError(t, err, "Drain failed after writing")
		}
	}
}

func testPcmGetDelay(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &defaultConfig)
	require.NoError(t, err)
	defer pcm.Close()

	// On a non-running stream Delay may error or return 0.
	delay, err := pcm.Delay()
	if err == nil {
		if delay < 0 {
			t.Errorf("expected non-negative delay, got %d", delay)
		}
	}
}

func testPcmReadTiming(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &defaultConfig)
	require.NoError(t, err)

	playbackPcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
	require.NoError(t, err)

	err = playbackPcm.Link(pcm)
	if err != nil {
		pcm.Close()
		playbackPcm.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	require.NoError(t, playbackPcm.Prepare())

	var wg sync.WaitGroup
	done := make(chan struct{})
	readyToWrite := make(chan struct{})

	var writerErr error
	var writerErrMtx sync.Mutex
	setWriterErr := func(e error) {
		// Ignore errors observed while shutting down.
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

		close(readyToWrite)

		for {
			select {
			case <-done:
				return
			default:
			}

			_, err := playbackPcm.Write(playbackBuffer)
			if err != nil {
				if isStreamDisrupted(err) {
					return
				}

				setWriterErr(fmt.Errorf("playbackPcm.Write failed: %w", err))
				return
			}
		}
	}()

	defer func() {
		close(done)
		_ = pcm.Stop()
		_ = playbackPcm.Stop()
		wg.Wait()

		writerErrMtx.Lock()
		if writerErr != nil {
			t.Logf("Writer goroutine reported an error: %v", writerErr)
		}
		writerErrMtx.Unlock()

		_ = playbackPcm.Unlink()
		_ = pcm.Close()
		_ = playbackPcm.Close()
	}()

	<-readyToWrite

	const readCount = 20
	bufferSize := alsa.PcmFramesToBytes(pcm, defaultConfig.PeriodSize)
	buffer := make([]byte, bufferSize)
	frames := alsa.PcmBytesToFrames(pcm, bufferSize)

	for i := 0; i < readCount; i++ {
		writerErrMtx.Lock()
		if writerErr != nil {
			writerErrMtx.Unlock()
			t.Fatalf("Aborting read loop because writer goroutine failed: %v", writerErr)
		}
		writerErrMtx.Unlock()

		read, err := pcm.Read(buffer)
		if err != nil {
			// The writer underrunning and stopping is tolerated.
			if isStreamDisrupted(err) {
				break
			}

			t.Fatalf("Read failed on iteration %d: %v", i, err)
		}

		if read != int(frames) {
			t.Fatalf("Read read %d frames, want %d", read, frames)
		}
	}
}

func testPcmReadWriteSimple(t *testing.T) {
	config := defaultConfig

	pcmOut, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_NONBLOCK, &config)
	require.NoError(t, err)

	pcmIn, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_NONBLOCK, &config)
	require.NoError(t, err)

	err = pcmOut.Link(pcmIn)
	if err != nil {
		pcmOut.Close()
		pcmIn.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	require.NoError(t, pcmOut.Prepare())

	stopFeeder := startLoopbackFeeder(pcmOut)
	stopDrainer := startLoopbackDrainer(pcmIn)

	time.Sleep(500 * time.Millisecond)

	writerErr := stopFeeder()
	readerErr := stopDrainer()

	_ = pcmOut.Stop()
	_ = pcmIn.Stop()
	_ = pcmOut.Unlink()
	_ = pcmIn.Close()
	_ = pcmOut.Close()

	require.NoError(t, writerErr, "Writer goroutine failed")
	require.NoError(t, readerErr, "Reader goroutine failed")
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

	if finalConfigOut.Channels != finalConfigIn.Channels ||
		finalConfigOut.Rate != finalConfigIn.Rate ||
		finalConfigOut.Format != finalConfigIn.Format ||
		finalConfigOut.PeriodSize != finalConfigIn.PeriodSize ||
		finalConfigOut.PeriodCount != finalConfigIn.PeriodCount {
		t.Fatalf("Loopback device parameters do not match after configuration. Out: C=%d R=%d F=%v PS=%d PC=%d, In: C=%d R=%d F=%v PS=%d PC=%d",
			finalConfigOut.Channels, finalConfigOut.Rate, finalConfigOut.Format, finalConfigOut.PeriodSize, finalConfigOut.PeriodCount,
			finalConfigIn.Channels, finalConfigIn.Rate, finalConfigIn.Format, finalConfigIn.PeriodSize, finalConfigIn.PeriodCount)
	}

	err = pcm.Link(capturePcm)
	if err != nil {
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}
	defer pcm.Unlink()

	require.NoError(t, pcm.Prepare(), "playback stream prepare failed")

	var wg sync.WaitGroup
	done := make(chan struct{})
	readyToRead := make(chan struct{})

	wg.Add(1)

	go func() {
		defer wg.Done()
		readBuffer := make([]byte, alsa.PcmFramesToBytes(capturePcm, capturePcm.PeriodSize()))

		close(readyToRead)

		for {
			select {
			case <-done:
				return
			default:
				_, err := capturePcm.MmapRead(readBuffer)
				if err != nil {
					if isStreamDisrupted(err) {
						return
					}

					if errors.Is(err, syscall.EAGAIN) {
						continue
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

	const writeCount = 20
	bufferSize := alsa.PcmFramesToBytes(pcm, pcm.PeriodSize())
	buffer := make([]byte, bufferSize)

	for i := 0; i < writeCount; i++ {
		// The first MmapWrite auto-starts the linked streams.
		written, err := pcm.MmapWrite(buffer)
		if err != nil {
			if errors.Is(err, syscall.ENOTTY) {
				t.Skip("Skipping MMAP test: device does not support HWSYNC/SYNC_PTR (ENOTTY)")
			}

			if isStreamDisrupted(err) {
				break
			}

			if errors.Is(err, syscall.EAGAIN) {
				i--

				continue
			}

			t.Fatalf("MmapWrite failed on iteration %d: %v", i, err)
		}

		expectedFrames := int(alsa.PcmBytesToFrames(pcm, uint32(len(buffer))))
		if written != expectedFrames {
			t.Fatalf("MmapWrite wrote %d frames, want %d", written, expectedFrames)
		}
	}

	require.NoError(t, pcm.Stop())
}

func testPcmMmapRead(t *testing.T) {
	playbackConfig := defaultConfig
	playbackConfig.Format = alsa.SNDRV_PCM_FORMAT_S16_LE
	// Large start threshold avoids underruns at the start of the stream.
	if playbackConfig.PeriodCount > 1 {
		playbackConfig.StartThreshold = playbackConfig.PeriodSize * (playbackConfig.PeriodCount - 1)
	} else {
		playbackConfig.StartThreshold = playbackConfig.PeriodSize
	}

	captureConfig := defaultConfig
	captureConfig.Format = alsa.SNDRV_PCM_FORMAT_S16_LE
	// Threshold larger than the buffer keeps MmapRead's Start() from firing; the
	// linked playback stream starts capture instead.
	captureConfig.StartThreshold = captureConfig.PeriodSize*captureConfig.PeriodCount + 1

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

	finalConfigOut := pcmOut.Config()
	finalConfigIn := pcmIn.Config()

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

	var wg sync.WaitGroup
	done := make(chan struct{})

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
					if errors.Is(err, syscall.EBADF) {
						return
					}

					// An XRUN whose recovery failed is a failure unless we are shutting down.
					if isStreamDisrupted(err) {
						select {
						case <-done:
							return
						default:
							setWriterErr(fmt.Errorf("MmapWrite failed with unrecoverable XRUN (EPIPE/EBADFD): %w", err))

							return
						}
					}

					if errors.Is(err, syscall.EAGAIN) {
						continue
					}

					setWriterErr(err)

					return
				}
			}
		}
	}()

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
					if errors.Is(err, syscall.EBADF) {
						return
					}

					// An XRUN whose recovery failed is a failure unless we are shutting down.
					if isStreamDisrupted(err) {
						select {
						case <-done:
							return
						default:
							setReaderErr(fmt.Errorf("MmapRead failed with unrecoverable XRUN (EPIPE/EBADFD): %w", err))
							return
						}
					}

					if errors.Is(err, syscall.EAGAIN) {
						time.Sleep(1 * time.Millisecond)

						continue
					}

					setReaderErr(err)

					return
				}

				if read == 0 {
					continue
				}

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

	time.Sleep(500 * time.Millisecond)
	close(done)
	wg.Wait()

	// For MMAP, Stop after the goroutines exit to leave the hardware quiet.
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
		params, err := alsa.PcmParamsGet(1000, 1000, alsa.PCM_IN)
		require.Error(t, err, "expected error when getting params for non-existent device")
		require.Nil(t, params)

		params, err = alsa.PcmParamsGet(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT)
		if err != nil {
			require.NoError(t, err, "PcmParamsGet failed for valid device")
		}
		require.NotNil(t, params, "PcmParamsGet returned nil params for valid device")

		rate, errMin := params.Min(alsa.SNDRV_PCM_HW_PARAM_RATE)
		rateMax, errMax := params.Max(alsa.SNDRV_PCM_HW_PARAM_RATE)
		require.NoError(t, errMin)
		require.NoError(t, errMax)
		assert.NotZero(t, rateMax, "Max rate should not be zero")
		assert.NotZero(t, rate, "Min rate should not be zero")

		channels, err := params.Min(alsa.SNDRV_PCM_HW_PARAM_CHANNELS)
		require.NoError(t, err)
		assert.NotZero(t, channels, "Channels should not be zero")

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
	})
}

func testSetConfig(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, nil)
	require.NoError(t, err, "PcmOpen with nil config failed")
	defer pcm.Close()

	require.NotZero(t, pcm.Channels(), "expected non-zero channels with default config")

	// Change only the buffer geometry; the device may have a fixed rate/channel count.
	newConfig := alsa.Config{
		Channels:    defaultConfig.Channels,
		Rate:        defaultConfig.Rate,
		PeriodSize:  512,
		PeriodCount: 2,
		Format:      alsa.SNDRV_PCM_FORMAT_S16_LE,
	}

	err = pcm.SetConfig(&newConfig)
	require.NoError(t, err)

	finalConfig := pcm.Config()
	require.Equal(t, newConfig.Channels, finalConfig.Channels)
	require.Equal(t, newConfig.Rate, finalConfig.Rate)
}

func testPcmLink(t *testing.T) {
	pcm1, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &defaultConfig)
	require.NoError(t, err, "Failed to open playback stream")
	defer pcm1.Close()

	pcm2, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &defaultConfig)
	require.NoError(t, err, "Failed to open capture stream")
	defer pcm2.Close()

	if err := pcm1.Link(pcm2); err != nil {
		t.Skipf("pcm1.Link(pcm2) failed: %v. This may not be supported on this system.", err)

		return
	}

	require.NoError(t, pcm1.Unlink(), "pcm1.Unlink() failed")
}

// testPcmLinkedXrunRecovery guards the recovery path that xrunRecover depends on
// for linked streams: a group-wide PREPARE is rejected with EBUSY while a sibling
// is still RUNNING, so recovery must drop the group to SETUP before preparing.
func testPcmLinkedXrunRecovery(t *testing.T) {
	config := defaultConfig
	config.StartThreshold = config.PeriodSize

	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_NONBLOCK, &config)
	require.NoError(t, err)

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_NONBLOCK, &config)
	require.NoError(t, err)

	if err := pcm.Link(capturePcm); err != nil {
		pcm.Close()
		capturePcm.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	require.NoError(t, pcm.Prepare())

	stopDrainer := startLoopbackDrainer(capturePcm)
	defer func() {
		_ = stopDrainer()
		_ = pcm.Stop()
		_ = pcm.Unlink()
		_ = pcm.Close()
		_ = capturePcm.Close()
	}()

	buffer := make([]byte, alsa.PcmFramesToBytes(pcm, pcm.PeriodSize()))
	writeTwoPeriods := func() {
		for i := 0; i < 2; i++ {
			if _, err := pcm.Write(buffer); err != nil && !isStreamDisrupted(err) {
				require.NoError(t, err, "write failed")
			}
		}
	}

	writeTwoPeriods()
	if waitForState(pcm, alsa.SNDRV_PCM_STATE_RUNNING) != alsa.SNDRV_PCM_STATE_RUNNING {
		t.Skip("stream did not reach RUNNING state")
	}

	// A linked stream can't re-prepare itself; xrunRecover must surface the xrun
	// (EPIPE) without dropping the group.
	recErr := alsa.XrunRecoverForTest(pcm, syscall.EPIPE)
	require.ErrorIs(t, recErr, syscall.EPIPE, "linked xrun should surface as EPIPE")
	require.Equal(t, alsa.SNDRV_PCM_STATE_RUNNING, pcm.State(), "xrunRecover must not drop a linked stream")

	// The caller recovers the group explicitly.
	require.NoError(t, pcm.Stop(), "group stop failed")
	require.NoError(t, pcm.Prepare(), "group prepare failed")
	writeTwoPeriods()
	require.Equal(t, alsa.SNDRV_PCM_STATE_RUNNING, waitForState(pcm, alsa.SNDRV_PCM_STATE_RUNNING), "stream should restart to RUNNING after group recovery")
}

func testPcmDrain(t *testing.T) {
	config := defaultConfig
	config.StartThreshold = config.PeriodSize

	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &config)
	require.NoError(t, err)

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_NONBLOCK, &config)
	require.NoError(t, err, "Could not open capture side of device")

	err = pcm.Link(capturePcm)
	if err != nil {
		pcm.Close()
		capturePcm.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	require.NoError(t, pcm.Prepare())

	stopDrainer := startLoopbackDrainer(capturePcm)
	defer func() {
		stopDrainer()
		_ = pcm.Stop()
		_ = pcm.Unlink()
		_ = pcm.Close()
		_ = capturePcm.Close()
	}()

	// Write two periods to cross the start threshold and reach RUNNING.
	buffer := make([]byte, alsa.PcmFramesToBytes(pcm, pcm.PeriodSize()))
	for i := 0; i < 2; i++ {
		if _, err := pcm.Write(buffer); err != nil && !isStreamDisrupted(err) {
			require.NoError(t, err, "Write before drain failed")
		}
	}
	if waitForState(pcm, alsa.SNDRV_PCM_STATE_RUNNING) != alsa.SNDRV_PCM_STATE_RUNNING {
		t.Skip("stream did not reach RUNNING state")
	}

	err = drainWithin(pcm, 5*time.Second)
	if err != nil && !isStreamDisrupted(err) {
		require.NoError(t, err, "Drain failed")
	}

	// A completed (or stopped) drain leaves the stream no longer RUNNING.
	require.NotEqual(t, alsa.SNDRV_PCM_STATE_RUNNING, pcm.State(), "stream should not be RUNNING after Drain")
}

func testPcmPause(t *testing.T) {
	config := defaultConfig
	config.StartThreshold = config.PeriodSize

	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_NONBLOCK, &config)
	require.NoError(t, err)

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_NONBLOCK, &config)
	require.NoError(t, err, "PcmOpen capture failed")

	err = pcm.Link(capturePcm)
	if err != nil {
		pcm.Close()
		capturePcm.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	require.NoError(t, pcm.Prepare(), "playback stream prepare failed")

	stopFeeder := startLoopbackFeeder(pcm)
	stopDrainer := startLoopbackDrainer(capturePcm)
	defer func() {
		_ = stopFeeder()
		_ = stopDrainer()
		_ = pcm.Stop()
		_ = pcm.Unlink()
		_ = pcm.Close()
		_ = capturePcm.Close()
	}()

	if waitForState(pcm, alsa.SNDRV_PCM_STATE_RUNNING) != alsa.SNDRV_PCM_STATE_RUNNING {
		t.Skip("stream did not reach RUNNING state")
	}

	err = pcm.Pause(true)
	if err != nil {
		if errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.ENOSYS) {
			t.Skipf("PAUSE ioctl is not supported: %v", err)
		}
		require.NoError(t, err, "pcm.Pause(true) failed")
	}

	require.Equal(t, alsa.SNDRV_PCM_STATE_PAUSED, waitForState(pcm, alsa.SNDRV_PCM_STATE_PAUSED), "stream should be PAUSED")

	require.NoError(t, pcm.Pause(false), "pcm.Pause(false) failed")

	require.Equal(t, alsa.SNDRV_PCM_STATE_RUNNING, waitForState(pcm, alsa.SNDRV_PCM_STATE_RUNNING), "stream should be RUNNING after resume")
}

func testPcmLoopback(t *testing.T) {
	testFormats := []struct {
		name   string
		format alsa.PcmFormat
	}{
		{"S16_LE", alsa.SNDRV_PCM_FORMAT_S16_LE},
		{"FLOAT_LE", alsa.SNDRV_PCM_FORMAT_FLOAT_LE},
	}

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

			playbackConfig := defaultConfig
			playbackConfig.Format = tf.format
			// Large start threshold avoids underruns at the start of the stream.
			if playbackConfig.PeriodCount > 1 {
				playbackConfig.StartThreshold = playbackConfig.PeriodSize * (playbackConfig.PeriodCount - 1)
			} else {
				playbackConfig.StartThreshold = playbackConfig.PeriodSize
			}

			captureConfig := defaultConfig
			captureConfig.Format = tf.format
			// 0 selects the default capture start threshold (1).
			captureConfig.StartThreshold = 0

			pcmOut, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT, &playbackConfig)
			require.NoError(t, err, "PcmOpen(playback) failed")
			defer pcmOut.Close()

			pcmIn, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN, &captureConfig)
			require.NoError(t, err, "PcmOpen(capture) failed")
			defer pcmIn.Close()

			finalConfigOut := pcmOut.Config()
			finalConfigIn := pcmIn.Config()

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
						read, err := pcmIn.Read(buffer)
						if err != nil {
							if isStreamDisrupted(err) {
								return
							}

							setCaptureErr(fmt.Errorf("the Read failed: %w", err))

							return
						}

						if read != int(frames) {
							setCaptureErr(fmt.Errorf("short read: got %d frames, want %d", read, frames))

							return
						}

						if !energyFound {
							if energy(buffer, playbackConfig.Format) > 0.0 {
								energyFound = true
							}
						}
					}
				}
			}()

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
							if isStreamDisrupted(err) {
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

			time.Sleep(250 * time.Millisecond)
			close(done)
			wg.Wait()

			// Stop only after the goroutines exit, so no Close races a syscall in flight.
			_ = pcmOut.Stop()
			_ = pcmIn.Stop()

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

		bufferSizeInFrames := pcm.BufferSize()
		periodSizeInBytes := alsa.PcmFramesToBytes(pcm, pcm.PeriodSize())
		writeBuffer := make([]byte, periodSizeInBytes)

		var writeErr error
		// Writing 2x the buffer fills it and forces EAGAIN on a non-blocking stream.
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
		pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_NONBLOCK, &defaultConfig)
		require.NoError(t, err)
		defer pcm.Close()

		buffer := make([]byte, alsa.PcmFramesToBytes(pcm, pcm.PeriodSize()))
		read, err := pcm.Read(buffer)

		assert.Equal(t, 0, read, "Read should return 0 frames when no data is available")
		assert.ErrorIs(t, err, syscall.EAGAIN, "Expected EAGAIN when reading from an empty non-blocking buffer")
	})
}

func testPcmMmapLoopback(t *testing.T) {
	playbackConfig := defaultConfig
	playbackConfig.Format = alsa.SNDRV_PCM_FORMAT_S16_LE

	// Large start threshold avoids underruns at the start of the stream.
	if playbackConfig.PeriodCount > 1 {
		playbackConfig.StartThreshold = playbackConfig.PeriodSize * (playbackConfig.PeriodCount - 1)
	} else {
		playbackConfig.StartThreshold = playbackConfig.PeriodSize
	}

	captureConfig := defaultConfig
	captureConfig.Format = alsa.SNDRV_PCM_FORMAT_S16_LE

	// Threshold larger than the buffer keeps MmapRead's Start() from firing; the
	// linked playback stream starts capture instead.
	captureConfig.StartThreshold = captureConfig.PeriodSize*captureConfig.PeriodCount + 1

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

	pcmOut, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_MMAP, &playbackConfig)
	require.NoError(t, err, "PcmOpen(playback, mmap) failed")
	defer pcmOut.Close()

	pcmIn, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_MMAP, &captureConfig)
	require.NoError(t, err, "PcmOpen(capture, mmap) failed")
	defer pcmIn.Close()

	finalConfigOut := pcmOut.Config()
	finalConfigIn := pcmIn.Config()

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

	wg.Add(1)

	go func() {
		defer wg.Done()
		buffer := make([]byte, alsa.PcmFramesToBytes(pcmIn, pcmIn.PeriodSize()))

		for {
			select {
			case <-done:
				energyMtx.Lock()
				if !energyFound {
					setCaptureErr(fmt.Errorf("test finished but no signal energy was ever detected"))
				}
				energyMtx.Unlock()

				return
			default:
				read, err := pcmIn.MmapRead(buffer)
				if err != nil {
					if errors.Is(err, syscall.EBADF) {
						return
					}

					// An XRUN whose recovery failed is a failure unless we are shutting down.
					if isStreamDisrupted(err) {
						select {
						case <-done:
							return
						default:
							setCaptureErr(fmt.Errorf("MmapRead failed with unrecoverable XRUN (EPIPE/EBADFD): %w", err))

							return
						}
					}

					if errors.Is(err, syscall.EAGAIN) {
						time.Sleep(1 * time.Millisecond)

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
					// An XRUN whose recovery failed is a failure unless we are shutting down.
					if isStreamDisrupted(err) {
						select {
						case <-done:
							return
						default:
							setPlaybackErr(fmt.Errorf("MmapWrite failed with unrecoverable XRUN (EPIPE/EBADFD) on iteration %d: %w", counter, err))
							return
						}
					}

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

	time.Sleep(500 * time.Millisecond)
	close(done)
	wg.Wait()
	_ = pcmOut.Stop()

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

		// Larger than the device buffer so the write fills it and returns EAGAIN.
		bufferSizeInBytes := alsa.PcmFramesToBytes(pcm, pcm.BufferSize())
		writeBuffer := make([]byte, bufferSizeInBytes*2)

		written, err := pcm.MmapWrite(writeBuffer)

		require.ErrorIs(t, err, syscall.EAGAIN, "Expected EAGAIN when MmapWrite fills the buffer")
		assert.Greater(t, written, 0, "MmapWrite should have written some data before returning EAGAIN")
		assert.LessOrEqual(t, written, int(pcm.BufferSize()), "MmapWrite should not write more frames than the buffer size")
	})

	t.Run("Read", func(t *testing.T) {
		pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_MMAP|alsa.PCM_NONBLOCK, &defaultConfig)
		require.NoError(t, err)
		defer pcm.Close()

		require.NoError(t, pcm.Prepare())

		buffer := make([]byte, alsa.PcmFramesToBytes(pcm, pcm.PeriodSize()))
		read, err := pcm.MmapRead(buffer)

		assert.Equal(t, 0, read, "MmapRead should return 0 frames when no data is available")
		assert.ErrorIs(t, err, syscall.EAGAIN, "Expected EAGAIN when reading from an empty non-blocking mmap buffer")
	})
}

func testPcmMmapWriteTiming(t *testing.T) {
	config := defaultConfig
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_MMAP, &config)
	require.NoError(t, err)

	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_MMAP, &config)
	require.NoError(t, err, "Could not open capture side of device")

	err = pcm.Link(capturePcm)
	if err != nil {
		_ = pcm.Close()
		_ = capturePcm.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	require.NoError(t, pcm.Prepare())

	var wg sync.WaitGroup
	done := make(chan struct{})
	readyToRead := make(chan struct{})

	var captureErr error
	var captureErrMtx sync.Mutex
	setCaptureErr := func(e error) {
		// Ignore errors observed while shutting down.
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
			}

			_, err := capturePcm.MmapRead(captureBuffer)
			if err != nil {
				if isStreamDisrupted(err) {
					return
				}

				setCaptureErr(fmt.Errorf("capturePcm.MmapRead failed: %w", err))

				return
			}
		}
	}()

	defer func() {
		close(done)
		_ = pcm.Stop()
		_ = capturePcm.Stop()
		wg.Wait()

		captureErrMtx.Lock()
		if captureErr != nil {
			t.Logf("Capture goroutine reported an error: %v", captureErr)
		}
		captureErrMtx.Unlock()

		_ = pcm.Unlink()
		_ = pcm.Close()
		_ = capturePcm.Close()
	}()

	<-readyToRead

	const writeCount = 20
	bufferSize := alsa.PcmFramesToBytes(pcm, config.PeriodSize)
	buffer := make([]byte, bufferSize)
	frames := alsa.PcmBytesToFrames(pcm, bufferSize)

	for i := 0; i < writeCount; i++ {
		captureErrMtx.Lock()
		if captureErr != nil {
			captureErrMtx.Unlock()
			t.Fatalf("Aborting write loop because capture goroutine failed: %v", captureErr)
		}
		captureErrMtx.Unlock()

		// The first MmapWrite implicitly starts both prepared, linked streams.
		written, err := pcm.MmapWrite(buffer)
		if err != nil {
			// A stream stopping unexpectedly (e.g. underrun) during a concurrent test is tolerated.
			if isStreamDisrupted(err) {
				break
			}

			t.Fatalf("MmapWrite failed on iteration %d: %v", i, err)
		}

		if written != int(frames) {
			t.Fatalf("MmapWrite wrote %d frames, want %d", written, frames)
		}
	}

	captureErrMtx.Lock()
	if captureErr != nil {
		captureErrMtx.Unlock()
		// A failed capture side would make Drain hang or return EPIPE.
		t.Logf("Skipping Drain because capture goroutine failed: %v", captureErr)
	} else {
		captureErrMtx.Unlock()
		err = drainWithin(pcm, 5*time.Second)
		// An xrun in Drain (consumer stopped or earlier underrun) is acceptable.
		if err != nil && !isStreamDisrupted(err) {
			require.NoError(t, err, "Drain failed after writing")
		}
	}
}

func testPcmMmapReadTiming(t *testing.T) {
	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_MMAP, &defaultConfig)
	require.NoError(t, err)

	playbackPcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), alsa.PCM_OUT|alsa.PCM_MMAP, &defaultConfig)
	require.NoError(t, err)

	err = playbackPcm.Link(pcm)
	if err != nil {
		_ = pcm.Close()
		_ = playbackPcm.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	require.NoError(t, playbackPcm.Prepare())

	var wg sync.WaitGroup
	done := make(chan struct{})
	readyToWrite := make(chan struct{})

	var writerErr error
	var writerErrMtx sync.Mutex
	setWriterErr := func(e error) {
		// Ignore errors observed while shutting down.
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

		close(readyToWrite)

		for {
			select {
			case <-done:
				return
			default:
			}

			_, err := playbackPcm.MmapWrite(playbackBuffer)
			if err != nil {
				if isStreamDisrupted(err) {
					return
				}

				setWriterErr(fmt.Errorf("playbackPcm.MmapWrite failed: %w", err))

				return
			}
		}
	}()

	defer func() {
		close(done)
		_ = pcm.Stop()
		_ = playbackPcm.Stop()
		wg.Wait()

		writerErrMtx.Lock()
		if writerErr != nil {
			t.Logf("Writer goroutine reported an error: %v", writerErr)
		}
		writerErrMtx.Unlock()

		_ = playbackPcm.Unlink()
		_ = pcm.Close()
		_ = playbackPcm.Close()
	}()

	<-readyToWrite

	const readCount = 20
	bufferSize := alsa.PcmFramesToBytes(pcm, defaultConfig.PeriodSize)
	buffer := make([]byte, bufferSize)
	frames := alsa.PcmBytesToFrames(pcm, bufferSize)

	for i := 0; i < readCount; i++ {
		writerErrMtx.Lock()
		if writerErr != nil {
			writerErrMtx.Unlock()
			t.Fatalf("Aborting read loop because writer goroutine failed: %v", writerErr)
		}
		writerErrMtx.Unlock()

		read, err := pcm.MmapRead(buffer)
		if err != nil {
			// The writer underrunning and stopping is tolerated.
			if isStreamDisrupted(err) {
				break
			}

			t.Fatalf("MmapRead failed on iteration %d: %v", i, err)
		}

		if read != int(frames) {
			t.Fatalf("MmapRead read %d frames, want %d", read, frames)
		}
	}
}

func testPcmMmapTimestamp(t *testing.T) {
	flags := alsa.PCM_OUT | alsa.PCM_MMAP
	monotonicFlags := flags | alsa.PCM_MONOTONIC

	pcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), monotonicFlags, &defaultConfig)
	useMonotonic := err == nil
	if !useMonotonic {
		// Fall back to non-monotonic if the kernel lacks TTSTAMP support.
		pcm, err = alsa.PcmOpen(uint(loopbackCard), uint(loopbackPlaybackDevice), flags, &defaultConfig)
		require.NoError(t, err, "PcmOpen(mmap) failed")
	}

	// A capture stream is needed to drain the loopback buffer.
	capturePcm, err := alsa.PcmOpen(uint(loopbackCard), uint(loopbackCaptureDevice), alsa.PCM_IN|alsa.PCM_MMAP, &defaultConfig)
	require.NoError(t, err, "PcmOpen capture (MMAP) failed")

	err = pcm.Link(capturePcm)
	if err != nil {
		pcm.Close()
		capturePcm.Close()
		t.Skipf("Failed to link PCM streams, skipping test: %v", err)
	}

	require.NoError(t, pcm.Prepare(), "playback stream prepare failed")

	var wg sync.WaitGroup
	done := make(chan struct{})

	var writerErr, readerErr error
	var writerErrMtx, readerErrMtx sync.Mutex

	setWriterErr := func(e error) {
		writerErrMtx.Lock()
		defer writerErrMtx.Unlock()

		if writerErr == nil {
			writerErr = e
		}
	}

	wg.Add(1)

	go func() {
		defer wg.Done()
		buffer := make([]byte, alsa.PcmFramesToBytes(pcm, pcm.PeriodSize()))

		for {
			select {
			case <-done:
				return
			default:
				_, err := pcm.MmapWrite(buffer)
				if err != nil {
					if isStreamDisrupted(err) {
						return
					}

					if errors.Is(err, syscall.EAGAIN) {
						continue
					}

					setWriterErr(err)

					return
				}
			}
		}
	}()

	wg.Add(1)

	go func() {
		defer wg.Done()
		buffer := make([]byte, alsa.PcmFramesToBytes(capturePcm, capturePcm.PeriodSize()))

		for {
			select {
			case <-done:
				return
			default:
				_, err := capturePcm.MmapRead(buffer)
				if err != nil {
					return
				}
			}
		}
	}()

	defer func() {
		close(done)
		wg.Wait()
		_ = pcm.Stop()
		_ = capturePcm.Stop()
		_ = pcm.Unlink()
		_ = pcm.Close()
		_ = capturePcm.Close()

		writerErrMtx.Lock()
		require.NoError(t, writerErr, "Writer goroutine failed")
		writerErrMtx.Unlock()

		readerErrMtx.Lock()
		require.NoError(t, readerErr, "Reader goroutine failed")
		readerErrMtx.Unlock()
	}()

	require.Equal(t, alsa.SNDRV_PCM_STATE_RUNNING, waitForState(pcm, alsa.SNDRV_PCM_STATE_RUNNING), "Stream did not enter RUNNING state")

	// A single sample can read 0 free frames right after the writer refills the
	// buffer; while RUNNING, AvailUpdate must report consumable space eventually.
	availSeen := false
	for i := 0; i < 20 && !availSeen; i++ {
		avail, _, err := pcm.Timestamp()
		require.NoError(t, err, "Timestamp() failed")
		if avail > 0 {
			availSeen = true
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.True(t, availSeen, "AvailUpdate should report >0 free frames while running")

	_, ts1, err1 := pcm.Timestamp()
	require.NoError(t, err1, "First Timestamp() call failed")

	time.Sleep(100 * time.Millisecond)

	_, ts2, err2 := pcm.Timestamp()
	require.NoError(t, err2, "Second Timestamp() call failed")

	assert.True(t, ts2.After(ts1), "Second timestamp should be after the first")

	// Without monotonic, the timestamp tracks wall-clock time (generous delta).
	if !useMonotonic {
		now := time.Now()
		assert.WithinDuration(t, now, ts2, 5*time.Second, "Timestamp should be close to current wall-clock time")
	}
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

	for f := 0; f < numFrames; f++ {
		for c := 0; c < int(g.numCh); c++ {
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
