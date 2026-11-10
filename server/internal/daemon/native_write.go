package daemon

import (
	"context"
	"errors"

	"github.com/multica-ai/multica/server/pkg/agentstate"
)

// ReconcileToolOutcomes observes server-owned receipts before exposing an
// uncertain business result to the model. It cannot execute the original write.
func (s *nativeStore) ReconcileToolOutcomes(ctx context.Context, current agentstate.Session) (agentstate.Session, error) {
	return s.reconcileWrites(ctx, current)
}

func (s *nativeStore) reconcileWrites(ctx context.Context, current agentstate.Session) (agentstate.Session, error) {
	for i := range current.Checkpoint.Tools {
		record := current.Checkpoint.Tools[i]
		if record.State != agentstate.ToolStarted && record.State != agentstate.ToolUnknown || !agentstate.NativeWriteTool(record.Call.Name) {
			continue
		}
		var result agentstate.WriteResult
		identity := agentstate.Session{ID: current.ID, Version: current.Version, LeaseToken: current.LeaseToken}
		if err := s.client.postJSON(ctx, s.path("write-result"), agentstate.WriteResultRequest{Claim: s.claim(), Session: identity, CallID: record.Call.ID}, &result); err != nil {
			return current, errors.New("native write receipt could not be read; reconcile before continuing")
		}
		if !result.Known {
			continue
		}
		output, failed, err := result.ToolOutput(record.Call.Name)
		if err != nil || result.Session == nil {
			return current, errors.New("native write receipt is invalid; reconcile before continuing")
		}
		next := *result.Session
		if err := validateNativeSession(&next); err != nil {
			return current, err
		}
		if next.ID != current.ID || next.LeaseToken != current.LeaseToken || next.Version != current.Version+1 || len(next.Checkpoint.Tools) != len(current.Checkpoint.Tools) {
			return current, errors.New("native write reconciliation changed execution identity")
		}
		reconciled := next.Checkpoint.Tools[i]
		state := agentstate.ToolSucceeded
		if failed {
			state = agentstate.ToolFailed
		}
		if reconciled.State != state || reconciled.Result == nil || *reconciled.Result != output {
			return current, errors.New("native write reconciliation does not match its receipt")
		}
		// An unknown result may be resolved only by this server-owned operation.
		// Validate all other history and tool invariants as an ordinary finish.
		previous := current.Checkpoint
		previous.Tools = append([]agentstate.ToolRecord(nil), previous.Tools...)
		previous.Tools[i].State = agentstate.ToolStarted
		if err := agentstate.ValidateTransition(previous, next.Checkpoint, 1<<20); err != nil {
			return current, err
		}
		current = next
	}
	return current, nil
}
