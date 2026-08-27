package manager

import "reflect"

// nilPort rejects both nil interfaces and interfaces containing nil pointer,
// function, map, slice, channel, or interface values before invoking an
// externally supplied runtime/connector port.
func nilPort(port any) bool {
	if port == nil {
		return true
	}
	value := reflect.ValueOf(port)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
