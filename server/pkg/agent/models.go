package agent

import (
	"regexp"

	"strings"
)

// Model describes a single LLM model exposed by an agent provider.
// The dropdown groups by Provider when the ID uses the
// `provider/model` form (e.g. "openai/gpt-4o" from opencode).
// Default is a *display* hint: the UI badges the entry the
// runtime advertises as its preferred pick (e.g. Claude Code's
// shipped default, or hermes' currentModelId). It has no effect
// at execution time — when agent.model is empty the daemon passes
// "" to the backend so each provider's own CLI resolves its own
// default, which is always closer to what the user's account /
// environment actually supports than a static guess here.
type Model struct {
	ID                                  string             `json:"id"`
	Label                               string             `json:"label"`
	Provider                            string             `json:"provider,omitempty"`
	Default                             bool               `json:"default,omitempty"`
	ServiceTiers                        []ModelServiceTier `json:"service_tiers,omitempty"`
	SupportsExplicitStandardServiceTier bool               `json:"supports_explicit_standard_service_tier,omitempty"`
	// Thinking advertises the runtime's reasoning/effort catalog for this
	// model. nil means the runtime/model has no thinking-level control
	// (or the daemon couldn't discover one); the UI hides its picker. The
	// catalog is per-model because Codex's `codex debug models` is itself
	// per-model and Claude's `--effort` superset has known per-model gaps
	// (`xhigh` is Opus-only, `max` is session-only). See MUL-2339.
	Thinking *ModelThinking `json:"thinking,omitempty"`
}

// ModelServiceTier is one runtime-native execution tier advertised for a
// model. ID is sent back to the provider protocol unchanged; Name and
// Description are display copy owned by the runtime catalog.
type ModelServiceTier struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ModelThinking carries the per-model reasoning/effort catalog
// surfaced by an agent runtime. Values are runtime-native — Codex can emit
// "none|minimal|low|medium|high|xhigh|max|ultra"; Claude emits
// "low|medium|high|xhigh|max"; Pi emits
// "off|minimal|low|medium|high|xhigh|max". The frontend renders
// SupportedLevels as-is so what users see matches each CLI's own UI.
type ModelThinking struct {
	SupportedLevels []ThinkingLevel `json:"supported_levels"`
	// DefaultLevel is the value the runtime picks when no override is
	// provided. Empty means "the runtime picks, we don't know" — the
	// UI shows "Default" as a generic option.
	DefaultLevel string `json:"default_level,omitempty"`
}

// ThinkingLevel is one entry in a ModelThinking.SupportedLevels list.
// Value is the literal token passed to the CLI (Claude `--effort <value>`
// or Codex `model_reasoning_effort=<value>`); Label is a display string;
// Description is optional helper copy lifted from the upstream catalog
// when available (Codex's `description` field).
type ThinkingLevel struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// ModelKnownIncompatibleWithProvider reports whether a saved model is a known
// mismatch for a target runtime provider. Only Claude and Codex are judged, and
// only by model-family prefix: they gain same-family model IDs through live
// discovery without a Multica release, so no static list could be an
// allow-list here. Unknown/custom model strings still return false because the
// UI and CLI allow manual entries and the server should not erase values it
// cannot confidently classify.
func ModelKnownIncompatibleWithProvider(providerType, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}

	lookupID := modelIDForCapabilityLookup(providerType, model)
	switch providerType {
	case "claude":
		if strings.HasPrefix(lookupID, "claude-") && !strings.ContainsAny(lookupID, "[]") {
			return false
		}
	case "codex":
		if strings.HasPrefix(lookupID, "gpt-") || isOpenAIReasoningSeriesID(lookupID) {
			return false
		}
	default:
		return false
	}
	return isRuntimeSpecificModelID(model)
}

// claudeContextWindowTagRe recognises Claude Code's trailing context-window
// model modifier (for example, claude-opus-5[1m]). Keep this narrower than a
// generic bracket suffix: capability lookup may inherit the base model's
// effort catalog only when the modifier is syntactically a context size.
var claudeContextWindowTagRe = regexp.MustCompile(`\[[1-9][0-9]*[km]\]$`)

// modelIDForCapabilityLookup returns the catalog identity for a runtime-native
// model string. It never changes the value persisted on the agent or passed to
// the provider CLI. Claude context-window variants share their base model's
// capabilities; every other provider and malformed/unknown modifier retains
// exact-match behavior.
func modelIDForCapabilityLookup(providerType, model string) string {
	if providerType != "claude" {
		return model
	}
	return claudeContextWindowTagRe.ReplaceAllString(model, "")
}

func isRuntimeSpecificModelID(model string) bool {
	if strings.Contains(model, "/") {
		return true
	}
	return modelHasKnownPrefix(model)
}

func modelHasKnownPrefix(model string) bool {
	return strings.HasPrefix(model, "claude-") ||
		strings.HasPrefix(model, "gpt-") ||
		strings.HasPrefix(model, "gemini-") ||
		strings.HasPrefix(model, "auto-gemini-") ||
		isOpenAIReasoningSeriesID(model)
}

// isOpenAIReasoningSeriesID matches IDs in OpenAI's `o`-prefixed
// reasoning family: lowercase `o` followed by at least one digit
// and then either end-of-string or a `-` separator (e.g. `o3`,
// `o3-mini`, `o4-mini-high`). Avoids false positives like
// `opus-…` or random IDs that happen to start with `o`.
func isOpenAIReasoningSeriesID(id string) bool {
	if len(id) < 2 || id[0] != 'o' {
		return false
	}
	i := 1
	for i < len(id) && id[i] >= '0' && id[i] <= '9' {
		i++
	}
	if i == 1 {
		return false
	}
	return i == len(id) || id[i] == '-'
}
