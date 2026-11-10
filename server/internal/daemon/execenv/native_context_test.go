package execenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeBriefRequiresNoCLIOrSidecarDiscovery(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("user rules"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := TaskContextForEnv{IssueID: "issue", AgentName: "native", WorkspaceContext: "workspace rules", AgentSkills: []SkillContextForEnv{{Name: "Review", Content: "review"}, {Name: "Hidden", Content: "---\ndisable-model-invocation: true\n---\nprivate"}}}
	brief, err := InjectRuntimeConfig(root, "native", ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"read_rules", "load_skill", "workspace rules", "`review`", "Do not delegate"} {
		if !strings.Contains(brief, required) {
			t.Errorf("missing %s", required)
		}
	}
	for _, forbidden := range []string{"multica issue", "Always Use CLI", "`hidden`", "discovered automatically"} {
		if strings.Contains(brief, forbidden) {
			t.Errorf("unexpected %s", forbidden)
		}
	}
	content, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil || string(content) != "user rules" {
		t.Fatal("native brief changed repository instructions")
	}
}
