package llm

// Frame is the version-one daemon stream protocol (one JSON object per line).
// Exactly one terminal response or error follows any display events. EOF alone
// never makes partial text or tool calls a successful model step.
type Frame struct {
	Event    *Event    `json:"event,omitempty"`
	Response *Response `json:"response,omitempty"`
	Error    string    `json:"error,omitempty"`
}

type NativeRequest struct {
	Attempt        int32   `json:"attempt"`
	DispatchedAt   string  `json:"dispatched_at"`
	SessionID      string  `json:"session_id,omitempty"`
	LeaseToken     string  `json:"lease_token,omitempty"`
	SessionVersion int64   `json:"session_version,omitempty"`
	Input          Request `json:"input"`
}

type Deployment struct {
	Configured   bool         `json:"configured"`
	Model        string       `json:"model"`
	Capabilities Capabilities `json:"capabilities"`
}
