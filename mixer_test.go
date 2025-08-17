package alsa_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gen2brain/alsa"
)

// To run these tests, the 'snd-dummy' kernel module must be loaded:
//
// sudo modprobe snd-dummy
//
// This creates virtual dummy sound cards that allow testing controls.

// TestMixerHardware runs all hardware-related tests sequentially to avoid race conditions.
func TestMixerHardware(t *testing.T) {
	t.Run("OpenAndClose", testMixerOpenAndClose)
	t.Run("Functionality", testMixerFunctionality)
}

// TestMixerInvalidParameters can run in parallel as it does not access hardware.
func TestMixerInvalidParameters(t *testing.T) {
	var nilMixer *alsa.Mixer
	var nilCtl *alsa.MixerCtl

	// Test functions on a nil Mixer object
	assert.NotPanics(t, func() {
		err := nilMixer.Close()
		assert.NoError(t, err)
	}, "Close on nil mixer should not panic")

	assert.Equal(t, "", nilMixer.Name(), "Name on nil mixer should be empty string")
	assert.Equal(t, 0, nilMixer.NumCtls(), "NumCtls on nil mixer should be 0")
	assert.Equal(t, 0, nilMixer.NumCtlsByName("test"), "NumCtlsByName on nil mixer should be 0")

	_, err := nilMixer.Ctl(0)
	assert.Error(t, err, "Ctl on nil mixer should return an error")

	_, err = nilMixer.CtlByName("test")
	assert.Error(t, err, "CtlByName on nil mixer should return an error")

	err = nilMixer.SubscribeEvents(true)
	assert.Error(t, err, "SubscribeEvents on nil mixer should return an error")

	_, err = nilMixer.WaitEvent(0)
	assert.Error(t, err, "WaitEvent on nil mixer should return an error")

	_, err = nilMixer.ReadEvent()
	assert.Error(t, err, "ReadEvent on nil mixer should return an error")

	err = nilMixer.ConsumeEvent()
	assert.Error(t, err, "ConsumeEvent on nil mixer should return an error")

	// Test functions on a nil MixerCtl object
	assert.Equal(t, "", nilCtl.Name(), "Name on nil ctl should be empty string")
	assert.NotEqual(t, uint32(0), nilCtl.ID(), "ID on nil ctl should be max_uint")
	assert.Equal(t, alsa.MIXER_CTL_TYPE_UNKNOWN, nilCtl.Type(), "Type on nil ctl should be UNKNOWN")
	assert.Equal(t, "UNKNOWN", nilCtl.TypeString(), "TypeString on nil ctl should be UNKNOWN")
	assert.Equal(t, uint32(0), nilCtl.NumValues(), "NumValues on nil ctl should be 0")

	_, err = nilCtl.Value(0)
	assert.Error(t, err, "Value on nil ctl should return an error")

	err = nilCtl.SetValue(0, 0)
	assert.Error(t, err, "SetValue on nil ctl should return an error")

	_, err = nilCtl.Percent(0)
	assert.Error(t, err, "Percent on nil ctl should return an error")

	err = nilCtl.SetPercent(0, 50)
	assert.Error(t, err, "SetPercent on nil ctl should return an error")

	err = nilCtl.Update()
	assert.Error(t, err, "Update on nil ctl should return an error")

	var data []int32
	err = nilCtl.Array(&data)
	assert.Error(t, err, "Array on nil ctl should return an error")

	err = nilCtl.Array(nil)
	assert.Error(t, err, "Array with nil argument should return an error")

	err = nilCtl.SetArray(data)
	assert.Error(t, err, "SetArray on nil ctl should return an error")

	err = nilCtl.SetArray(nil)
	assert.Error(t, err, "SetArray with nil argument should return an error")
}

