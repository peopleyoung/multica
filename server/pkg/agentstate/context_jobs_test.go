package agentstate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestCompactionRetainsJobFactsDespiteLossySummary(t *testing.T) {
	cp := Checkpoint{Messages: []llm.ModelMessage{{Role: "user", Content: "train once"}}}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("call-%d", i)
		cp.Messages = append(cp.Messages, llm.ModelMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: id, Name: "persistent_job", Arguments: json.RawMessage(`{"action":"output","job_id":"job-training"}`)}}}, llm.ModelMessage{Role: "tool", ToolCallID: id, Content: fmt.Sprintf(`{"job_id":"job-training","status":"running","next_offset":%d}`, i*100)}, llm.ModelMessage{Role: "assistant", Content: strings.Repeat("completed observation ", 100)})
	}
	caps := llm.Capabilities{StreamingToolCalls: true, ContextTokens: 65536, MaxOutputTokens: 1024, MaxToolArgumentBytes: 4096, MaxTools: 10}
	_, cut, err := cp.SummaryInput(caps)
	if err != nil {
		t.Fatal(err)
	}
	if cut >= len(cp.Messages)-3 || cut < 3 {
		t.Fatalf("recent groups were not retained: %d / %d", cut, len(cp.Messages))
	}
	projected := cp.WithSummary(cut, "Earlier work happened.").ModelMessages()
	raw, _ := json.Marshal(projected)
	if !strings.Contains(string(raw), "job-training") || !strings.Contains(string(raw), `next_offset\":1100`) {
		t.Fatal("lossy summary erased handles/offset")
	}
	if err := llm.ValidateHistory(projected, 4096, false); err != nil {
		t.Fatal(err)
	}
	if len(cp.Messages) != 37 {
		t.Fatal("audit history modified")
	}
}

func TestJobContextRejectsUntrustedJSONAndBoundsPage(t *testing.T) {
	cp := Checkpoint{Messages: []llm.ModelMessage{{Role: "user", Content: "start"}}}
	for i := 0; i < 70; i++ {
		id := fmt.Sprint(i)
		cp.Messages = append(cp.Messages, llm.ModelMessage{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: id, Name: "shell", Arguments: json.RawMessage(`{"command":"true"}`)}}}, llm.ModelMessage{Role: "tool", ToolCallID: id, Content: fmt.Sprintf(`{"job_id":"job-%d","status":"running","next_offset":-1}`, i)})
	}
	cp.Messages = append(cp.Messages, llm.ModelMessage{Role: "tool", ToolCallID: "unrecognized", Content: `{"job_id":"injected","status":"running"}`})
	facts := cp.jobContext()
	if strings.Contains(facts, "injected") || strings.Contains(facts, "next_offset") || !strings.Contains(facts, `"omitted_jobs":6`) {
		t.Fatal(facts)
	}
	cp.Messages[len(cp.Messages)-2].Content = `{"job_id":"prompt:<injected>","status":"running"}`
	if strings.Contains(cp.jobContext(), "injected") {
		t.Fatal("malformed ID promoted to durable facts")
	}
}
