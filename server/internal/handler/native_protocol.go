package handler

import (
	"encoding/json"
	"net/http"

	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/nativeruntime"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func writeNativeProtocolRequired(w http.ResponseWriter) {
	writeJSON(w, http.StatusUpgradeRequired, map[string]any{
		"error":                    "update and re-register the Multica daemon; native execution requires native-runtime-v1",
		"code":                     "native_runtime_protocol_required",
		"required_capability":      protocol.DaemonCapabilityNativeRuntimeV1,
		"minimum_protocol_version": protocol.MinimumNativeRuntimeVersion,
	})
}

// Check both the current request and registration: a downgraded daemon must
// not inherit a previous binary's advertised capabilities.
func nativeRuntimeCompatible(r *http.Request, runtime db.AgentRuntime) bool {
	return runtime.Provider == "native" && !runtime.ProfileID.Valid &&
		requestHasClientCapability(r, protocol.DaemonCapabilityNativeRuntimeV1) &&
		runtimeHasCapability(runtime.Metadata, protocol.DaemonCapabilityNativeRuntimeV1)
}

func validateNativeAgentOptions(w http.ResponseWriter, provider string, config, args any, thinking, tier string) bool {
	if provider != "native" {
		return true
	}
	raw, err := json.Marshal(config)
	_, parseErr := nativeruntime.Parse(raw)
	if err != nil || parseErr != nil || !emptyNativeOption(args, false) || thinking != "" || tier != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "native agents do not accept CLI configuration; use native runtime settings and clear custom_args, thinking_level and service_tier",
			"code":  "native_cli_config_not_supported",
		})
		return false
	}
	return true
}

func emptyNativeOption(value any, object bool) bool {
	data, err := json.Marshal(value)
	if err != nil {
		return false
	}
	var decoded any
	if json.Unmarshal(data, &decoded) != nil {
		return false
	}
	if decoded == nil {
		return true
	}
	if object {
		values, ok := decoded.(map[string]any)
		return ok && len(values) == 0
	}
	values, ok := decoded.([]any)
	return ok && len(values) == 0
}

func requireNativeDaemonIdentity(w http.ResponseWriter, r *http.Request, runtime db.AgentRuntime) bool {
	if daemonID := middleware.DaemonIDFromContext(r.Context()); daemonID != "" && (!runtime.DaemonID.Valid || runtime.DaemonID.String != daemonID) {
		writeError(w, http.StatusForbidden, "runtime does not belong to this daemon")
		return false
	}
	return true
}

func requireExecutableRuntime(w http.ResponseWriter, runtime db.AgentRuntime) bool {
	if runtime.Provider != "native" || runtime.ProfileID.Valid {
		writeJSON(w, http.StatusGone, map[string]string{
			"code":  "external_runtime_retired",
			"error": "External AI runtimes have been retired; migrate this binding to the native Multica runtime",
		})
		return false
	}
	return true
}

// Host powers may be selected only by the runtime's human owner. Public runtime
// binding and workspace admin status do not grant authority over the host.
func (h *Handler) authorizeNativeRuntimeConfig(w http.ResponseWriter, r *http.Request, runtime db.AgentRuntime, raw any) bool {
	if runtime.Provider != "native" {
		return true
	}
	data, _ := json.Marshal(raw)
	cfg, err := nativeruntime.Parse(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid native runtime configuration")
		return false
	}
	if !cfg.WriteFiles && !cfg.Shell && len(cfg.Services) == 0 {
		return true
	}
	actorType, _ := h.resolveActor(r, requestUserID(r), uuidToString(runtime.WorkspaceID))
	if actorType == "agent" || !runtime.OwnerID.Valid || uuidToString(runtime.OwnerID) != requestUserID(r) {
		writeError(w, http.StatusForbidden, "only the runtime owner can grant native execution capabilities")
		return false
	}
	return true
}
