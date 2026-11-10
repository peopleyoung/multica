package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeSquadEvaluationIsPinnedToLeaderRun(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/api/issues/assigned-issue/squad-evaluated" || r.Header.Get("Authorization") != "Bearer mat_leader" {
			t.Errorf("evaluation escaped its run: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 2 || body["outcome"] != "no_action" || body["reason"] != "Already addressed" {
			t.Errorf("evaluation payload=%v error=%v", body, err)
		}
		_, _ = w.Write([]byte(`{"id":"evaluation"}`))
	}))
	defer server.Close()
	host := &nativeToolHost{control: NewClient(server.URL), task: Task{IssueID: "assigned-issue", SquadID: "squad", IsLeaderTask: true, AuthToken: "mat_leader", WorkspaceID: "workspace"}}
	call := llm.ToolCall{Name: "multica_record_squad_evaluation", Arguments: json.RawMessage(`{"outcome":"no_action","reason":"Already addressed"}`)}
	if _, err := host.Execute(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{
		`{"outcome":"no_action"}`,
		`{"outcome":"other","reason":"x"}`,
		`{"outcome":"action","reason":" "}`,
		`{"outcome":"no_action","reason":"x","issue_id":"another-issue"}`,
		`{"outcome":"no_action","reason":null}`,
	} {
		call.Arguments = json.RawMessage(args)
		if _, err := host.Execute(context.Background(), call); err == nil {
			t.Errorf("accepted invalid evaluation %s", args)
		}
	}
	call.Arguments = json.RawMessage(`{"outcome":"no_action","reason":"Already addressed"}`)
	for _, task := range []Task{
		{IssueID: "assigned-issue", SquadID: "squad"},
		{SquadID: "squad", IsLeaderTask: true},
		{IssueID: "assigned-issue", IsLeaderTask: true},
	} {
		host.task = task
		if _, err := host.Execute(context.Background(), call); err == nil {
			t.Errorf("non-leader run recorded evaluation: %+v", task)
		}
	}
	if requests != 1 {
		t.Fatalf("invalid evaluation reached API: %d requests", requests)
	}
}
