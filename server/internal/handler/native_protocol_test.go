package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/daemonws"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestNativeProtocolRegistration(t *testing.T) {
	for _, tc := range []struct {
		name, capability, profile string
		want                      int
	}{
		{"old daemon", "", "", http.StatusUpgradeRequired},
		{"unknown future capability", "native-runtime-v2", "", http.StatusUpgradeRequired},
		{"CLI profile", protocol.DaemonCapabilityNativeRuntimeV1, "fixture-profile", http.StatusBadRequest},
		{"current daemon", protocol.DaemonCapabilityNativeRuntimeV1, "", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			machine := "protocol-register-" + tc.name
			dbfx.Cleanup(t, "DELETE FROM agent_runtime WHERE workspace_id=$1 AND daemon_id=$2", testWorkspaceID, machine)
			r := newRequest(http.MethodPost, "/api/daemon/register", map[string]any{
				"workspace_id": testWorkspaceID, "daemon_id": machine, "cli_version": "9999.0.0",
				"runtimes": []map[string]string{
					{"name": "native second", "type": "native", "status": "online", "profile_id": tc.profile},
				},
			})
			r.Header.Set("X-Client-Capabilities", tc.capability)
			res := testutil.Call(t, testHandler.DaemonRegister, r).Want(tc.want)
			var count int
			if err := testPool.QueryRow(r.Context(), "SELECT count(*) FROM agent_runtime WHERE workspace_id=$1 AND daemon_id=$2", testWorkspaceID, machine).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if tc.want != http.StatusOK {
				if count != 0 {
					t.Fatalf("rejected registration partially wrote %d rows", count)
				}
				if tc.want == http.StatusUpgradeRequired {
					var out struct {
						Code    string `json:"code"`
						Minimum int    `json:"minimum_protocol_version"`
					}
					res.JSON(&out)
					if out.Code != "native_runtime_protocol_required" || out.Minimum != 1 {
						t.Fatalf("missing upgrade guidance: %+v", out)
					}
				}
				return
			}
			var out daemon.RegisterResponse
			res.JSON(&out)
			if count != 1 || len(out.Runtimes) != 1 || !out.NativeRuntimeProtocol.Supports(1) || !slices.Contains(out.ServerCapabilities, protocol.DaemonCapabilityNativeRuntimeV1) || out.ReposVersion == "" {
				t.Fatalf("registration lost protocol or existing response fields: %+v rows=%d", out, count)
			}
			var metadata []byte
			if err := testPool.QueryRow(r.Context(), "SELECT metadata FROM agent_runtime WHERE workspace_id=$1 AND daemon_id=$2 AND provider='native'", testWorkspaceID, machine).Scan(&metadata); err != nil {
				t.Fatal(err)
			}
			if !runtimeHasCapability(metadata, protocol.DaemonCapabilityNativeRuntimeV1) {
				t.Fatal("registration did not persist native capability")
			}
		})
	}
}

