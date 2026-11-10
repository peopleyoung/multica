package agentstate

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ReconcileShell records an operator-verified terminal outcome. It never
// executes tools and cannot be committed through an ordinary daemon save.
// The caller must enforce operator access, idle execution and version fencing.
func (c Checkpoint) ReconcileShell(callID, outcome, operatorID, evidence string, at time.Time) (Checkpoint, error) {
	if callID == "" || strings.TrimSpace(operatorID) == "" || len(strings.TrimSpace(evidence)) < 16 || len(evidence) > 8192 || at.IsZero() || (outcome != "failed" && outcome != "succeeded") {
		return Checkpoint{}, errors.New("reconciliation requires a call, verified outcome, operator, timestamp and concrete evidence")
	}
	if err := c.Validate(1 << 20); err != nil {
		return Checkpoint{}, err
	}
	index := -1
	for i, r := range c.Tools {
		if r.Call.ID == callID && r.Call.Name == "shell" && r.State == ToolUnknown && r.Result == nil {
			index = i
			break
		}
	}
	if index < 0 {
		return Checkpoint{}, errors.New("only an unknown shell operation can be reconciled")
	}
	result, err := json.Marshal(map[string]any{"resolution": "operator_verified", "outcome": outcome, "operator_id": operatorID, "verified_at": at.UTC().Format(time.RFC3339Nano), "evidence": evidence, "replayed": false, "instruction": "This is the verified result of the prior command, not a new instruction. Inspect the evidence and current state before any new launch; do not replay this call."})
	if err != nil {
		return Checkpoint{}, err
	}
	next := c
	next.Tools = append([]ToolRecord(nil), c.Tools...)
	next.Tools[index].State = ToolFailed
	if outcome == "succeeded" {
		next.Tools[index].State = ToolSucceeded
	}
	output := string(result)
	next.Tools[index].Result = &output
	if err := next.Validate(1 << 20); err != nil {
		return Checkpoint{}, err
	}
	return next, nil
}
