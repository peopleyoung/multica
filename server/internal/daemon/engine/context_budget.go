package engine

import (
	"bytes"
	"encoding/json"
	"github.com/multica-ai/multica/server/pkg/llm"
)

// Provider measurement replaces the byte upper estimate only for an unchanged
// request prefix. Every appended byte is still reserved as one possible token;
// model switches, tool changes and compaction invalidate the measurement.
type contextBudget struct {
	messages []llm.ModelMessage
	tools    []byte
	input    int64
	bytes    int
}

func (b *contextBudget) estimate(req llm.Request) int64 {
	raw, _ := json.Marshal(req)
	estimate := int64(len(raw))
	defs, _ := json.Marshal(req.Tools)
	if b.input <= 0 || len(req.Messages) < len(b.messages) || !bytes.Equal(defs, b.tools) {
		return estimate
	}
	prefix, _ := json.Marshal(req.Messages[:len(b.messages)])
	old, _ := json.Marshal(b.messages)
	if !bytes.Equal(prefix, old) {
		return estimate
	}
	return b.input + int64(max(0, len(raw)-b.bytes)) + 256
}
func (b *contextBudget) observe(req llm.Request, usage *llm.Usage) {
	*b = contextBudget{}
	if usage == nil || usage.InputTokens <= 0 {
		return
	}
	raw, _ := json.Marshal(req)
	b.messages = append([]llm.ModelMessage(nil), req.Messages...)
	b.tools, _ = json.Marshal(req.Tools)
	b.bytes = len(raw)
	b.input = usage.InputTokens
}