func TestNativeProtocolClaimsRejectBeforeMutation(t *testing.T) {
	for _, mode := range []string{"single", "batch", "websocket"} {
		for _, tc := range []struct {
			name, header, metadata string
			want                   int
		}{
			{"downgraded binary", "", `{"capabilities":["native-runtime-v1"]}`, http.StatusUpgradeRequired},
			{"stale registration", protocol.DaemonCapabilityNativeRuntimeV1, `{}`, http.StatusUpgradeRequired},
			{"malformed registration", protocol.DaemonCapabilityNativeRuntimeV1, `{"capabilities":"native-runtime-v1"}`, http.StatusUpgradeRequired},
			{"future capability only", "native-runtime-v2", `{"capabilities":["native-runtime-v1"]}`, http.StatusUpgradeRequired},
			{"current", protocol.DaemonCapabilityNativeRuntimeV1, `{"capabilities":["native-runtime-v1"]}`, http.StatusOK},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				machine := "native-protocol-claim"
				runtimeID := dbfx.Runtime(t, tc.name, testutil.Cols{"provider": "native", "daemon_id": machine, "metadata": tc.metadata})
				agentID := dbfx.Agent(t, tc.name, runtimeID)
				issueID := dbfx.Issue(t, tc.name)
				taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID})
				dbfx.Cleanup(t, "DELETE FROM task_token WHERE task_id=$1", taskID)
				before, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(taskID))
				if err != nil {
					t.Fatal(err)
				}
				body := map[string]any{"daemon_id": machine, "runtime_ids": []string{runtimeID}, "max_tasks": 1}
				if mode == "websocket" {
					raw, _ := json.Marshal(body)
					status, out, err := testHandler.DaemonRPCHandler(context.Background(), daemonws.ClientIdentity{DaemonID: machine, WorkspaceID: testWorkspaceID, RuntimeIDs: []string{runtimeID}, Capabilities: tc.header, ClientVersion: "9999.0.0"}, "tasks.claim", raw)
					if err != nil || status != tc.want {
						t.Fatalf("WS status=%d body=%s error=%v", status, out, err)
					}
				} else {
					r := newDaemonTokenRequest(http.MethodPost, "/api/daemon/tasks/claim", body, testWorkspaceID, machine)
					r.Header.Set("X-Client-Capabilities", tc.header)
					r.Header.Set("X-Client-Version", "9999.0.0")
					call := testHandler.ClaimTasksByRuntime
					if mode == "single" {
						r = testutil.WithURLParams(r, "runtimeId", runtimeID)
						call = testHandler.ClaimTaskByRuntime
					}
					testutil.Call(t, call, r).Want(tc.want)
				}
				after, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(taskID))
				if err != nil {
					t.Fatal(err)
				}
				var tokens int
				if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM task_token WHERE task_id=$1", taskID).Scan(&tokens); err != nil {
					t.Fatal(err)
				}
				if tc.want == http.StatusOK {
					if after.Status != "dispatched" || !after.DispatchedAt.Valid || tokens != 1 {
						t.Fatalf("current daemon not dispatched: status=%s tokens=%d", after.Status, tokens)
					}
				} else if after.Status != before.Status || after.Attempt != before.Attempt || after.DispatchedAt != before.DispatchedAt || tokens != 0 {
					t.Fatalf("rejected claim mutated task: before=%s/%d after=%s/%d tokens=%d", before.Status, before.Attempt, after.Status, after.Attempt, tokens)
				}
			})
		}
	}
}

func TestNativeProtocolMixedBatchSkipsRetiredRuntime(t *testing.T) {
	machine := "native-mixed-protocol"
	var runtimeIDs, taskIDs []string
	for _, provider := range []string{"claude", "native"} {
		rid := dbfx.Runtime(t, provider, testutil.Cols{"provider": provider, "daemon_id": machine})
		aid := dbfx.Agent(t, provider, rid)
		iid := dbfx.Issue(t, provider)
		tid := dbfx.Task(t, aid, testutil.Cols{"runtime_id": rid, "issue_id": iid})
		dbfx.Cleanup(t, "DELETE FROM task_token WHERE task_id=$1", tid)
		runtimeIDs, taskIDs = append(runtimeIDs, rid), append(taskIDs, tid)
	}
	var out batchClaimResponse
	req := newDaemonTokenRequest(http.MethodPost, "/api/daemon/tasks/claim", map[string]any{"daemon_id": machine, "runtime_ids": runtimeIDs, "max_tasks": 2}, testWorkspaceID, machine)
	testutil.Call(t, testHandler.ClaimTasksByRuntime, req).Want(http.StatusOK).JSON(&out)
	if len(out.Tasks) != 1 || out.Tasks[0].ID != taskIDs[1] {
		t.Fatalf("mixed batch did not isolate native work: %+v", out.Tasks)
	}
	native, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(taskIDs[0]))
	if err != nil {
		t.Fatal(err)
	}
	if native.Status != "queued" || native.DispatchedAt.Valid {
		t.Fatal("retired runtime queue was disturbed")
	}
}

