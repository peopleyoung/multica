package agentstate

import (
	"bytes"
	"encoding/json"
	"errors"
)

// ProjectBusinessResponse keeps attachment capabilities out of durable model
// history and write receipts. Only API-owned attachment collections are
// projected; user text and arbitrary nested payloads remain untouched.
func ProjectBusinessResponse(body json.RawMessage, upload bool) (json.RawMessage, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if !json.Valid(body) {
		return nil, errors.New("invalid business response")
	}
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if upload {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("invalid artifact response")
		}
		return json.Marshal(projectAttachment(object))
	}
	projectBusinessCollections(value)
	return json.Marshal(value)
}

func projectBusinessCollections(value any) {
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			projectBusinessCollections(item)
		}
	case map[string]any:
		if attachments, ok := value["attachments"].([]any); ok {
			for i, item := range attachments {
				if attachment, ok := item.(map[string]any); ok {
					attachments[i] = projectAttachment(attachment)
				}
			}
		}
		for _, key := range []string{"comments", "messages", "replies"} {
			if nested, ok := value[key]; ok {
				projectBusinessCollections(nested)
			}
		}
	}
}

func projectAttachment(source map[string]any) map[string]any {
	out := make(map[string]any)
	for _, key := range []string{"id", "filename", "content_type", "size_bytes", "created_at", "issue_id", "comment_id", "chat_session_id", "chat_message_id", "uploader_type", "uploader_id"} {
		if value, ok := source[key]; ok {
			out[key] = value
		}
	}
	if id, ok := source["id"].(string); ok && validAttachmentID(id) {
		out["download_path"] = "/api/attachments/" + id + "/download"
	}
	return out
}

func validAttachmentID(id string) bool {
	if id == "" || len(id) > 200 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
