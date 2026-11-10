package agent

// ThinkingLevelRejectedWithoutModel reports whether the API should refuse to
// STORE an effort that has no pinned model, instead of storing it and leaving
// the daemon to drop it at launch.
//
// Deliberately narrower than ThinkingRequiresExplicitModel. codex shares the
// execution constraint but predates this check: agents out there already hold an
// effort alongside an empty model, and 400ing that combination would block
// unrelated edits to them, so codex stays grandfathered and the daemon keeps
// dropping the level. omp has no such history — persisting an effort at all was
// impossible before MUL-7412 — so it is strict from the start and the invalid
// combination never reaches storage.
func ThinkingLevelRejectedWithoutModel(providerType string) bool {
	return providerType == "omp"
}

// providerThinkingEnums is the server-side accept-list for runtimes with a
// fixed reasoning-effort vocabulary. Codex and OpenCode are deliberately
// absent because their values come from daemon-local model catalogs, which can
// gain new tokens without a Multica release.
//
// The server doesn't have local CLI binaries, so it cannot do per-model
// discovery the way the daemon can. Fixed-catalog providers use this enum;
// dynamic providers take the safe-token path in IsKnownThinkingValue below.
// Per-model gaps are handled by the daemon's pre-execution guard, which logs
// and skips injection rather than mutating persisted agent state.
//
// Keep fixed-provider lists permissive: this is a provider-universe check,
// not an "is this right for this model" check.
var providerThinkingEnums = map[string]map[string]bool{
	"claude": {
		"low":    true,
		"medium": true,
		"high":   true,
		"xhigh":  true,
		"max":    true,
	},
	// Confirmed against CodeBuddy 2.130.0's advertised thought_level catalog.
	// `minimal` and `max` were missing here, so the server rejected two levels
	// the CLI genuinely accepts.
	"codebuddy": {
		"minimal": true,
		"low":     true,
		"medium":  true,
		"high":    true,
		"xhigh":   true,
		"max":     true,
	},
	// Pi owns a fixed CLI vocabulary; RPC discovery narrows this universe to
	// the exact subset supported by each model before execution.
	"pi": {
		"off":     true,
		"minimal": true,
		"low":     true,
		"medium":  true,
		"high":    true,
		"xhigh":   true,
		"max":     true,
	},
	// omp (Oh-My-Pi) dispatches to the pi backend (see BuiltinRuntimes), so it
	// inherits pi's fixed CLI vocabulary; discoverOmpModels narrows it to each
	// model's advertised efforts before execution. `auto` is deliberately absent
	// even though omp's --thinking accepts it — see ompThinkingFromCatalogEntry
	// (MUL-7412).
	"omp": {
		"off":     true,
		"minimal": true,
		"low":     true,
		"medium":  true,
		"high":    true,
		"xhigh":   true,
		"max":     true,
	},
}

// thinkingDynamicCatalogProviders are the runtimes whose effort vocabulary is
// owned by a daemon-local model catalog instead of a fixed enum above. The
// server accepts any well-formed token for them and lets the daemon's
// per-model check decide before execution.
var thinkingDynamicCatalogProviders = map[string]bool{
	"codex": true,
	"dsh":   true,
	// Grok advertises each model's effort catalog through session/new, so the
	// server does not maintain a provider-wide fixed enum. The daemon applies
	// the selected effort with `--effort`, not session/set_config_option.
	"grok":     true,
	"opencode": true,
	"kimi":     true,
}

// acpCatalogThinkingProviders are the ACP runtimes that discover their effort
// catalog from `session/new` and apply it with `session/set_config_option`.
// They behave like the dynamic-catalog providers above — the server accepts a
// well-formed token and the daemon checks it against the discovered catalog —
// but they are listed separately because membership means something stricter:
// the runtime's Execute must actually call applyACPEffortOption.
//
// Do NOT add a runtime here just because it speaks ACP. Two things have to be
// true, and neither is implied by the protocol:
//
//   - Its Execute wires up applyACPEffortOption. Copilot is the counterexample
//     — its discovery runs over ACP but it executes through its own CLI
//     surface (`--acp` is blocked in copilot.go), so a catalog here would
//     render a picker with nothing behind it.
//   - Someone has confirmed the runtime actually threads the setting into its
//     provider request, from its source or a real run. Advertising is not
//     evidence — Hermes accepts set_config_option and ignores it, Kimi ≤0.28.1
//     confirms "on" after being set to "max" — and neither is the read-back in
//     applyACPEffortOption, which only proves the session reports the new
//     value. This list is where that offline verification is recorded; the
//     read-back is runtime diagnostics on top of it.
var acpCatalogThinkingProviders = map[string]bool{
	// reasonix v1.21.5: session/new advertises option id `effort` (category
	// `thought_level`), set_config_option returns the refreshed options, and
	// the effort reaches the session controller rather than stopping at the
	// config surface. Its catalog is per model — see
	// annotateACPThinkingForSessionModel.
	"reasonix": true,
	// hermes covers two unrelated binaries, and membership here is safe only
	// because the catalog decides per session which one answered:
	//
	//   - jcode advertises option id `reasoning_effort` (category
	//     `thought_level`) and genuinely applies it — set_config_option waits
	//     for an `effort_changed` ack, and the provider request carries
	//     `reasoning.effort` upstream. Confirmed against jcode v0.71.1 and
	//     v0.73.0 (GitHub #6720). Its catalog is per model too: jcode
	//     revalidates the effort against the new model's advertised list on a
	//     model switch.
	//   - Hermes Agent advertises no configOptions at all, so it gets an empty
	//     catalog, no picker, and no set_config_option call. Re-verified
	//     against v0.20.0 on 2026-08-11: session/new still returns only
	//     `_meta`, `models`, `modes`, `sessionId` — unchanged from the v0.18.2
	//     finding in MUL-5770.
	//
	// That split is why this feature is catalog-driven rather than gated on a
	// version string: one provider, two binaries, and the session answers the
	// capability question directly.
	"hermes": true,
	// dim (dimcode 0.3.10+): session/new advertises thought_level.
	"dim": true,
}