func TestNativeProtocolStartRequiresCurrentDaemonAndClaim(t *testing.T) {
	for _, tc := range []struct {
		name, header, metadata, machine string
		legacy                          bool
		want                            int
	}{
		{"downgraded", "", `{"capabilities":["native-runtime-v1"]}`, "native-start", false, http.StatusUpgradeRequired},
		{"stale registration", protocol.DaemonCapabilityNativeRuntimeV1, `{}`, "native-start", false, http.StatusUpgradeRequired},
		{"unfenced start", protocol.DaemonCapabilityNativeRuntimeV1, `{"capabilities":["native-runtime-v1"]}`, "native-start", true, http.StatusBadRequest},
		{"peer machine", protocol.DaemonCapabilityNativeRuntimeV1, `{"capabilities":["native-runtime-v1"]}`, "peer-machine", false, http.StatusForbidden},
		{"current", protocol.DaemonCapabilityNativeRuntimeV1, `{"capabilities":["native-runtime-v1"]}`, "native-start", false, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rid := dbfx.Runtime(t, tc.name, testutil.Cols{"provider": "native", "daemon_id": "native-start", "metadata": tc.metadata})
			aid := dbfx.Agent(t, tc.name, rid)
			iid := dbfx.Issue(t, tc.name)
			dispatched := time.Now().UTC().Truncate(time.Microsecond)
			tid := dbfx.Task(t, aid, testutil.Cols{"runtime_id": rid, "issue_id": iid, "status": "dispatched", "dispatched_at": dispatched, "attempt": 1})
			body := map[string]any{}
			if !tc.legacy {
				body["runtime_id"], body["dispatched_at"] = rid, dispatched.Format(time.RFC3339Nano)
			}
			r := testutil.WithURLParams(newDaemonTokenRequest(http.MethodPost, "/api/daemon/tasks/"+tid+"/start", body, testWorkspaceID, tc.machine), "taskId", tid)
			r.Header.Set("X-Client-Capabilities", tc.header)
			testutil.Call(t, testHandler.StartTask, r).Want(tc.want)
			after, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(tid))
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == http.StatusOK {
				if after.Status != "running" {
					t.Fatalf("current daemon start = %s", after.Status)
				}
			} else if after.Status != "dispatched" || after.StartedAt.Valid {
				t.Fatal("rejected start mutated task")
			}
		})
	}
}

func TestNativeAgentRejectsCLIOptions(t *testing.T) {
	rid := dbfx.Runtime(t, "native options", testutil.Cols{"provider": "native"})
	aid := dbfx.Agent(t, "native options", rid)
	for _, field := range []string{"runtime_config", "custom_args", "thinking_level", "service_tier"} {
		t.Run(field, func(t *testing.T) {
			var value any = "fixture-secret"
			if field == "runtime_config" {
				value = map[string]string{"command": "fixture-secret"}
			}
			if field == "custom_args" {
				value = []string{"fixture-secret"}
			}
			for _, update := range []bool{false, true} {
				body := map[string]any{"name": "native rejected", "runtime_id": rid, field: value}
				r, call := newRequest(http.MethodPost, "/api/agents", body), testHandler.CreateAgent
				if update {
					r = testutil.WithURLParams(newRequest(http.MethodPut, "/api/agents/"+aid, body), "id", aid)
					call = testHandler.UpdateAgent
				}
				res := testutil.Call(t, call, r).Want(http.StatusBadRequest)
				if strings.Contains(res.Body.String(), "fixture-secret") || !strings.Contains(res.Body.String(), "native_cli_config_not_supported") {
					t.Fatalf("error leaks input or omits recovery code: %s", res.Body.String())
				}
			}
		})
	}
	stored, err := testHandler.Queries.GetAgent(context.Background(), parseUUID(aid))
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.RuntimeConfig) != "{}" || len(stored.CustomArgs) > 0 && string(stored.CustomArgs) != "[]" {
		t.Fatal("rejected writes changed agent options")
	}
}