func testMixerOpenAndClose(t *testing.T) {
	// Assume card 0 is always present
	mixer, err := alsa.MixerOpen(0)
	if err != nil {
		t.Skipf("Skipping open/close test for card 0: %v", err)
	}

	require.NotNil(t, mixer, "Mixer object should not be nil")
	err = mixer.Close()
	assert.NoError(t, err, "mixer_close() should succeed")

	// Attempt to open a card that is very unlikely to exist
	mixer, err = alsa.MixerOpen(1000)
	assert.Error(t, err, "mixer_open(1000) should fail")
	assert.Nil(t, mixer, "Mixer object should be nil on failure")
}

func testMixerFunctionality(t *testing.T) {
	t.Run(fmt.Sprintf("Card%d", dummyCard), func(t *testing.T) {
		runCardTests(t, uint(dummyCard))
	})
}

func runCardTests(t *testing.T, card uint) {
	mixer, err := alsa.MixerOpen(card)
	if err != nil {
		t.Skipf("Skipping tests for card %d: cannot open mixer: %v", card, err)

		return
	}
	defer mixer.Close()

	t.Run("MixerInfo", func(t *testing.T) { testMixerInfo(t, mixer) })
	t.Run("MixerBasicFunctionality", func(t *testing.T) { testMixerBasicFunctionality(t, mixer) })
	t.Run("Events", func(t *testing.T) { testMixerEvents(t, mixer) })
	t.Run("ControlAccess", func(t *testing.T) { testControlAccess(t, mixer) })
	t.Run("ControlProperties", func(t *testing.T) { testControlProperties(t, mixer) })
	t.Run("ControlValues", func(t *testing.T) { testControlValues(t, mixer) })
	t.Run("ControlNotFound", func(t *testing.T) { testMixerControlNotFound(t, mixer) })
	t.Run("CtlByNameAndDevice", func(t *testing.T) { testMixerCtlByNameAndDevice(t, mixer) })
	t.Run("CtlTypeString", func(t *testing.T) { testMixerCtlTypeString(t, mixer) })
}

func testMixerInfo(t *testing.T, m *alsa.Mixer) {
	// Test Name
	name := m.Name()
	assert.NotEmpty(t, name, "mixer.Name() should return a non-empty string")

	// Test NumberOfControls
	numCtls := m.NumCtls()

	t.Logf("Card '%s' has %d controls.", name, numCtls)

	// A standard snd-dummy card should have controls. If this is 0, enumeration failed.
	require.Greater(t, numCtls, 0, "mixer enumeration returned 0 controls; it likely failed. The 'snd-dummy' card should have controls.")

	// Test AddNewControls
	initialCount := m.NumCtls()
	err := m.AddNewCtls()
	assert.NoError(t, err, "mixer.AddNewCtls() should not return an error")
	assert.GreaterOrEqual(t, m.NumCtls(), initialCount, "Control count should not decrease after AddNewCtls")
}

func testMixerBasicFunctionality(t *testing.T, m *alsa.Mixer) {
	// Basic functionality checks
	if m.NumCtls() == 0 {
		t.Skip("No mixer controls found on dummy device")
	}

	// Try to access the first control if available
	if m.NumCtls() > 0 {
		ctl, err := m.CtlByIndex(0)
		if err != nil {
			t.Fatalf("Failed to get first control: %v", err)
		}

		// Basic control info access
		name := ctl.Name()
		ctlType := ctl.Type()
		t.Logf("First control: name='%s', type=%s", name, ctl.TypeString())

		// These operations should not panic or fail due to struct issues
		_ = ctl.NumValues()
		_ = ctl.Access()

		// Test type-specific operations based on a control type
		switch ctlType {
		case alsa.MIXER_CTL_TYPE_INT:
			if ctl.NumValues() > 0 {
				_, err := ctl.Value(0)
				if err != nil {
					t.Logf("Could not read integer control value (this may be normal): %v", err)
				}
			}
		case alsa.MIXER_CTL_TYPE_BOOL:
			if ctl.NumValues() > 0 {
				_, err := ctl.Value(0)
				if err != nil {
					t.Logf("Could not read boolean control value (this may be normal): %v", err)
				}
			}
		case alsa.MIXER_CTL_TYPE_ENUM:
			numEnums, err := ctl.NumEnums()
			if err != nil {
				t.Logf("Could not get enum count (this may be normal): %v", err)
			} else {
				t.Logf("Enum control has %d items", numEnums)
			}
		}
	}
}

