package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"strings"

	"github.com/multica-ai/multica/server/pkg/llm"
)

const noProgressNotice = "Runtime progress check: repeated read-only tool calls have returned identical results without new evidence. Reuse the facts already in history; do not restart the same investigation after a summary. Change approach only if an available tool can obtain new evidence. Otherwise report the concrete missing data, access or capability through the requested reply thread and finish with the blocker. Do not claim inaccessible data does not exist, fabricate results, mark an unfulfilled objective done, or bypass permissions. If unchanged reads continue, this run will stop. Existing permissions and user constraints still apply."

type progressAction int

const (
	progressContinue progressAction = iota
	progressWarn
	progressStop
)

// Full durable history, rather than the compacted projection, owns progress.
// No call IDs, argument values or tool contents are included in diagnostics.
// Writes/unknown effects and new human input reset the comparison epoch.
func repeatedReadAction(messages []llm.ModelMessage, host ToolHost) progressAction {
	type observation struct {
		output [32]byte
		count  int
	}
	seen := map[[32]byte]observation{}
	var calls []llm.ToolCall
	results := map[string]string{}
	warned := false
	action := progressContinue
	for _, message := range messages {
		switch message.Role {
		case "user":
			clear(seen)
			warned, action = false, progressContinue
		case "system":
			if strings.HasSuffix(message.Content, noProgressNotice) {
				warned = true
			}
			action = progressContinue
		case "assistant":
			calls = message.ToolCalls
			clear(results)
			action = progressContinue
		case "tool":
			results[message.ToolCallID] = message.Content
			if len(calls) == 0 || len(results) != len(calls) {
				continue
			}
			allRepeated, maxCount := true, 0
			for _, call := range calls {
				polling := false
				if call.Name == "persistent_job" {
					var args struct {
						Action string `json:"action"`
					}
					_ = json.Unmarshal(call.Arguments, &args)
					polling = args.Action == "output" || args.Action == "list"
				}
				if polling || !host.IsReadOnly(call) {
					clear(seen)
					warned, allRepeated = false, false
					break
				}
			}
			if !allRepeated {
				action = progressContinue
				continue
			}
			for _, call := range calls {
				key := sha256.Sum256(append([]byte(call.Name+"\x00"), canonicalJSON(call.Arguments)...))
				output := sha256.Sum256(canonicalJSON([]byte(results[call.ID])))
				previous, exists := seen[key]
				if exists && previous.output == output {
					previous.count++
				} else {
					previous = observation{output: output, count: 1}
					allRepeated = false
				}
				seen[key] = previous
				maxCount = max(maxCount, previous.count)
			}
			action = progressContinue
			if !allRepeated {
				warned = false
			} else if maxCount >= 3 {
				action = progressWarn
				if warned {
					action = progressStop
				}
			}
		}
	}
	return action
}

func canonicalJSON(raw []byte) []byte {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return raw
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return encoded
}