func TestNativeAgentRebindRequiresClearingInheritedCLIOptions(t *testing.T) {
	legacy := dbfx.Runtime(t, "legacy options", testutil.Cols{"provider": "codex"})
	native := dbfx.Runtime(t, "native target", testutil.Cols{"provider": "native"})
	aid := dbfx.Agent(t, "rebind options", legacy, testutil.Cols{"runtime_config": `{"command":"legacy"}`, "custom_args": `["--legacy"]`, "thinking_level": "high", "service_tier": "fast", "model": "legacy-model"})
	call := func(body map[string]any) *http.Request {
		return testutil.WithURLParams(newRequest(http.MethodPut, "/api/agents/"+aid, body), "id", aid)
	}
	testutil.Call(t, testHandler.UpdateAgent, call(map[string]any{"runtime_id": native})).Want(http.StatusBadRequest)
	before, err := testHandler.Queries.GetAgent(context.Background(), parseUUID(aid))
	if err != nil {
		t.Fatal(err)
	}
	if uuidToString(before.RuntimeID) != legacy {
		t.Fatal("rejected rebind changed runtime")
	}
	var out AgentResponse
	testutil.Call(t, testHandler.UpdateAgent, call(map[string]any{"runtime_id": native, "runtime_config": map[string]any{}, "custom_args": []string{}, "thinking_level": "", "service_tier": "", "model": ""})).Want(http.StatusOK).JSON(&out)
	after, err := testHandler.Queries.GetAgent(context.Background(), parseUUID(aid))
	if err != nil {
		t.Fatal(err)
	}
	if uuidToString(after.RuntimeID) != native || string(after.RuntimeConfig) != "{}" || string(after.CustomArgs) != "[]" || after.ThinkingLevel.Valid || after.ServiceTier.Valid || after.Model.String != "" {
		t.Fatalf("explicit clears not saved: runtime=%s config=%s args=%s", uuidToString(after.RuntimeID), after.RuntimeConfig, after.CustomArgs)
	}
}

func TestNativeProtocolSingleClaimRejectsPeerMachine(t *testing.T) {
	rid := dbfx.Runtime(t, "native peer", testutil.Cols{"provider": "native", "daemon_id": "native-owner"})
	r := testutil.WithURLParams(newDaemonTokenRequest(http.MethodPost, "/api/daemon/runtimes/"+rid+"/claim", nil, testWorkspaceID, "native-peer"), "runtimeId", rid)
	r.Header.Set("X-Client-Capabilities", protocol.DaemonCapabilityNativeRuntimeV1)
	testutil.Call(t, testHandler.ClaimTaskByRuntime, r).Want(http.StatusForbidden)
}

func TestNativeAgentModelRebindRequiresExplicitClear(t *testing.T) {
	legacy := dbfx.Runtime(t, "model rebind legacy", testutil.Cols{"provider": "claude"})
	native := dbfx.Runtime(t, "model rebind native", testutil.Cols{"provider": "native"})
	aid := dbfx.Agent(t, "model rebind", legacy, testutil.Cols{"model": "claude-sonnet-4-5"})
	call := func(body map[string]any) *http.Request {
		return testutil.WithURLParams(newRequest(http.MethodPut, "/api/agents/"+aid, body), "id", aid)
	}
	testutil.Call(t, testHandler.UpdateAgent, call(map[string]any{"runtime_id": native})).Want(http.StatusBadRequest)
	testutil.Call(t, testHandler.UpdateAgent, call(map[string]any{"runtime_id": native, "model": ""})).Want(http.StatusOK)
}

func TestNativeAgentOptionsShapes(t *testing.T) {
	rid := dbfx.Runtime(t, "native shapes", testutil.Cols{"provider": "native"})
	for _, config := range []any{[]any{}, "", false, 0} {
		testutil.Call(t, testHandler.CreateAgent, newRequest(http.MethodPost, "/api/agents", map[string]any{"name": "invalid native shape", "runtime_id": rid, "runtime_config": config})).Want(http.StatusBadRequest)
	}
	for _, tc := range []struct {
		name   string
		config any
	}{{"null", nil}, {"object", map[string]any{}}} {
		var out AgentResponse
		testutil.Call(t, testHandler.CreateAgent, newRequest(http.MethodPost, "/api/agents", map[string]any{"name": "empty native options " + tc.name, "runtime_id": rid, "runtime_config": tc.config, "custom_args": []string{}, "model": ""})).Want(http.StatusCreated).JSON(&out)
		dbfx.Cleanup(t, "DELETE FROM agent WHERE id=$1", out.ID)
		dbfx.Cleanup(t, "DELETE FROM agent_invocation_target WHERE agent_id=$1", out.ID)
	}
}

