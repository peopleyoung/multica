package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/internal/modelcontrol"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/llm"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestModelControlSharesConcurrencyAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	workspace := dbfx.Workspace(t, "Shared model quota", "model-quota-"+uuid.NewString())
	dbfx.Cleanup(t, "DELETE FROM model_call WHERE workspace_id=$1", workspace)
	limits := modelcontrol.Limits{GlobalConcurrent: 1000, WorkspaceConcurrent: 2, GlobalRequestsPerMinute: 100000, WorkspaceRequestsPerMinute: 60, GlobalTokensPerMinute: 1_000_000_000, WorkspaceTokensPerMinute: 1_000_000}
	replicas := []*modelcontrol.Controller{modelcontrol.New(testPool, limits, nil), modelcontrol.New(testPool, limits, nil)}
	var mu sync.Mutex
	var finishes []func(context.Context, llm.CallResult) error
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			finish, err := replicas[i%2].Acquire(ctx, llm.Call{Scope: llm.CallScope{WorkspaceID: workspace, TaskID: uuid.NewString(), Purpose: "execution"}, Model: "fixture", ReservedTokens: 100})
			if err != nil {
				if !errors.Is(err, llm.ErrQuotaExceeded) {
					t.Errorf("admission failed unexpectedly: %v", err)
				}
				return
			}
			mu.Lock()
			finishes = append(finishes, finish)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	if len(finishes) != 2 {
		t.Fatalf("replicas admitted %d concurrent calls; want 2", len(finishes))
	}
	for _, finish := range finishes {
		if err := finish(ctx, llm.CallResult{Outcome: "completed"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := dbfx.Count(t, "SELECT count(*) FROM model_call WHERE workspace_id=$1 AND outcome='active'", workspace); got != 0 {
		t.Fatalf("completed calls retained %d active reservations", got)
	}
}

func TestModelToolMetricsIgnoreRepeatedDaemonBatches(t *testing.T) {
	taskID := seedBatchTask(t, "model-tool-metrics")
	h := *testHandler
	h.Metrics = metrics.NewBusinessMetrics()
	h.Bus = events.New()
	var mu sync.Mutex
	published := 0
	h.Bus.Subscribe(protocol.EventTaskMessage, func(e events.Event) {
		if e.TaskID == taskID {
			mu.Lock()
			published++
			mu.Unlock()
		}
	})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			testutil.Call(t, h.ReportTaskMessages, batchMessagesRequest(t, taskID, []any{
				map[string]any{"seq": 1, "type": "tool_use", "tool": "multica_create_comment", "call_id": "one"},
				map[string]any{"seq": 2, "type": "tool_result", "tool": "multica_create_comment", "call_id": "one", "output": "private tool result"},
			})).Want(http.StatusOK)
		}()
	}
	wg.Wait()
	if got := dbfx.Count(t, "SELECT count(*) FROM task_message WHERE task_id=$1", taskID); got != 2 {
		t.Fatalf("duplicate callbacks persisted %d messages; want 2", got)
	}
	mu.Lock()
	gotPublished := published
	mu.Unlock()
	if gotPublished != 2 {
		t.Fatalf("duplicate callbacks published %d events; want 2", gotPublished)
	}
	for _, family := range metrics.GatherForTest(t, h.Metrics) {
		if family.GetName() != "multica_tool_events_total" {
			continue
		}
		if len(family.Metric) != 2 {
			t.Fatalf("tool events missing: %+v", family)
		}
		for _, metric := range family.Metric {
			if metric.GetCounter().GetValue() != 1 {
				t.Fatal("duplicate callback incremented tool metrics")
			}
		}
		return
	}
	t.Fatal("persisted tool events were not observable")
}

