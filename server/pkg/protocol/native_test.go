package protocol

import "testing"

func TestNativeRuntimeProtocolRange(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rangeValue NativeRuntimeProtocol
		want       bool
	}{
		{"current", NativeRuntimeSupport(), true},
		{"newer compatible server", NativeRuntimeProtocol{Version: 2, MinimumVersion: 1}, true},
		{"omitted", NativeRuntimeProtocol{}, false},
		{"missing minimum", NativeRuntimeProtocol{Version: 1}, false},
		{"future minimum", NativeRuntimeProtocol{Version: 2, MinimumVersion: 2}, false},
		{"inverted range", NativeRuntimeProtocol{Version: 1, MinimumVersion: 2}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rangeValue.Supports(NativeRuntimeVersion); got != tc.want {
				t.Fatalf("Supports = %v, want %v", got, tc.want)
			}
		})
	}
}