func TestNativeProtocolRegisterRejectsPeerToken(t *testing.T) {
	r := newDaemonTokenRequest(http.MethodPost, "/api/daemon/register", map[string]any{
		"workspace_id": testWorkspaceID, "daemon_id": "peer-registration",
		"runtimes": []map[string]string{{"name": "native peer", "type": "native"}},
	}, testWorkspaceID, "registration-owner")
	r.Header.Set("X-Client-Capabilities", protocol.DaemonCapabilityNativeRuntimeV1)
	testutil.Call(t, testHandler.DaemonRegister, r).Want(http.StatusForbidden)
}

func TestRetiredRuntimeCannotClaimOrStart(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "unknown"} {
		t.Run(provider, func(t *testing.T) {
			rid := dbfx.Runtime(t, "retired", testutil.Cols{"provider": provider, "daemon_id": "retired-machine"})
			aid := dbfx.Agent(t, "retired", rid)
			iid := dbfx.Issue(t, "retired execution")
			tid := dbfx.Task(t, aid, testutil.Cols{"runtime_id": rid, "issue_id": iid})
			req := withURLParam(newDaemonTokenRequest(http.MethodPost, "/claim", nil, testWorkspaceID, "retired-machine"), "runtimeId", rid)
			testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusGone)
			dbfx.Exec(t, `UPDATE agent_task_queue SET status='dispatched', dispatched_at=now() WHERE id=$1`, tid)
			req = withURLParam(newDaemonTokenRequest(http.MethodPost, "/start", map[string]any{}, testWorkspaceID, "retired-machine"), "taskId", tid)
			testutil.Call(t, testHandler.StartTask, req).Want(http.StatusGone)
			var status string
			dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id=$1`, tid).Scan(&status)
			if status != "dispatched" {
				t.Fatal("retired start changed task state")
			}
			if got := dbfx.Count(t, `SELECT count(*) FROM task_token WHERE task_id=$1`, tid); got != 0 {
				t.Fatal("retired claim minted a credential")
			}
		})
	}
}

func TestNativeExecutionPermissionsRequireRuntimeOwner(t *testing.T) {
	own := dbfx.Runtime(t, "native own permissions", testutil.Cols{"provider": "native", "owner_id": testUserID})
	otherUser := dbfx.User(t, "native host owner", "native-owner-"+own+"@example.test")
	foreign := dbfx.Runtime(t, "native foreign permissions", testutil.Cols{"provider": "native", "owner_id": otherUser, "visibility": "public"})
	aid := dbfx.Agent(t, "native configured agent", own)
	for _, tc := range []struct {
		runtime string
		want    int
	}{{own, http.StatusOK}, {foreign, http.StatusForbidden}} {
		request := testutil.WithURLParams(newRequest(http.MethodPut, "/api/agents/"+aid, map[string]any{"runtime_id": tc.runtime, "runtime_config": map[string]any{"write_files": true, "shell": true}}), "id", aid)
		testutil.Call(t, testHandler.UpdateAgent, request).Want(tc.want)
	}
	actorRequest := testutil.WithURLParams(newRequest(http.MethodPut, "/api/agents/"+aid, map[string]any{"runtime_config": map[string]any{"shell": true}}), "id", aid)
	actorRequest.Header.Set("X-Actor-Source", "task_token")
	actorRequest.Header.Set("X-Agent-ID", aid)
	testutil.Call(t, testHandler.UpdateAgent, actorRequest).Want(http.StatusForbidden)
	// Revoking powers remains possible without host authority.
	request := testutil.WithURLParams(newRequest(http.MethodPut, "/api/agents/"+aid, map[string]any{"runtime_id": foreign, "runtime_config": map[string]any{}}), "id", aid)
	testutil.Call(t, testHandler.UpdateAgent, request).Want(http.StatusOK)
}