func testControlAccess(t *testing.T, m *alsa.Mixer) {
	numCtls := m.NumCtls()
	if numCtls == 0 {
		t.Skip("Skipping control access tests: no controls found.")

		return
	}

	// Build a map for verifying name counts
	namesWithCounts := make(map[string]int)
	for _, ctl := range m.Ctls {
		namesWithCounts[ctl.Name()]++
	}

	// Test CtlByIndex out of bounds
	_, err := m.CtlByIndex(uint(numCtls))
	assert.Error(t, err, "CtlByIndex with out-of-bounds index should fail")

	// Test CtlByName, Ctl, NumCtlsByName, CtlByNameAndIndex
	visitedNames := make(map[string]int)
	for i, ctl := range m.Ctls {
		name := ctl.Name()
		id := ctl.ID()

		// Verify CtlByIndex
		ctlByIndex, err := m.CtlByIndex(uint(i))
		require.NoError(t, err)
		assert.Same(t, ctl, ctlByIndex, "CtlByIndex should return the correct control")

		// Verify Ctl(id)
		ctlByID, err := m.Ctl(id)
		assert.NoError(t, err, "Ctl(%d) should succeed", id)
		assert.Same(t, ctl, ctlByID, "Ctl(%d) should return the same control instance", id)

		// Verify NumCtlsByName
		count := m.NumCtlsByName(name)
		assert.Equal(t, namesWithCounts[name], count, "NumCtlsByName for '%s' should be correct", name)

		// Verify CtlByName (should get the first one)
		if visitedNames[name] == 0 {
			ctlByName, err := m.CtlByName(name)
			assert.NoError(t, err, "CtlByName('%s') should succeed", name)
			assert.Same(t, ctl, ctlByName, "CtlByName should return the first matching control")
		}

		// Verify CtlByNameAndIndex
		indexInNameGroup := visitedNames[name]
		ctlByNameAndIndex, err := m.CtlByNameAndIndex(name, uint(indexInNameGroup))
		assert.NoError(t, err, "CtlByNameAndIndex('%s', %d) should succeed", name, indexInNameGroup)
		assert.Same(t, ctl, ctlByNameAndIndex, "CtlByNameAndIndex returned wrong control for '%s' at index %d", name, indexInNameGroup)
		visitedNames[name]++
	}

	// Test non-existent name
	nonExistentName := "This Control Really Should Not Exist"
	assert.Equal(t, 0, m.NumCtlsByName(nonExistentName))
	_, err = m.CtlByName(nonExistentName)
	assert.Error(t, err)
}

func testControlProperties(t *testing.T, m *alsa.Mixer) {
	if m.NumCtls() == 0 {
		t.Skip("Skipping control properties tests: no controls found.")
		return
	}

	validTypes := map[string]bool{
		"BOOL":    true,
		"INT":     true,
		"ENUM":    true,
		"BYTE":    true,
		"IEC958":  true,
		"INT64":   true,
		"UNKNOWN": true,
	}

	for _, ctl := range m.Ctls {
		// Test Update
		err := ctl.Update()
		assert.NoError(t, err, "ctl.Update() should succeed for ctl '%s'", ctl.Name())

		// Test TypeString
		typeStr := ctl.TypeString()
		assert.True(t, validTypes[typeStr], "ctl.TypeString() returned invalid type '%s'", typeStr)

		// Test NumValues
		assert.NotPanics(t, func() { ctl.NumValues() }, "ctl.NumValues() should not panic")

		// Test enums
		if ctl.Type() == alsa.MIXER_CTL_TYPE_ENUM {
			numEnums, err := ctl.NumEnums()
			assert.NoError(t, err, "ctl.NumEnums() should succeed for enum ctl '%s'", ctl.Name())
			if numEnums > 0 {
				for j := uint(0); j < uint(numEnums); j++ {
					enumStr, err := ctl.EnumString(j)
					assert.NoError(t, err, "ctl.EnumString() should succeed for enum #%d", j)
					assert.NotEmpty(t, enumStr, "Enum string should not be empty")
				}

				allEnums, err := ctl.AllEnumStrings()
				assert.NoError(t, err)
				assert.Equal(t, int(numEnums), len(allEnums))
			}
		}
	}
}

