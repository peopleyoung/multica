package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/multica-ai/multica/server/internal/daemon/tools"
	"github.com/multica-ai/multica/server/pkg/llm"
)

func fixture(t *testing.T, kind string, handler mcp.ToolHandler) json.RawMessage {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, &mcp.ServerOptions{PageSize: 1})
	for _, name := range []string{"fixture.read", "fixture-write"} {
		server.AddTool(&mcp.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`)}, handler)
	}
	get := func(*http.Request) *mcp.Server { return server }
	var transport http.Handler = mcp.NewStreamableHTTPHandler(get, &mcp.StreamableHTTPOptions{JSONResponse: kind == "json"})
	if kind == "sse" {
		transport = mcp.NewSSEHandler(get, nil)
	}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-only" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		transport.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	// The existing exact-origin development policy also permits local fixtures.
	t.Setenv("MULTICA_PLUGIN_DEV_ORIGINS", httpServer.URL)
	kindName := "http"
	if kind == "sse" {
		kindName = "sse"
	}
	raw, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"fixture": map[string]any{"type": kindName, "url": httpServer.URL, "headers": map[string]string{"Authorization": "Bearer fixture-only"}}}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestHTTPTransportsPaginateValidateAndPreserveTypedResults(t *testing.T) {
	for _, kind := range []string{"json", "stream", "sse"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			config := fixture(t, kind, func(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls.Add(1)
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: r.Params.Name}, &mcp.ImageContent{Data: []byte("fixture"), MIMEType: "image/png"}}, StructuredContent: map[string]any{"ok": true}}, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			h, err := Open(ctx, config, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			defs := h.Definitions()
			if len(defs) != 2 || defs[0].Name == defs[1].Name {
				t.Fatalf("pagination or names: %+v", defs)
			}
			for _, args := range []string{`{}`, `{"value":3}`, `{"value":"ok","extra":true}`} {
				if _, err := h.Execute(ctx, llm.ToolCall{Name: defs[0].Name, Arguments: json.RawMessage(args)}); err == nil {
					t.Fatalf("accepted %s", args)
				}
			}
			if calls.Load() != 0 {
				t.Fatal("invalid args reached server")
			}
			out, err := h.Execute(ctx, llm.ToolCall{Name: defs[0].Name, Arguments: json.RawMessage(`{"value":"ok"}`)})
			if err != nil || !strings.Contains(out, `"structuredContent":{"ok":true}`) || !strings.Contains(out, `"type":"image"`) || calls.Load() != 1 {
				t.Fatalf("typed result: %s / %v", out, err)
			}
		})
	}
}

func TestCancellationLeavesUnknownOutcomeWithoutRetry(t *testing.T) {
	entered, exited := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	config := fixture(t, "stream", func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		close(exited)
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := Open(ctx, config, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	callCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := h.Execute(callCtx, llm.ToolCall{Name: h.Definitions()[0].Name, Arguments: json.RawMessage(`{"value":"ok"}`)})
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("call never reached server")
	}
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, tools.ErrOutcomeUnknown) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("call did not cancel")
	}
	select {
	case <-exited:
	case <-ctx.Done():
		t.Fatal("server handler did not cancel")
	}
	if calls.Load() != 1 {
		t.Fatal("MCP call retried")
	}
}

func TestStdioUsesOnlyExplicitCommandAndPermission(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"fixture": serverConfig{Command: executable, Args: []string{"-test.run=^TestMCPFixtureProcess$"}, Env: map[string]string{"MULTICA_MCP_FIXTURE": "1"}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Open(ctx, raw, Options{}); err == nil {
		t.Fatal("stdio ran without explicit permission")
	}
	// Fixture marker is task data; production reserves MULTICA_ configuration.
	raw = []byte(strings.ReplaceAll(string(raw), "MULTICA_MCP_FIXTURE", "MCP_FIXTURE_PROCESS"))
	h, err := Open(ctx, raw, Options{AllowStdio: true, Env: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if len(h.Definitions()) != 1 {
		t.Fatal("stdio tools missing")
	}
	out, err := h.Execute(ctx, llm.ToolCall{Name: h.Definitions()[0].Name, Arguments: json.RawMessage(`{}`)})
	if err != nil || !strings.Contains(out, "stdio-ok") {
		t.Fatalf("%s: %v", out, err)
	}
	h.Close()
}

// The child is the test binary itself, never a user-installed MCP or AI CLI.
func TestMCPFixtureProcess(t *testing.T) {
	if os.Getenv("MCP_FIXTURE_PROCESS") != "1" {
		return
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "stdio-fixture", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "stdio-ok"}}}, nil
	})
	_ = server.Run(context.Background(), &mcp.StdioTransport{})
	os.Exit(0)
}

func TestDiscoveryRejectsRepeatedCursorAndDuplicateTools(t *testing.T) {
	for _, mode := range []string{"cursor", "duplicate", "external-ref", "count"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					ID     json.RawMessage
					Method string
					Params struct{ Cursor string }
				}
				if json.NewDecoder(r.Body).Decode(&req) != nil {
					t.Error("bad request")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if req.Method == "notifications/initialized" {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				var result any = map[string]any{"protocolVersion": "2025-03-26", "serverInfo": map[string]string{"name": "fixture", "version": "1"}, "capabilities": map[string]any{"tools": map[string]any{}}}
				if req.Method == "tools/list" {
					name := "read"
					schema := `{"type":"object"}`
					if mode == "cursor" {
						name = req.Params.Cursor + "-read"
					}
					if mode == "external-ref" {
						schema = `{"type":"object","$ref":"https://example.invalid/schema"}`
					}
					result = map[string]any{"tools": []any{map[string]any{"name": name, "inputSchema": json.RawMessage(schema)}}, "nextCursor": "repeat"}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			}))
			defer server.Close()
			config := json.RawMessage(fmt.Sprintf(`{"mcpServers":{"fixture":{"url":%q}}}`, server.URL))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			limit := 128
			if mode == "count" {
				limit = 1
			}
			_, err := Open(ctx, config, Options{TrustedLocalEndpoints: map[string]bool{server.URL: true}, MaxTools: limit})
			if err == nil {
				t.Fatal("invalid discovery accepted")
			}
		})
	}
}

func TestManagedEndpointDoesNotSelfAuthorizeLoopback(t *testing.T) {
	_, err := Open(context.Background(), json.RawMessage(`{"mcpServers":{"multica-plugins":{"url":"http://127.0.0.1:1/private"}}}`), Options{})
	if err == nil || !strings.Contains(err.Error(), "network policy") {
		t.Fatalf("loopback accepted: %v", err)
	}
}

func TestFailedWriteResponseIsNeverRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage
			Method string
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{"protocolVersion": "2025-03-26", "serverInfo": map[string]string{"name": "fixture", "version": "1"}, "capabilities": map[string]any{"tools": map[string]any{}}}
		switch req.Method {
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "write", "inputSchema": map[string]string{"type": "object"}}}}
		case "tools/call":
			calls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	config := json.RawMessage(fmt.Sprintf(`{"mcpServers":{"fixture":{"url":%q}}}`, server.URL))
	h, err := Open(ctx, config, Options{TrustedLocalEndpoints: map[string]bool{server.URL: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	_, err = h.Execute(ctx, llm.ToolCall{Name: h.Definitions()[0].Name, Arguments: json.RawMessage(`{}`)})
	if !errors.Is(err, tools.ErrOutcomeUnknown) || calls.Load() != 1 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
}
