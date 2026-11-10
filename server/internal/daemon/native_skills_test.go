package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeSkillLoadingRespectsVisibilityAndBundleScope(t *testing.T) {
	host := &nativeToolHost{skills: execenv.NativeSkills([]execenv.SkillContextForEnv{
		{Name: "Review", Content: "Use the review rules", Files: []execenv.SkillFileContextForEnv{{Path: "references/check.md", Content: "Canonical checklist"}}},
		{Name: "Hidden", Content: "---\ndisable-model-invocation: true\n---\nprivate"},
	})}
	for _, args := range []string{`{"name":"review"}`, `{"name":"review","path":"references/check.md"}`} {
		out, err := host.Execute(context.Background(), llm.ToolCall{Name: "load_skill", Arguments: json.RawMessage(args)})
		if err != nil || !strings.Contains(out, "references/check.md") {
			t.Fatalf("%s: %v", out, err)
		}
	}
	for _, args := range []string{`{"name":"hidden"}`, `{"name":"review","path":"../AGENTS.md"}`, `{"name":"review","path":"other.md"}`, `{"name":"review","exec":true}`} {
		if _, err := host.Execute(context.Background(), llm.ToolCall{Name: "load_skill", Arguments: json.RawMessage(args)}); err == nil {
			t.Fatalf("accepted %s", args)
		}
	}
}