func testControlValues(t *testing.T, m *alsa.Mixer) {
	if m.NumCtls() == 0 {
		t.Skip("Skipping control value tests: no controls found.")
		return
	}

	for _, ctl := range m.Ctls {
		if ctl.NumValues() == 0 {
			continue
		}

		// Test for out-of-bounds access
		testOutOfBoundsAccess(t, ctl)

		// Test Get/Set Percent for Integer controls
		if ctl.Type() == alsa.MIXER_CTL_TYPE_INT {
			testIntegerCtl(t, ctl)
		}

		// Test Get/Set for Enum controls
		if ctl.Type() == alsa.MIXER_CTL_TYPE_ENUM {
			testEnumCtl(t, ctl)
		}

		// Test Get/Set Array for all readable/writable types
		testArrayCtl(t, ctl)
	}
}

func testOutOfBoundsAccess(t *testing.T, ctl *alsa.MixerCtl) {
	numValues := ctl.NumValues()
	if numValues == 0 {
		return
	}
	invalidIndex := uint(numValues)

	_, err := ctl.Value(invalidIndex)
	assert.Error(t, err, "Value() with out-of-bounds index should fail for ctl '%s'", ctl.Name())

	err = ctl.SetValue(invalidIndex, 0)
	assert.Error(t, err, "SetValue() with out-of-bounds index should fail for ctl '%s'", ctl.Name())

	if ctl.Type() == alsa.MIXER_CTL_TYPE_INT {
		_, err = ctl.Percent(invalidIndex)
		assert.Error(t, err, "Percent() with out-of-bounds index should fail for ctl '%s'", ctl.Name())

		err = ctl.SetPercent(invalidIndex, 50)
		assert.Error(t, err, "SetPercent() with out-of-bounds index should fail for ctl '%s'", ctl.Name())
	}

	if ctl.Type() == alsa.MIXER_CTL_TYPE_ENUM {
		_, err := ctl.EnumValueString(invalidIndex)
		assert.Error(t, err, "EnumValueString() with out-of-bounds index should fail for ctl '%s'", ctl.Name())

		numEnums, err := ctl.NumEnums()
		if err == nil && numEnums > 0 {
			_, err := ctl.EnumString(uint(numEnums))
			assert.Error(t, err, "EnumString() with out-of-bounds index should fail for ctl '%s'", ctl.Name())
		}
	}
}

func testIntegerCtl(t *testing.T, ctl *alsa.MixerCtl) {
	minVal, errMin := ctl.RangeMin()
	maxVal, errMax := ctl.RangeMax()
	if errMin != nil || errMax != nil {
		return // Skip if range is not available
	}

	rangeVal := maxVal - minVal
	if rangeVal <= 0 {
		return // Skip if range is zero or negative
	}

	originalVal, err := ctl.Value(0)
	if err != nil {
		return // Skip if value cannot be read
	}

	isWritable := (ctl.Access() & uint32(alsa.SNDRV_CTL_ELEM_ACCESS_WRITE)) != 0
	if isWritable {
		defer func() {
			err := ctl.SetValue(0, originalVal)
			if err != nil {
				t.Logf("Warning: failed to restore original value for control '%s': %v", ctl.Name(), err)
			}
		}()
	}

	// Test Percent
	pct, err := ctl.Percent(0)
	assert.NoError(t, err, "ctl.Percent() should succeed")
	assert.GreaterOrEqual(t, pct, 0)
	assert.LessOrEqual(t, pct, 100)
	expectedPct := (originalVal - minVal) * 100 / rangeVal
	assert.Equal(t, expectedPct, pct, "Calculated percent does not match returned percent")

	// Test SetPercent (only if writable)
	if !isWritable {
		return
	}

	// Try to set to 100%
	err = ctl.SetPercent(0, 100)
	if err == nil {
		newVal, _ := ctl.Value(0)
		// NOTE: some controls can be written to, but their values might not actually change.
		assert.True(t, newVal == originalVal || newVal == maxVal, "Value after setting 100%% should be original or max")
	}

	// Try to set to 0%
	err = ctl.SetPercent(0, 0)
	if err == nil {
		newVal, _ := ctl.Value(0)
		assert.True(t, newVal == originalVal || newVal == minVal, "Value after setting 0%% should be original or min")
	}
}

