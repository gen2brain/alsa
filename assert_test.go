package alsa_test

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// asserter is a minimal local replacement for the testify assert/require API.
// The package-level values assert and require share this type; require fails the
// test immediately (t.FailNow), while assert records the failure and continues.
type asserter struct {
	fatal bool
}

var (
	assert  = asserter{fatal: false}
	require = asserter{fatal: true}
)

// fail reports a failure with an optional caller-supplied message and, for the
// require variant, aborts the test. It always returns false so callers may
// return its result directly.
func (a asserter) fail(t *testing.T, defaultMsg string, msgAndArgs ...any) bool {
	t.Helper()

	if msg := formatMsg(msgAndArgs); msg != "" {
		t.Errorf("%s: %s", defaultMsg, msg)
	} else {
		t.Errorf("%s", defaultMsg)
	}

	if a.fatal {
		t.FailNow()
	}

	return false
}

// formatMsg renders the trailing msgAndArgs the same way testify does: a single
// argument is used verbatim, while a leading format string is applied to the
// rest.
func formatMsg(msgAndArgs []any) string {
	switch len(msgAndArgs) {
	case 0:
		return ""
	case 1:
		if s, ok := msgAndArgs[0].(string); ok {
			return s
		}
		return fmt.Sprint(msgAndArgs[0])
	default:
		if format, ok := msgAndArgs[0].(string); ok {
			return fmt.Sprintf(format, msgAndArgs[1:]...)
		}
		return fmt.Sprint(msgAndArgs...)
	}
}

func (a asserter) NoError(t *testing.T, err error, msgAndArgs ...any) bool {
	t.Helper()
	if err != nil {
		return a.fail(t, fmt.Sprintf("expected no error, got: %v", err), msgAndArgs...)
	}
	return true
}

func (a asserter) Error(t *testing.T, err error, msgAndArgs ...any) bool {
	t.Helper()
	if err == nil {
		return a.fail(t, "expected an error, got nil", msgAndArgs...)
	}
	return true
}

func (a asserter) ErrorIs(t *testing.T, err, target error, msgAndArgs ...any) bool {
	t.Helper()
	if !errors.Is(err, target) {
		return a.fail(t, fmt.Sprintf("expected error to be %v, got: %v", target, err), msgAndArgs...)
	}
	return true
}

func (a asserter) Equal(t *testing.T, expected, actual any, msgAndArgs ...any) bool {
	t.Helper()
	if !objectsAreEqual(expected, actual) {
		return a.fail(t, fmt.Sprintf("not equal:\n\texpected: %#v\n\tactual:   %#v", expected, actual), msgAndArgs...)
	}
	return true
}

func (a asserter) NotEqual(t *testing.T, expected, actual any, msgAndArgs ...any) bool {
	t.Helper()
	if objectsAreEqual(expected, actual) {
		return a.fail(t, fmt.Sprintf("expected values to differ, both are: %#v", actual), msgAndArgs...)
	}
	return true
}

func (a asserter) True(t *testing.T, value bool, msgAndArgs ...any) bool {
	t.Helper()
	if !value {
		return a.fail(t, "expected true, got false", msgAndArgs...)
	}
	return true
}

func (a asserter) False(t *testing.T, value bool, msgAndArgs ...any) bool {
	t.Helper()
	if value {
		return a.fail(t, "expected false, got true", msgAndArgs...)
	}
	return true
}

func (a asserter) Nil(t *testing.T, object any, msgAndArgs ...any) bool {
	t.Helper()
	if !isNil(object) {
		return a.fail(t, fmt.Sprintf("expected nil, got: %#v", object), msgAndArgs...)
	}
	return true
}

func (a asserter) NotNil(t *testing.T, object any, msgAndArgs ...any) bool {
	t.Helper()
	if isNil(object) {
		return a.fail(t, "expected non-nil value, got nil", msgAndArgs...)
	}
	return true
}

// Same asserts that two pointers reference the same object.
func (a asserter) Same(t *testing.T, expected, actual any, msgAndArgs ...any) bool {
	t.Helper()
	if !samePointer(expected, actual) {
		return a.fail(t, fmt.Sprintf("expected pointers to reference the same object:\n\texpected: %p\n\tactual:   %p", expected, actual), msgAndArgs...)
	}
	return true
}

// Contains asserts that a string contains a substring, or that a slice/array/map
// contains an element/key.
func (a asserter) Contains(t *testing.T, container, element any, msgAndArgs ...any) bool {
	t.Helper()
	if !containsElement(container, element) {
		return a.fail(t, fmt.Sprintf("%#v does not contain %#v", container, element), msgAndArgs...)
	}
	return true
}

