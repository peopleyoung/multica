package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/pkg/agentstate"
	"github.com/multica-ai/multica/server/pkg/llm"
)

// nativeStore binds every checkpoint request to one claimed run and executor.
// No retry is hidden here: an unconfirmed save stops the loop before more tools.
type nativeStore struct {
	client     *Client
	task       Task
	leaseToken string
	journalDir string
}

func newNativeStore(client *Client, task Task) *nativeStore {
	return &nativeStore{client: client, task: task, leaseToken: uuid.NewString()}
}
func (s *nativeStore) claim() llm.NativeRequest {
	return llm.NativeRequest{Attempt: s.task.Attempt, DispatchedAt: s.task.DispatchedAt}
}
func (s *nativeStore) path(operation string) string {
	return fmt.Sprintf("/api/daemon/tasks/%s/native/%s", s.task.ID, operation)
}

func (s *nativeStore) Acquire(ctx context.Context, workDir string) (agentstate.Session, error) {
	var session agentstate.Session
	err := s.client.postJSON(ctx, s.path("session"), agentstate.SessionRequest{Claim: s.claim(), WorkDir: workDir, LeaseToken: s.leaseToken}, &session)
	if err == nil {
		err = validateNativeSession(&session)
	}
	if err == nil {
		session, err = s.reconcilePending(ctx, session)
	}
	if err == nil {
		session, err = s.reconcileWrites(ctx, session)
	}
	return session, err
}
func (s *nativeStore) Save(ctx context.Context, session agentstate.Session) (agentstate.Session, error) {
	hasOutcome := false
	for _, record := range session.Checkpoint.Tools {
		if record.State == agentstate.ToolSucceeded || record.State == agentstate.ToolFailed || record.State == agentstate.ToolUnknown {
			hasOutcome = true
		}
	}
	if hasOutcome {
		if err := s.stageResult(session); err != nil {
			return agentstate.Session{}, err
		}
	}
	var saved agentstate.Session
	err := s.client.postJSON(ctx, s.path("checkpoint/save"), agentstate.CheckpointRequest{Claim: s.claim(), Session: session}, &saved)
	if err == nil {
		err = validateNativeSession(&saved)
	}
	if err == nil && hasOutcome {
		err = s.removePending(session.ID)
	}
	return saved, err
}
func (s *nativeStore) Renew(ctx context.Context, session agentstate.Session) error {
	var renewed agentstate.Session
	if err := s.client.postJSON(ctx, s.path("checkpoint/renew"), agentstate.CheckpointRequest{Claim: s.claim(), Session: session}, &renewed); err != nil {
		return err
	}
	return validateNativeSession(&renewed)
}
func (s *nativeStore) Release(ctx context.Context, session agentstate.Session) error {
	return s.client.postJSON(ctx, s.path("checkpoint/release"), agentstate.CheckpointRequest{Claim: s.claim(), Session: session}, nil)
}

func (s *nativeStore) Compact(ctx context.Context, session agentstate.Session) (agentstate.Session, error) {
	var compacted agentstate.Session
	// Summary generation has the same lifetime as an inference step. Keep the
	// control-plane timeout short for ordinary checkpoint and lease requests.
	client := &http.Client{Transport: s.client.client.Transport, Timeout: 10*time.Minute + 5*time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if err := s.client.postJSONVia(ctx, client, s.path("checkpoint/compact"), agentstate.CheckpointRequest{Claim: s.claim(), Session: agentstate.Session{ID: session.ID, Version: session.Version, LeaseToken: session.LeaseToken}}, &compacted); err != nil {
		return agentstate.Session{}, err
	}
	if err := validateNativeSession(&compacted); err != nil {
		return agentstate.Session{}, err
	}
	return compacted, nil
}
func validateNativeSession(session *agentstate.Session) error {
	if _, err := uuid.Parse(session.ID); err != nil {
		return errors.New("native session response has an invalid identity")
	}
	if _, err := uuid.Parse(session.LeaseToken); err != nil {
		return errors.New("native session response has an invalid lease")
	}
	if session.Version < 0 {
		return errors.New("native session response has an invalid version")
	}
	session.Checkpoint.Version = session.Version
	return session.Checkpoint.Validate(1 << 20)
}