func testEnumCtl(t *testing.T, ctl *alsa.MixerCtl) {
	numEnums, err := ctl.NumEnums()
	if err != nil || numEnums == 0 {
		return
	}

	// Get current value
	originalValue, err := ctl.Value(0)
	if err != nil {
		return // Cannot read initial value
	}

	isWritable := (ctl.Access() & uint32(alsa.SNDRV_CTL_ELEM_ACCESS_WRITE)) != 0
	if isWritable {
		defer func() {
			err := ctl.SetValue(0, originalValue)
			if err != nil {
				t.Logf("Warning: failed to restore original enum value for control '%s': %v", ctl.Name(), err)
			}
		}()
	}

	originalStr, err := ctl.EnumString(uint(originalValue))
	require.NoError(t, err)

	// Get current value as string
	valueStr, err := ctl.EnumValueString(0)
	assert.NoError(t, err)
	assert.Equal(t, originalStr, valueStr)

	// Test setting by string (if writable)
	if !isWritable {
		return
	}

	// Find a different enum string to set
	targetStr := ""
	for i := uint(0); i < uint(numEnums); i++ {
		str, _ := ctl.EnumString(i)
		if str != originalStr {
			targetStr = str
			break
		}
	}

	// If a different enum exists, try to set it
	if targetStr != "" {
		err = ctl.SetEnumByString(targetStr)
		if err == nil {
			newValueStr, _ := ctl.EnumValueString(0)
			// Some controls might not actually change value even if write succeeds
			assert.True(t, newValueStr == originalStr || newValueStr == targetStr)
		}
	}
}

func testArrayCtl(t *testing.T, ctl *alsa.MixerCtl) {
	isReadable := (ctl.Access() & uint32(alsa.SNDRV_CTL_ELEM_ACCESS_READ)) != 0
	isWritable := (ctl.Access() & uint32(alsa.SNDRV_CTL_ELEM_ACCESS_WRITE)) != 0

	if !isReadable {
		return
	}

	var originalData any

	switch ctl.Type() {
	case alsa.MIXER_CTL_TYPE_BOOL, alsa.MIXER_CTL_TYPE_INT, alsa.MIXER_CTL_TYPE_ENUM:
		var d []int32
		err := ctl.Array(&d)
		assert.NoError(t, err)
		originalData = d
	case alsa.MIXER_CTL_TYPE_BYTE:
		var d []byte
		err := ctl.Array(&d)
		assert.NoError(t, err)
		originalData = d
	case alsa.MIXER_CTL_TYPE_INT64:
		var d []int64
		err := ctl.Array(&d)
		assert.NoError(t, err)
		originalData = d
	default:
		return
	}

	require.NotNil(t, originalData)
	assert.Equal(t, int(ctl.NumValues()), reflect.ValueOf(originalData).Len(), "Array length should match NumValues")

	if isWritable {
		err := ctl.SetArray(originalData)
		assert.NoError(t, err, "SetArray with original data should succeed for ctl '%s'", ctl.Name())

		// Test that setting an array with the wrong length fails
		if int(ctl.NumValues()) > 0 {
			sliceVal := reflect.ValueOf(originalData)
			badSlice := reflect.MakeSlice(sliceVal.Type(), 0, 0)
			err = ctl.SetArray(badSlice.Interface())
			assert.Error(t, err, "SetArray with wrong length slice should fail")
		}
	}
}

