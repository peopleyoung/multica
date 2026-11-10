package agentstate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfirmedWriteOutputSurvivesJSONBAndHTTPFormatting(t *testing.T) {
	stored := WriteResult{Known: true, Status: 201, Body: json.RawMessage(`{"id": "evaluation", "action": "squad_leader_evaluated", "revision": 9007199254740993, "content": "<result>"}`)}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	var received WriteResult
	if err := json.Unmarshal(encoded, &received); err != nil {
		t.Fatal(err)
	}
	before, _, err := stored.ToolOutput("multica_record_squad_evaluation")
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := received.ToolOutput("multica_record_squad_evaluation")
	if err != nil || before != after {
		t.Fatalf("HTTP serialization changed confirmed output: stored=%q received=%q error=%v", before, after, err)
	}
	if !strings.Contains(before, "9007199254740993") {
		t.Fatal("normalizing the receipt rounded a resource revision")
	}
}
