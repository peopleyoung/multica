package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func TestNativeCapturedQuickCreateInputUsesImmutableClonedDocuments(t *testing.T) {
	ctx := context.Background()
	runtimeID := dbfx.Runtime(t, "native capture ingress", testutil.Cols{"provider": "native", "metadata": []byte(`{"capabilities":["source_context_quick_create_v1"]}`)})
	agentID := dbfx.Agent(t, "native capture ingress", runtimeID)
	issueID := dbfx.Issue(t, "Historical objective")
	anchorID := dbfx.Comment(t, issueID, "Historical facts")
	storage := &mockStorage{}
	storage.put("original.txt", []byte("immutable facts"))
	originalID := dbfx.Insert(t, "attachment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issueID, "comment_id": anchorID, "uploader_type": "member", "uploader_id": testUserID, "filename": "original.txt", "url": storage.ObjectURL("original.txt"), "content_type": "text/plain", "size_bytes": int64(15)})
	h := *testHandler
	h.Storage = storage
	var preview sourceContextPreviewResponse
	testutil.Call(t, h.PreviewCommentSubIssue, withURLParam(newRequest(http.MethodGet, "/api/comments/"+anchorID+"/sub-issue-preview", nil), "commentId", anchorID)).Want(http.StatusOK).JSON(&preview)
	var queued QuickCreateIssueResponse
	testutil.Call(t, h.CreateCommentSubIssue, withURLParam(newRequest(http.MethodPost, "/api/comments/"+anchorID+"/sub-issues", map[string]any{
		"mode": "agent", "capture_token": preview.CaptureToken,
		"quick_create": map[string]any{"agent_id": agentID, "prompt": "Create a new objective using the historical facts", "priority": "high", "due_date": "2026-12-01"},
	}), "commentId", anchorID)).Want(http.StatusAccepted).JSON(&queued)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM attachment WHERE source_context_id IN (SELECT id FROM issue_source_context WHERE origin_task_id=$1)", queued.TaskID)
		_, _ = testPool.Exec(context.Background(), "DELETE FROM issue_source_context WHERE origin_task_id=$1", queued.TaskID)
		_, _ = testPool.Exec(context.Background(), "DELETE FROM agent_task_queue WHERE id=$1", queued.TaskID)
	})
	task, err := h.Queries.GetAgentTask(ctx, parseUUID(queued.TaskID))
	if err != nil {
		t.Fatal(err)
	}
	var quick service.QuickCreateContext
	if json.Unmarshal(task.Context, &quick) != nil || quick.SourceContextID == "" || quick.ParentIssueID != issueID || task.OriginatorUserID != parseUUID(testUserID) {
		t.Fatalf("capture ingress lost native input provenance: %s", task.Context)
	}
	row, err := h.Queries.GetIssueSourceContextByID(ctx, db.GetIssueSourceContextByIDParams{WorkspaceID: parseUUID(testWorkspaceID), ID: parseUUID(quick.SourceContextID)})
	var snapshot service.SourceContextSnapshot
	if err != nil || json.Unmarshal(row.Snapshot, &snapshot) != nil || len(snapshot.CommentThread) != 1 || len(snapshot.CommentThread[0].Attachments) != 1 {
		t.Fatalf("capture ingress lost cloned historical snapshot: %+v error=%v", row, err)
	}
	cloneID := snapshot.CommentThread[0].Attachments[0].ID
	if cloneID == originalID {
		t.Fatal("source capture reused the mutable original attachment")
	}
	storage.put("original.txt", []byte("changed source"))
	r := httptest.NewRequest(http.MethodPost, "/native/attachment", nil)
	if _, data, err := h.nativeAttachment(r, task, cloneID); err != nil || string(data) != "immutable facts" {
		t.Fatalf("native input could not read its immutable captured document: %q error=%v", data, err)
	}
	if _, _, err := h.nativeAttachment(r, task, originalID); err == nil {
		t.Fatal("native input read the mutable source document")
	}
}