func TestModelControlSharesRunSummaryAndAuxiliaryBudgets(t *testing.T) {
	ctx := context.Background()
	for _, budget := range []string{"requests", "tokens", "scope", "expired"} {
		t.Run(budget, func(t *testing.T) {
			workspace := dbfx.Workspace(t, "Shared model budget", "model-budget-"+uuid.NewString())
			dbfx.Cleanup(t, "DELETE FROM model_call WHERE workspace_id=$1", workspace)
			limits := modelcontrol.Limits{GlobalConcurrent: 1000, WorkspaceConcurrent: 4, GlobalRequestsPerMinute: 100000, WorkspaceRequestsPerMinute: 3, GlobalTokensPerMinute: 1_000_000_000, WorkspaceTokensPerMinute: 1000}
			if budget == "tokens" {
				limits.WorkspaceTokensPerMinute = 150
			}
			a, b := modelcontrol.New(testPool, limits, nil), modelcontrol.New(testPool, limits, nil)
			call := llm.Call{Scope: llm.CallScope{WorkspaceID: workspace, TaskID: uuid.NewString(), SessionID: uuid.NewString(), Purpose: "execution"}, Model: "fixture", ReservedTokens: 100}
			finish, err := a.Acquire(ctx, call)
			if err != nil {
				t.Fatal(err)
			}
			summary := call
			summary.Scope.Purpose = "summary"
			if budget == "scope" {
				summary.Scope.TaskID = strings.ToUpper(call.Scope.TaskID)
				if _, err := b.Acquire(ctx, summary); !errors.Is(err, llm.ErrQuotaExceeded) {
					t.Fatalf("summary overlapped the same run: %v", err)
				}
			} else if budget == "tokens" {
				summary.Scope.TaskID, summary.ReservedTokens = uuid.NewString(), 60
				if _, err := b.Acquire(ctx, summary); !errors.Is(err, llm.ErrQuotaExceeded) {
					t.Fatalf("summary bypassed reserved execution tokens: %v", err)
				}
			} else if budget == "expired" {
				dbfx.Exec(t, "UPDATE model_call SET lease_until=now()-interval '1 second' WHERE workspace_id=$1", workspace)
				f, err := b.Acquire(ctx, summary)
				if err != nil {
					t.Fatalf("expired replica blocked recovery: %v", err)
				}
				if err := f(ctx, llm.CallResult{Outcome: "completed"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := finish(ctx, llm.CallResult{Outcome: "completed", Usage: &llm.Usage{InputTokens: 30, OutputTokens: 10, CachedTokens: 5}}); err != nil {
				t.Fatal(err)
			}
			// A duplicate completion cannot revise settled usage.
			if err := finish(ctx, llm.CallResult{Outcome: "failed", Usage: &llm.Usage{InputTokens: 999}}); err != nil {
				t.Fatal(err)
			}
			if got := dbfx.Count(t, "SELECT count(*) FROM model_call WHERE workspace_id=$1 AND purpose='execution' AND charged_tokens=40 AND cached_tokens=5 AND outcome='completed'", workspace); got != 1 {
				t.Fatal("usage was lost, cached tokens charged twice, or duplicate finish rewrote accounting")
			}
			if budget == "expired" {
				return
			}
			f, err := b.Acquire(ctx, summary)
			if err != nil {
				t.Fatal(err)
			}
			if err := f(ctx, llm.CallResult{Outcome: "completed"}); err != nil {
				t.Fatal(err)
			}
			title := call
			title.Scope.Purpose, title.ReservedTokens = "title", 51
			if budget == "tokens" {
				if _, err := a.Acquire(ctx, title); !errors.Is(err, llm.ErrQuotaExceeded) {
					t.Fatalf("missing summary usage became free tokens: %v", err)
				}
				return
			}
			f, err = a.Acquire(ctx, title)
			if err != nil {
				t.Fatal(err)
			}
			if err := f(ctx, llm.CallResult{Outcome: "completed"}); err != nil {
				t.Fatal(err)
			}
			title.Scope.Purpose = "quick_actions"
			if _, err := b.Acquire(ctx, title); !errors.Is(err, llm.ErrQuotaExceeded) {
				t.Fatalf("auxiliary request bypassed shared rate budget: %v", err)
			}
		})
	}
}

func TestModelControlRejectsMissingWorkspaceAndAttribution(t *testing.T) {
	c := modelcontrol.New(testPool, modelcontrol.DefaultLimits(), nil)
	for _, scope := range []llm.CallScope{
		{}, {WorkspaceID: testWorkspaceID, Purpose: "execution"},
		{WorkspaceID: testWorkspaceID, TaskID: uuid.NewString(), Purpose: "untrusted"},
		{WorkspaceID: uuid.NewString(), TaskID: uuid.NewString(), Purpose: "execution"},
	} {
		if _, err := c.Acquire(context.Background(), llm.Call{Scope: scope, ReservedTokens: 100}); !errors.Is(err, llm.ErrAdmissionUnavailable) {
			t.Fatalf("unattributed call admitted: %+v error=%v", scope, err)
		}
	}
}