func testMixerEvents(t *testing.T, m *alsa.Mixer) {
	if m.NumCtls() == 0 {
		t.Skip("Skipping event tests: no controls found.")
		return
	}

	err := m.SubscribeEvents(true)
	if err != nil {
		// Handle dummy devices that don't support event subscription.
		if errors.Is(err, syscall.ENOTTY) {
			t.Skipf("Skipping event test: device does not support event subscription (ENOTTY)")

			return
		}
		require.NoError(t, err, "SubscribeEvents(true) should succeed")
	}
	defer m.SubscribeEvents(false)

	// Find a writable integer control to change, preferably a "Volume" one.
	var targetCtl *alsa.MixerCtl
	for _, ctl := range m.Ctls {
		isWritable := (ctl.Access() & uint32(alsa.SNDRV_CTL_ELEM_ACCESS_WRITE)) != 0
		if isWritable && ctl.Type() == alsa.MIXER_CTL_TYPE_INT && ctl.NumValues() > 0 && strings.Contains(ctl.Name(), "Volume") {
			targetCtl = ctl
			break
		}
	}

	// Fallback to any writable integer control
	if targetCtl == nil {
		for _, ctl := range m.Ctls {
			isWritable := (ctl.Access() & uint32(alsa.SNDRV_CTL_ELEM_ACCESS_WRITE)) != 0
			if isWritable && ctl.Type() == alsa.MIXER_CTL_TYPE_INT && ctl.NumValues() > 0 {
				targetCtl = ctl
				break
			}
		}
	}

	if targetCtl == nil {
		t.Skip("Skipping event test: no suitable writable integer control found")
		return
	}

	// Read the original integer value to ensure accurate restoration.
	originalVal, err := targetCtl.Value(0)
	if err != nil {
		t.Skipf("Skipping event test: cannot get value for ctl '%s': %v", targetCtl.Name(), err)
		return
	}
	// Defer the restoration of the original value.
	defer func() {
		err := targetCtl.SetValue(0, originalVal)
		if err != nil {
			t.Logf("Warning: failed to restore original value for event test ctl '%s': %v", targetCtl.Name(), err)
		}
	}()

	var wg sync.WaitGroup
	wg.Add(1)

	// Change the control value in a separate goroutine to trigger an event.
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)

		// Try to toggle the value between its min and max to guarantee a change.
		minVal, errMin := targetCtl.RangeMin()
		maxVal, errMax := targetCtl.RangeMax()

		newValue := -1 // Sentinel for "not set"

		if errMin == nil && errMax == nil && maxVal > minVal {
			if originalVal == minVal {
				newValue = maxVal
			} else {
				newValue = minVal
			}
		}

		if newValue != -1 {
			_ = targetCtl.SetValue(0, newValue)
		} else {
			// Fallback to percent if range is not available or invalid.
			originalPct, err := targetCtl.Percent(0)
			if err == nil {
				newPct := 0
				if originalPct < 50 {
					newPct = 100
				}
				_ = targetCtl.SetPercent(0, newPct)
			}
		}
	}()

	// Wait for the event in the main test goroutine
	ready, err := m.WaitEvent(1000)
	assert.NoError(t, err, "WaitEvent should not return an error")
	assert.True(t, ready, "WaitEvent should return true, indicating a pending event")

	// If an event is ready, read it and verify its contents.
	if ready {
		event, err := m.ReadEvent()
		require.NoError(t, err, "ReadEvent should succeed after WaitEvent returns true")
		require.NotNil(t, event, "ReadEvent should have returned an event")

		assert.Equal(t, alsa.SNDRV_CTL_EVENT_MASK_VALUE, event.Type, "Event type should be a value change")
		assert.Equal(t, targetCtl.ID(), event.ControlID, "Event control ID should match the control that was changed")
	}

	wg.Wait()

	// Test ConsumeEvent: trigger another event by restoring the original value, then discard it.
	err = targetCtl.SetValue(0, originalVal)
	if err != nil {
		t.Logf("Could not set value to trigger consume event: %v", err)

		return
	}

	time.Sleep(50 * time.Millisecond) // Give time for event to propagate

	ready, _ = m.WaitEvent(100)
	if ready {
		err = m.ConsumeEvent()
		assert.NoError(t, err, "ConsumeEvent should succeed")
	}
}

