package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/mcpclient"
	"github.com/multica-ai/multica/server/pkg/llm"
	"github.com/multica-ai/multica/server/pkg/remotemcp"
	"github.com/multica-ai/multica/server/pkg/remotemcp/remotemcptest"
)

func TestNativeMCPUsesApprovedRemoteBrokerAndPluginHooks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fixture := remotemcptest.NewServer()
	defer fixture.Close()
	t.Setenv(remotemcp.DevOriginsEnv, fixture.URL)
	discovered, _, err := remotemcp.Discover(ctx, fixture.URL, nil, nil, http.Header{"Authorization": []string{"Bearer " + remotemcptest.Credential}})
	if err != nil {
		t.Fatal(err)
	}
	var approved []remotemcp.Tool
	for _, tool := range discovered {
		if tool.Name == "fixture.write" {
			approved = append(approved, tool)
		}
	}
	config, diagnostics, brokers, err := startTaskRemoteMCPBrokers(ctx, ctx, "native-task", "native", []remotemcp.Connection{{ContributionKey: "fixture", ContributionID: "fixture-contribution", Endpoint: fixture.URL, ApprovedTools: approved, CredentialHeader: "Authorization"}}, func(context.Context, string) (http.Header, error) {
		return http.Header{"Authorization": []string{"Bearer " + remotemcptest.Credential}}, nil
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer brokers.Close()
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	pluginCalls := 0
	pluginConfig, pluginServer, err := startTaskPluginHookMCP(ctx, "native-task", []PluginHookTool{{Name: "plugin_echo", InstallationID: "installation", HookKey: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}, func(_ context.Context, task, installation, hook string, _ json.RawMessage) (json.RawMessage, error) {
		if task != "native-task" || installation != "installation" || hook != "echo" {
			t.Error("plugin attribution lost")
		}
		pluginCalls++
		return json.RawMessage(`{"echo":"native"}`), nil
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer pluginServer.Close()
	config, err = mergeTaskRemoteMCPConfig(config, pluginConfig)
	if err != nil {
		t.Fatal(err)
	}
	host, err := mcpclient.Open(ctx, config, mcpclient.Options{TrustedLocalEndpoints: nativeTrustedBrokerEndpoints(config)})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	defs := host.Definitions()
	if len(defs) != 2 {
		t.Fatalf("unapproved tool exposed: %+v", defs)
	}
	for _, tool := range defs {
		args := json.RawMessage(`{}`)
		if strings.Contains(tool.Description, "fixture.write") {
			args = json.RawMessage(`{"value":"native-write"}`)
		}
		if _, err := host.Execute(ctx, llm.ToolCall{Name: tool.Name, Arguments: args}); err != nil {
			t.Fatal(err)
		}
	}
	if writes := fixture.Writes(); len(writes) != 1 || writes[0] != "native-write" || pluginCalls != 1 {
		t.Fatalf("writes=%v pluginCalls=%d", writes, pluginCalls)
	}
	for _, tool := range defs {
		if strings.Contains(tool.Description, remotemcptest.Credential) {
			t.Fatal("remote credential reached model")
		}
	}
}