func TestNativeQuickCreateAttachmentsFollowSelectedAndCapturedScope(t *testing.T) {
	ctx := context.Background()
	runtimeID := dbfx.Runtime(t, "native capture", testutil.Cols{"provider": "native"})
	agentID := dbfx.Agent(t, "native capture", runtimeID)
	issueID := dbfx.Issue(t, "captured issue")
	anchorID := dbfx.Comment(t, issueID, "historical facts")
	storage := &mockStorage{files: map[string][]byte{"capture.txt": []byte("captured facts")}}
	attachment := func(cols testutil.Cols) string {
		cols["workspace_id"], cols["uploader_type"], cols["uploader_id"] = testWorkspaceID, "member", testUserID
		cols["filename"], cols["url"], cols["content_type"], cols["size_bytes"] = "capture.txt", "https://cdn.example.com/capture.txt", "text/plain", int64(14)
		return dbfx.Insert(t, "attachment", cols)
	}
	selectedID, unselectedID := attachment(testutil.Cols{}), attachment(testutil.Cols{})
	quick := service.QuickCreateContext{Type: service.QuickCreateContextType, WorkspaceID: testWorkspaceID, RequesterID: testUserID, AttachmentIDs: []string{selectedID}}
	raw, _ := json.Marshal(quick)
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "status": "running", "originator_user_id": testUserID, "accountable_user_id": testUserID, "context": raw})
	quick.SourceContextID = dbfx.Insert(t, "issue_source_context", testutil.Cols{
		"id":           uuid.NewString(),
		"workspace_id": testWorkspaceID, "origin_task_id": taskID, "source_issue_id": issueID, "anchor_comment_id": anchorID,
		"captured_by_user_id": testUserID, "snapshot_version": int16(1), "snapshot": []byte(`{}`), "capture_digest": "native-fixture", "state": "pending",
	})
	capturedID := attachment(testutil.Cols{"source_context_id": quick.SourceContextID})
	raw, _ = json.Marshal(quick)
	dbfx.Exec(t, "UPDATE agent_task_queue SET context=$2 WHERE id=$1", taskID, raw)
	task, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(taskID))
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{Queries: testHandler.Queries, TaskService: testHandler.TaskService, Storage: storage}
	r := httptest.NewRequest("GET", "/fixture", nil)
	for _, id := range []string{selectedID, capturedID} {
		if _, data, err := h.nativeAttachment(r, task, id); err != nil || string(data) != "captured facts" {
			t.Fatalf("run could not read its selected/captured document: id=%s error=%v", id, err)
		}
	}
	if _, _, err := h.nativeAttachment(r, task, unselectedID); err == nil {
		t.Fatal("quick-create read an unselected workspace upload")
	}
	// Reading must follow the target issue after capture attachment, never the
	// mutable source issue or an unrelated run in the same workspace.
	targetID := dbfx.Issue(t, "capture target", testutil.Cols{"origin_type": "quick_create", "origin_id": taskID})
	dbfx.Exec(t, "UPDATE attachment SET issue_id=$2 WHERE id=$1", selectedID, targetID)
	dbfx.Exec(t, "UPDATE issue_source_context SET state='attached', issue_id=$2, attached_at=now() WHERE id=$1", quick.SourceContextID, targetID)
	for _, id := range []string{selectedID, capturedID} {
		if _, _, err := h.nativeAttachment(r, task, id); err != nil {
			t.Fatalf("creation made its own attachment unreadable: %v", err)
		}
	}
	task.IssueID = parseUUID(targetID)
	if _, _, err := h.nativeAttachment(r, task, capturedID); err != nil {
		t.Fatalf("target issue could not read captured document: %v", err)
	}
	task.IssueID = parseUUID(issueID)
	if _, _, err := h.nativeAttachment(r, task, capturedID); err == nil {
		t.Fatal("source issue read the target's immutable capture")
	}
	task.IssueID.Valid = false
	dbfx.Exec(t, "UPDATE attachment SET issue_id=$2 WHERE id=$1", selectedID, issueID)
	if _, _, err := h.nativeAttachment(r, task, selectedID); err == nil {
		t.Fatal("quick-create followed a selected upload moved into another issue")
	}
}