func testMixerControlNotFound(t *testing.T, m *alsa.Mixer) {
	// Test with a name that is highly unlikely to exist
	nonExistentName := "This Control Really Should Not Exist 12345"
	_, err := m.CtlByName(nonExistentName)
	assert.Error(t, err, "CtlByName with a non-existent name should return an error")

	// Test with an out-of-bounds index
	outOfBoundsIndex := uint(m.NumCtls())
	_, err = m.CtlByIndex(outOfBoundsIndex)
	assert.Error(t, err, "CtlByIndex with an out-of-bounds index should return an error")

	// Test with a non-existent numeric ID.
	// Find the max ID and add 1 to it.
	maxID := uint32(0)
	for _, ctl := range m.Ctls {
		if ctl.ID() > maxID {
			maxID = ctl.ID()
		}
	}

	nonExistentID := maxID + 1
	_, err = m.Ctl(nonExistentID)
	assert.Error(t, err, "Ctl with a non-existent ID should return an error")
}

func testMixerCtlByNameAndDevice(t *testing.T, m *alsa.Mixer) {
	if m.NumCtls() == 0 {
		t.Skip("Skipping CtlByNameAndDevice test: no controls found.")
		return
	}

	for _, ctl := range m.Ctls {
		name := ctl.Name()
		device := ctl.Device()

		foundCtl, err := m.CtlByNameAndDevice(name, device)
		require.NoError(t, err, "CtlByNameAndDevice should find control '%s' on device %d", name, device)
		assert.Same(t, ctl, foundCtl, "CtlByNameAndDevice should return the correct control instance")
	}

	// Test for a control that exists but not on the specified device
	if len(m.Ctls) > 0 {
		firstCtl := m.Ctls[0]
		name := firstCtl.Name()

		// Find a device number that is NOT associated with this control name
		nonExistentDevice := uint32(9999) // Start with a high number
		isUnique := false

		for !isUnique {
			found := false
			for _, ctl := range m.Ctls {
				if ctl.Name() == name && ctl.Device() == nonExistentDevice {
					nonExistentDevice++ // Increment if we somehow guessed an existing one
					found = true

					break
				}
			}
			if !found {
				isUnique = true
			}
		}

		_, err := m.CtlByNameAndDevice(name, nonExistentDevice)
		assert.Error(t, err, "CtlByNameAndDevice should return an error for a valid name but invalid device number")
	}
}

func testMixerCtlTypeString(t *testing.T, m *alsa.Mixer) {
	if m.NumCtls() == 0 {
		t.Skip("Skipping control type string test: no controls found.")

		return
	}

	typeMap := map[alsa.MixerCtlType]string{
		alsa.MIXER_CTL_TYPE_BOOL:   "BOOL",
		alsa.MIXER_CTL_TYPE_INT:    "INT",
		alsa.MIXER_CTL_TYPE_ENUM:   "ENUM",
		alsa.MIXER_CTL_TYPE_BYTE:   "BYTE",
		alsa.MIXER_CTL_TYPE_IEC958: "IEC958",
		alsa.MIXER_CTL_TYPE_INT64:  "INT64",
	}

	for _, ctl := range m.Ctls {
		ctlType := ctl.Type()
		typeStr := ctl.TypeString()

		expectedStr, ok := typeMap[ctlType]
		if ok {
			assert.Equal(t, expectedStr, typeStr, "TypeString() for control '%s' of type %v mismatch", ctl.Name(), ctlType)
		} else {
			assert.Equal(t, "UNKNOWN", typeStr, "TypeString() for control '%s' with unknown type %v should be UNKNOWN", ctl.Name(), ctlType)
		}
	}
}
