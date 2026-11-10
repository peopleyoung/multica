package agentstate

import (
	"bytes"
	"encoding/json"
)

// ProjectManagementResponse is shared by initial delivery and the durable
// receipt. A new credential field in an ordinary API response must not silently
// become model context, even when the originating human may read it in the UI.
func ProjectManagementResponse(tool string, body json.RawMessage) (json.RawMessage, error) {
	var keys []string
	switch tool {
	case "multica_manage_agent":
		keys = []string{"id", "workspace_id", "runtime_id", "runtime_bound", "runtime_availability", "name", "description", "instructions", "system_key", "status", "visibility", "permission_mode", "owner_id", "model", "skills", "enabled", "created_at", "updated_at"}
	case "multica_manage_autopilot":
		keys = []string{"id", "title", "description", "status", "execution_mode", "issue_title_template", "assignee_type", "assignee_id", "project_id", "last_run_at", "kind", "enabled", "cron_expression", "timezone", "label", "next_run_at", "provider", "event_filters", "can_write"}
	default:
		return body, nil
	}
	body = bytes.TrimSpace(body)
	var list []json.RawMessage
	if len(body) > 0 && body[0] == '[' {
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, err
		}
		for i, item := range list {
			projected, err := ProjectManagementResponse(tool, item)
			if err != nil {
				return nil, err
			}
			list[i] = projected
		}
		return json.Marshal(list)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage)
	for _, key := range keys {
		if raw, ok := object[key]; ok {
			out[key] = raw
		}
	}
	if tool == "multica_manage_agent" {
		if skills, ok := object["skills"]; ok {
			projected, err := ProjectManagementResponse(tool, skills)
			if err != nil {
				return nil, err
			}
			out["skills"] = projected
		}
	}
	if tool == "multica_manage_autopilot" {
		for _, key := range []string{"autopilot", "autopilots", "triggers"} {
			if nested, ok := object[key]; ok {
				projected, err := ProjectManagementResponse(tool, nested)
				if err != nil {
					return nil, err
				}
				out[key] = projected
			}
		}
	}
	return json.Marshal(out)
}
