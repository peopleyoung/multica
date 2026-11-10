package daemon

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeProgressUsesManagementOperationEffects(t *testing.T) {
	host := &nativeToolHost{}
	for _, spec := range nativeManagementSpecs {
		for _, operation := range spec.operations {
			raw, _ := json.Marshal(map[string]string{"action": operation.action})
			if got := host.IsReadOnly(llm.ToolCall{Name: spec.name, Arguments: raw}); got != (operation.method == http.MethodGet) {
				t.Errorf("wrong effect for %s/%s", spec.name, operation.action)
			}
		}
	}
	for _, name := range []string{"multica_create_comment", "multica_create_issue", "multica_update_issue_status", "multica_upload_artifact", "mcp_unknown", "shell"} {
		if host.IsReadOnly(llm.ToolCall{Name: name, Arguments: json.RawMessage(`{}`)}) {
			t.Errorf("write or unknown effect classified read-only: %s", name)
		}
	}
}
