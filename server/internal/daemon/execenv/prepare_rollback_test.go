package execenv

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPrepareRollsBackWhenWriteContextFilesFailsAfterMarker covers the earliest
// failure window there is: writeContextFiles lays the marker down as its very
// first act, then writes skills and project resources — either of which can
// fail with the marker already on disk.
//
// The first version of the MUL-6132 fix armed the rollback only after
// writeContextFiles returned, so exactly these failures still stranded a marker
// in the user's repository with nothing else beside it (MUL-6132 review). The
// induced failure is a plain file where .agent_context must be a directory;
// the task carries a skill because the default provider's skills tree
// (.agent_context/skills) is now the only thing that opens that directory.
func TestPrepareRollsBackWhenWriteContextFilesFailsAfterMarker(t *testing.T) {
	workspacesRoot := t.TempDir()
	userDir := t.TempDir()

	// A regular file at the path writeContextFiles needs as a directory, one
	// step after it writes the marker.
	blocker := filepath.Join(userDir, ".agent_context")
	if err := os.WriteFile(blocker, []byte("user file"), 0o644); err != nil {
		t.Fatalf("seed .agent_context blocker: %v", err)
	}

	_, err := Prepare(PrepareParams{
		WorkspacesRoot: workspacesRoot,
		WorkspaceID:    "ws-rollback-003",
		TaskID:         "cccccccc-dddd-eeee-ffff-000000000000",
		AgentName:      "Test Agent",
		LocalWorkDir:   userDir,
		Task: TaskContextForEnv{
			IssueID:     "33333333-4444-5555-6666-777777777777",
			AgentID:     "77777777-6666-5555-4444-333333333333",
			AgentSkills: []SkillContextForEnv{{Name: "Go Conventions", Content: "Follow Go conventions."}},
		},
	}, testLogger())
	if err == nil {
		t.Fatal("Prepare succeeded; expected the .agent_context blocker to fail it")
	}

	markerPath := filepath.Join(userDir, TaskContextMarkerRelPath)
	if _, statErr := os.Stat(markerPath); !os.IsNotExist(statErr) {
		t.Fatalf("daemon task marker survived a failure inside writeContextFiles at %s (stat err: %v)", markerPath, statErr)
	}
	if data, readErr := os.ReadFile(blocker); readErr != nil || string(data) != "user file" {
		t.Fatalf("rollback touched the user's own file: data=%q err=%v", string(data), readErr)
	}
}

// TestPrepareSucceedsInPlaceAndLeavesMarker guards the other half of the
// contract: the rollback fires only on failure. A Prepare that completes must
// still leave the marker in place — it is the guard that makes a CLI invocation
// from inside the task fail closed — and must persist the manifest that the
// task teardown later uses to remove it.
func TestPrepareSucceedsInPlaceAndLeavesMarker(t *testing.T) {
	workspacesRoot := t.TempDir()
	userDir := t.TempDir()

	env, err := Prepare(PrepareParams{
		WorkspacesRoot: workspacesRoot,
		WorkspaceID:    "ws-rollback-002",
		TaskID:         "bbbbbbbb-cccc-dddd-eeee-ffffffffffff",
		AgentName:      "Test Agent",
		LocalWorkDir:   userDir,
		Task: TaskContextForEnv{
			IssueID: "22222222-3333-4444-5555-666666666666",
			AgentID: "88888888-7777-6666-5555-444444444444",
		},
	}, testLogger())
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(userDir, TaskContextMarkerRelPath)); statErr != nil {
		t.Fatalf("successful Prepare did not leave the daemon task marker: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(env.RootDir, sidecarManifestFile)); statErr != nil {
		t.Fatalf("successful Prepare did not persist the sidecar manifest: %v", statErr)
	}

	// And the recorded manifest is what makes the marker removable later.
	if err := CleanupSidecars(env.RootDir); err != nil {
		t.Fatalf("CleanupSidecars: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(userDir, TaskContextMarkerRelPath)); !os.IsNotExist(statErr) {
		t.Fatalf("marker survived CleanupSidecars (stat err: %v)", statErr)
	}
}
