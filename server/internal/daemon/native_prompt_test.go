package daemon

import (
	"strings"
	"testing"
)

func TestNativePromptsPreserveRunContextWithoutCLIInstructionsOrCredentials(t *testing.T) {
	for _, tc := range []struct {
		name string
		task Task
		want []string
	}{
		{"issue thread", Task{IssueID: "issue", TriggerCommentID: "trigger", TriggerThreadID: "thread", TriggerCommentContent: "human objective", CoalescedCommentIDs: []string{"earlier"}, HandoffNote: "scope", WakeupID: "wakeup"}, []string{"human objective", "earlier", "thread", "wakeup", "scope", "multica_get_issue"}},
		{"channel", Task{ChatSessionID: "chat", ChatChannelType: "slack", ChatType: "group", ChatMessage: "user objective"}, []string{"user objective", "group", "text delivery only", "final outcome only"}},
		{"file channel", Task{ChatSessionID: "chat", ChatChannelType: "slack", ChatChannelDeliversFiles: true}, []string{"multica_upload_artifact", "separately after the text"}},
		{"automation", Task{AutopilotRunID: "automation-run", AutopilotID: "automation", AutopilotDescription: "trigger instructions", AutopilotTriggerPayload: []byte(`{"event":"facts"}`)}, []string{"trigger instructions", "facts", "run-only automation"}},
		{"quick-create", Task{QuickCreatePrompt: "new objective", QuickCreatePriority: "high", ParentIssueID: "parent", ProjectID: "project", SquadID: "squad"}, []string{"new objective", "high", "parent", "project", "squad", "exactly one issue"}},
		{"squad leader", Task{IssueID: "issue", SquadID: "squad", IsLeaderTask: true}, []string{"multica_record_squad_evaluation", "no_action", "post no comment"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.task.ID = "run"
			tc.task.AuthToken = "mat_secret_never_in_prompt"
			tc.task.RemoteMCPDaemonToken = "mdt_secret_never_in_prompt"
			prompt := BuildPrompt(tc.task, "native", WithSharedLocalDirectory())
			for _, want := range tc.want {
				if !strings.Contains(prompt, want) {
					t.Errorf("missing run context %q", want)
				}
			}
			for _, forbidden := range []string{"mat_secret", "mdt_secret", "`multica ", "--output", "--content-file"} {
				if strings.Contains(prompt, forbidden) {
					t.Errorf("native prompt contains %q", forbidden)
				}
			}
			if !strings.Contains(prompt, "Shared working directory") {
				t.Fatal("native prompt dropped daemon-local concurrency context")
			}
		})
	}
}

// Native bypasses the CLI runtime brief, so every issue entry path must still
// deliver the issue lifecycle and cross-issue handoff contract to the model.
func TestNativeIssueTurnsIncludeStatusAndParentHandoffWorkflow(t *testing.T) {
	for _, task := range []Task{
		{IssueID: "child-issue"},
		{IssueID: "child-issue", TriggerCommentID: "human-trigger"},
		{IssueID: "parent-issue", SquadID: "squad", IsLeaderTask: true, LeaderRoleResolved: true},
	} {
		prompt := strings.Join(strings.Fields(BuildPrompt(task, "native")), " ")
		for _, want := range []string{
			"set `in_progress` before starting",
			"delivered the issue's requested outcome",
			"set `in_review`",
			"`done` stays human",
			"set `blocked`",
			"Ancillary replies",
			"Agent Identity forbids",
			"expected_revision",
			"parent_issue_id",
			"mention://squad/",
			"mention://agent/",
			"new top-level comment on the parent",
			"child_done",
			"all children",
			"does not notify the coordinator",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("issue=%s trigger=%s: missing workflow %q", task.IssueID, task.TriggerCommentID, want)
			}
		}
		if task.IsLeaderTask && !strings.Contains(prompt, "dispatching members is not delivery") {
			t.Error("leader workflow permits premature parent completion")
		}
	}
	for _, task := range []Task{
		{ChatSessionID: "chat"},
		{QuickCreatePrompt: "new issue"},
		{AutopilotRunID: "automation"},
	} {
		if strings.Contains(BuildPrompt(task, "native"), "Native issue delivery workflow:") {
			t.Error("issue lifecycle leaked into a non-issue run")
		}
	}
}
