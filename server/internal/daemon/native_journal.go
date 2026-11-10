package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/pkg/agentstate"
)

// nativePendingResult contains no credentials. It preserves a locally observed
// tool outcome until PostgreSQL confirms it; it never authorizes execution.
type nativePendingResult struct {
	SessionID  string                `json:"session_id"`
	Version    int64                 `json:"version"`
	Checkpoint agentstate.Checkpoint `json:"checkpoint"`
}

func (s *nativeStore) pendingPath(sessionID string) (string, error) {
	if s.journalDir == "" {
		return "", errors.New("native result journal is not configured")
	}
	if _, err := uuid.Parse(sessionID); err != nil {
		return "", errors.New("invalid native session identity")
	}
	return filepath.Join(s.journalDir, sessionID+".json"), nil
}

func (s *nativeStore) stageResult(session agentstate.Session) error {
	path, err := s.pendingPath(session.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.journalDir, 0700); err != nil {
		return err
	}
	raw, err := json.Marshal(nativePendingResult{SessionID: session.ID, Version: session.Version, Checkpoint: session.Checkpoint})
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(s.journalDir, ".result-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	_, writeErr := temp.Write(raw)
	syncErr := temp.Sync()
	closeErr := temp.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return err
	}
	return syncNativeJournalDir(s.journalDir)
}

func (s *nativeStore) removePending(sessionID string) error {
	path, err := s.pendingPath(sessionID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncNativeJournalDir(s.journalDir)
}

func (s *nativeStore) reconcilePending(ctx context.Context, current agentstate.Session) (agentstate.Session, error) {
	path, err := s.pendingPath(current.ID)
	if err != nil {
		return current, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return current, nil
	}
	if err != nil {
		return current, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return current, err
	}
	if !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return current, errors.New("invalid native pending result file")
	}
	var pending nativePendingResult
	if err := json.NewDecoder(file).Decode(&pending); err != nil {
		return current, err
	}
	if pending.SessionID != current.ID {
		return current, errors.New("native result journal session mismatch")
	}
	if current.Version > pending.Version {
		// A newer authoritative checkpoint already committed the outcome or
		// its subsequent history. Never replace it with the older local copy.
		return current, s.removePending(current.ID)
	}
	if current.Version != pending.Version {
		return current, errors.New("native result journal is ahead of the server")
	}
	pending.Checkpoint.Version = current.Version
	if err := agentstate.ValidateTransition(current.Checkpoint, pending.Checkpoint, 1<<20); err != nil {
		return current, err
	}
	current.Checkpoint = pending.Checkpoint
	return s.Save(ctx, current)
}