// usesDynamicThinkingCatalog reports whether a provider's effort vocabulary is
// owned by a daemon-local catalog rather than a fixed server-side enum.
func usesDynamicThinkingCatalog(providerType string) bool {
	return thinkingDynamicCatalogProviders[providerType] || acpCatalogThinkingProviders[providerType]
}

// UsesACPCatalogThinking reports whether a provider's effort support is decided
// per session by what its ACP handshake advertises, rather than by the provider
// name alone.
//
// Callers that can reach a discovered catalog should use it to answer the
// capability question for a specific runtime: `hermes` covers both jcode (which
// advertises and applies an effort) and Hermes Agent (which advertises none), so
// the provider name is not a sufficient answer for either. See
// acpCatalogThinkingProviders.
func UsesACPCatalogThinking(providerType string) bool {
	return acpCatalogThinkingProviders[providerType]
}

// ThinkingControlSupported reports whether Multica can deliver a per-agent
// reasoning effort to this runtime at all. False means the answer to any
// thinking_level is "no", regardless of the token: the runtime exposes no
// effort dial on the surface the daemon speaks to it over, so there is nothing
// to inject and nothing a different spelling would fix.
//
// Copilot is the instructive case. It speaks ACP for model discovery but
// executes through its own CLI surface (`--acp` is blocked in copilot.go), so
// there is no live ACP session to carry an effort onto — a picker there would
// be inert no matter what discovery advertised.
//
// True at provider granularity only. The `hermes` provider covers two
// unrelated binaries whose answers differ — jcode applies an advertised
// effort, Hermes Agent has no effort surface on ACP at all — so it reports
// true here and the per-session catalog decides whether a picker actually
// appears. See acpCatalogThinkingProviders for the evidence on each.
func ThinkingControlSupported(providerType string) bool {
	if usesDynamicThinkingCatalog(providerType) {
		return true
	}
	_, ok := providerThinkingEnums[providerType]
	return ok
}

// IsKnownThinkingValue reports whether `value` is a recognised effort
// token for the given provider. Empty string is always accepted (means
// "use runtime default"). Providers with no reasoning control accept
// only empty; Codex, OpenCode, Kimi, and the ACP catalog runtimes accept
// well-formed tokens here because their daemon-local catalogs perform the
// exact per-model check before execution.
//
// This is the cheap synchronous gate the server uses on CreateAgent /
// UpdateAgent. Unlike ValidateThinkingLevel it does NOT consult the live
// catalog or per-model subset. Callers that surface a rejection to a user
// should ask ThinkingControlSupported first so the message says "this runtime
// has no reasoning control" instead of implying a bad token.
func IsKnownThinkingValue(providerType, value string) bool {
	if value == "" {
		return true
	}
	if usesDynamicThinkingCatalog(providerType) {
		return isValidDynamicThinkingValue(value)
	}
	enum, ok := providerThinkingEnums[providerType]
	if !ok {
		return false
	}
	return enum[value]
}

// IsKnownServiceTier is the server-side literal gate. The exact per-model
// catalog lives on the daemon host, so Codex accepts safe future catalog IDs
// here and ValidateServiceTier performs the execution-time compatibility
// check. Other providers do not currently expose service tiers.
func IsKnownServiceTier(providerType, value string) bool {
	if value == "" {
		return true
	}
	return providerType == "codex" && isValidDynamicThinkingValue(value)
}

func isValidDynamicThinkingValue(value string) bool {
	if len(value) > 64 {
		return false
	}
	for i, r := range value {
		valid := r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.'
		if !valid {
			return false
		}
		if i == 0 && (r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}