func TestNativeAttachmentsStayWithinRunScopeAndRespectCapabilities(t *testing.T) {
	const machine, token = "native-media-machine", "mdt_native_media_fixture"
	dispatched := time.Now().UTC().Truncate(time.Microsecond)
	runtimeID := dbfx.Runtime(t, "native media", testutil.Cols{"provider": "native", "daemon_id": machine})
	agentID := dbfx.Agent(t, "native media", runtimeID)
	issueID := dbfx.Issue(t, "native media")
	otherIssue := dbfx.Issue(t, "unrelated media")
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "attempt": 1, "dispatched_at": dispatched})
	dbfx.Insert(t, "daemon_token", testutil.Cols{"workspace_id": testWorkspaceID, "daemon_id": machine, "token_hash": auth.HashToken(token), "expires_at": time.Now().Add(time.Hour)})
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	storage := &mockStorage{files: map[string][]byte{"readme": []byte("attachment facts"), "picture": picture.Bytes(), "pdf": []byte("%PDF-binary")}}
	att := func(issue, key, ct string) string {
		return dbfx.Insert(t, "attachment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issue, "uploader_type": "member", "uploader_id": testUserID, "filename": key, "url": "https://cdn.example.com/" + key, "content_type": ct, "size_bytes": int64(len(storage.files[key]))})
	}
	textID, imageID, pdfID, otherID := att(issueID, "readme", "text/plain"), att(issueID, "picture", "image/png"), att(issueID, "pdf", "application/pdf"), att(otherIssue, "readme", "text/plain")
	caps := llm.Capabilities{StreamingToolCalls: true, ContextTokens: 32768, MaxOutputTokens: 1024, MaxToolArgumentBytes: 4096, MaxTools: 32}
	h := &Handler{Queries: testHandler.Queries, TaskService: testHandler.TaskService, Storage: storage, LLM: llm.New(llm.Config{BaseURL: "http://unused.invalid", DefaultModel: "fixture", Capabilities: caps})}
	r := httptest.NewRequest("POST", "/native/inference", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r = r.WithContext(middleware.WithDaemonContext(context.Background(), testWorkspaceID, machine))
	input := llm.NativeRequest{Attempt: 1, DispatchedAt: dispatched.Format(time.RFC3339Nano), Input: llm.Request{MaxOutputTokens: 1024, Messages: []llm.ModelMessage{{Role: "user", Content: "analyze", Attachments: []llm.AttachmentRef{{ID: textID}}}}}}
	result, err := h.hydrateNativeAttachments(r, taskID, input)
	if err != nil || !strings.Contains(result.Messages[0].Content, "attachment facts") {
		t.Fatalf("text: %v", err)
	}
	if input.Input.Messages[0].Content != "analyze" {
		t.Fatal("attachment hydration mutated durable history")
	}
	for _, id := range []string{otherID, pdfID, imageID} {
		input.Input.Messages[0].Attachments = []llm.AttachmentRef{{ID: id}}
		if _, err := h.hydrateNativeAttachments(r, taskID, input); err == nil {
			t.Fatalf("unsupported/out of scope attachment accepted: %s", id)
		}
	}
	caps.ImageInput = true
	h.LLM = llm.New(llm.Config{BaseURL: "http://unused.invalid", DefaultModel: "fixture", Capabilities: caps})
	input.Input.Messages[0].Attachments = []llm.AttachmentRef{{ID: imageID}}
	result, err = h.hydrateNativeAttachments(r, taskID, input)
	if err != nil || len(result.Messages[0].Images) != 1 || !bytes.Equal(result.Messages[0].Images[0].Data, picture.Bytes()) {
		t.Fatalf("image: %v", err)
	}
	_, err = h.Queries.GetAttachment(context.Background(), db.GetAttachmentParams{ID: parseUUID(otherID), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
}