func (a asserter) NotEmpty(t *testing.T, object any, msgAndArgs ...any) bool {
	t.Helper()
	if isEmpty(object) {
		return a.fail(t, "expected non-empty value", msgAndArgs...)
	}
	return true
}

func (a asserter) NotZero(t *testing.T, value any, msgAndArgs ...any) bool {
	t.Helper()
	if isZero(value) {
		return a.fail(t, fmt.Sprintf("expected non-zero value, got: %#v", value), msgAndArgs...)
	}
	return true
}

func (a asserter) Greater(t *testing.T, e1, e2 any, msgAndArgs ...any) bool {
	t.Helper()
	if compare(t, e1, e2) <= 0 {
		return a.fail(t, fmt.Sprintf("expected %#v > %#v", e1, e2), msgAndArgs...)
	}
	return true
}

func (a asserter) GreaterOrEqual(t *testing.T, e1, e2 any, msgAndArgs ...any) bool {
	t.Helper()
	if compare(t, e1, e2) < 0 {
		return a.fail(t, fmt.Sprintf("expected %#v >= %#v", e1, e2), msgAndArgs...)
	}
	return true
}

func (a asserter) LessOrEqual(t *testing.T, e1, e2 any, msgAndArgs ...any) bool {
	t.Helper()
	if compare(t, e1, e2) > 0 {
		return a.fail(t, fmt.Sprintf("expected %#v <= %#v", e1, e2), msgAndArgs...)
	}
	return true
}

func (a asserter) NotPanics(t *testing.T, f func(), msgAndArgs ...any) (ok bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			ok = a.fail(t, fmt.Sprintf("expected no panic, but panicked with: %v", r), msgAndArgs...)
		}
	}()
	f()
	return true
}

func (a asserter) WithinDuration(t *testing.T, expected, actual time.Time, delta time.Duration, msgAndArgs ...any) bool {
	t.Helper()
	diff := expected.Sub(actual)
	if diff < 0 {
		diff = -diff
	}
	if diff > delta {
		return a.fail(t, fmt.Sprintf("max difference between %v and %v allowed is %v, but difference was %v", expected, actual, delta, diff), msgAndArgs...)
	}
	return true
}

func objectsAreEqual(expected, actual any) bool {
	if expected == nil || actual == nil {
		return expected == actual
	}
	if exp, ok := expected.([]byte); ok {
		act, ok := actual.([]byte)
		if !ok {
			return false
		}
		return bytes.Equal(exp, act)
	}
	return reflect.DeepEqual(expected, actual)
}

func isNil(object any) bool {
	if object == nil {
		return true
	}
	v := reflect.ValueOf(object)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func isEmpty(object any) bool {
	if object == nil {
		return true
	}
	v := reflect.ValueOf(object)
	switch v.Kind() {
	case reflect.Array, reflect.Chan, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Ptr:
		if v.IsNil() {
			return true
		}
		return isEmpty(v.Elem().Interface())
	default:
		return v.IsZero()
	}
}

func isZero(value any) bool {
	if value == nil {
		return true
	}
	return reflect.ValueOf(value).IsZero()
}

func samePointer(expected, actual any) bool {
	expVal := reflect.ValueOf(expected)
	actVal := reflect.ValueOf(actual)
	if expVal.Kind() != reflect.Ptr || actVal.Kind() != reflect.Ptr {
		return false
	}
	if expVal.Type() != actVal.Type() {
		return false
	}
	return expVal.Pointer() == actVal.Pointer()
}

func containsElement(container, element any) bool {
	cv := reflect.ValueOf(container)
	switch cv.Kind() {
	case reflect.String:
		return strings.Contains(cv.String(), fmt.Sprint(element))
	case reflect.Map:
		for _, k := range cv.MapKeys() {
			if objectsAreEqual(k.Interface(), element) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < cv.Len(); i++ {
			if objectsAreEqual(cv.Index(i).Interface(), element) {
				return true
			}
		}
	}
	return false
}

// compare orders two numeric values, returning -1, 0 or 1. It fails the test if
// either value is not a number, mirroring the inputs the suite actually uses.
func compare(t *testing.T, e1, e2 any) int {
	t.Helper()
	f1, ok1 := toFloat(e1)
	f2, ok2 := toFloat(e2)
	if !ok1 || !ok2 {
		t.Fatalf("cannot compare non-numeric values %#v and %#v", e1, e2)
	}
	switch {
	case f1 < f2:
		return -1
	case f1 > f2:
		return 1
	default:
		return 0
	}
}

func toFloat(v any) (float64, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	default:
		return 0, false
	}
}
