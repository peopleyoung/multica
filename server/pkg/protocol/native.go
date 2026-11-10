package protocol

const (
	// NativeRuntimeV1 covers claim generation, durable sessions and the
	// authorized model/tool loop. Build labels cannot substitute for it.
	DaemonCapabilityNativeRuntimeV1 = "native-runtime-v1"
	NativeRuntimeVersion            = 1
	MinimumNativeRuntimeVersion     = 1
)

// NativeRuntimeProtocol is additive registration metadata. Version is the
// newest supported protocol; MinimumVersion is the oldest accepted protocol.
type NativeRuntimeProtocol struct {
	Version        int `json:"version"`
	MinimumVersion int `json:"minimum_version"`
}

func NativeRuntimeSupport() NativeRuntimeProtocol {
	return NativeRuntimeProtocol{Version: NativeRuntimeVersion, MinimumVersion: MinimumNativeRuntimeVersion}
}

func (p NativeRuntimeProtocol) Supports(version int) bool {
	return p.MinimumVersion > 0 && p.MinimumVersion <= version && p.Version >= version
}
