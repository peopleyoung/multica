package agentstate

import "github.com/multica-ai/multica/server/pkg/llm"

type SessionRequest struct {
	Claim      llm.NativeRequest `json:"claim"`
	WorkDir    string            `json:"work_dir"`
	LeaseToken string            `json:"lease_token"`
}

type Session struct {
	ID         string     `json:"id"`
	Version    int64      `json:"version"`
	LeaseToken string     `json:"lease_token"`
	Checkpoint Checkpoint `json:"checkpoint"`
}

type CheckpointRequest struct {
	Claim   llm.NativeRequest `json:"claim"`
	Session Session           `json:"session"`
}
