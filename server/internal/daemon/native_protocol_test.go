package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestNativeRegistrationNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name, offer, provider string
		protocol              protocol.NativeRuntimeProtocol
		capability            bool
		wantError             bool
	}{
		{name: "current", offer: "native", provider: "native", protocol: protocol.NativeRuntimeSupport(), capability: true},
		{name: "old API", offer: "native", provider: "native", wantError: true},
		{name: "old API unknown provider", offer: "native", provider: "unknown", wantError: true},
		{name: "old API omits runtime", offer: "native", wantError: true},
		{name: "future minimum", offer: "native", provider: "native", protocol: protocol.NativeRuntimeProtocol{Version: 2, MinimumVersion: 2}, capability: true, wantError: true},
		{name: "missing capability", offer: "native", provider: "native", protocol: protocol.NativeRuntimeSupport(), wantError: true},
		{name: "missing range", offer: "native", provider: "native", capability: true, wantError: true},
		{name: "negotiated but missing runtime", offer: "native", protocol: protocol.NativeRuntimeSupport(), capability: true, wantError: true},
		{name: "newer compatible API", offer: "native", provider: "native", protocol: protocol.NativeRuntimeProtocol{Version: 2, MinimumVersion: 1}, capability: true},
		{name: "old API legacy daemon", offer: "claude", provider: "claude"},
		{name: "native response cannot bypass negotiation", offer: "claude", provider: "native", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				resp := RegisterResponse{NativeRuntimeProtocol: tc.protocol}
				if tc.provider != "" {
					resp.Runtimes = []Runtime{{Provider: tc.provider}}
				}
				if tc.capability {
					resp.ServerCapabilities = []string{protocol.DaemonCapabilityNativeRuntimeV1}
				}
				json.NewEncoder(w).Encode(resp)
			}))
			defer srv.Close()
			_, err := NewClient(srv.URL).Register(context.Background(), map[string]any{"runtimes": []map[string]string{{"type": tc.offer}}})
			if (err != nil) != tc.wantError {
				t.Fatalf("Register error = %v, wantError = %v", err, tc.wantError)
			}
		})
	}
}
