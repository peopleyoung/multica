package remotemcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestDiscoverPaginatesToolsAndRejectsRepeatedCursors(t *testing.T) {
	for _, repeat := range []bool{false, true} {
		t.Run(fmt.Sprint(repeat), func(t *testing.T) {
			pages := 0
			fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					ID     json.RawMessage
					Method string
					Params struct{ Cursor string }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if req.Method == "notifications/initialized" {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				var result any = map[string]any{"protocolVersion": "2025-03-26"}
				if req.Method == "tools/list" {
					pages++
					if pages > 1 && req.Params.Cursor != "next" {
						t.Error("cursor missing")
					}
					cursor := ""
					if pages == 1 || repeat {
						cursor = "next"
					}
					result = map[string]any{"tools": []any{map[string]any{"name": fmt.Sprintf("tool-%d", pages), "inputSchema": map[string]string{"type": "object"}}}, "nextCursor": cursor}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			}))
			defer fixture.Close()
			t.Setenv(DevOriginsEnv, fixture.URL)
			tools, _, err := Discover(context.Background(), fixture.URL, nil, nil, nil)
			if repeat {
				if err == nil || !strings.Contains(err.Error(), "cursor") {
					t.Fatalf("repeated cursor: %v", err)
				}
			} else if err != nil || len(tools) != 2 {
				t.Fatalf("tools=%v err=%v", tools, err)
			}
			if pages != 2 {
				t.Fatalf("pages=%d", pages)
			}
		})
	}
}

type staticResolver map[string][]netip.Addr

func (r staticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	return r[host], nil
}

func TestValidatePublicHTTPSEndpoint(t *testing.T) {
	resolver := staticResolver{
		"mcp.example.com": {netip.MustParseAddr("8.8.8.8")},
		"private.example": {netip.MustParseAddr("169.254.169.254")},
	}
	endpoint, err := ValidatePublicHTTPSEndpoint(context.Background(), "https://mcp.example.com/v1/mcp", []string{"mcp.example.com"}, resolver)
	if err != nil || endpoint.Hostname() != "mcp.example.com" {
		t.Fatalf("ValidatePublicHTTPSEndpoint = %v, %v", endpoint, err)
	}
	for _, raw := range []string{
		"http://mcp.example.com/mcp",
		"https://localhost/mcp",
		"https://token@mcp.example.com/mcp",
		"https://private.example/mcp",
	} {
		if _, err := ValidatePublicHTTPSEndpoint(context.Background(), raw, nil, resolver); err == nil {
			t.Fatalf("endpoint %q was accepted", raw)
		}
	}
	if _, err := ValidatePublicHTTPSEndpoint(context.Background(), "https://mcp.example.com/mcp", []string{"other.example.com"}, resolver); err == nil || !strings.Contains(err.Error(), "policy") {
		t.Fatalf("host policy error = %v", err)
	}
}

func TestHostAllowedWildcardDoesNotMatchApex(t *testing.T) {
	if !hostAllowed("tools.example.com", []string{"*.example.com"}) {
		t.Fatal("wildcard did not match subdomain")
	}
	if hostAllowed("example.com", []string{"*.example.com"}) {
		t.Fatal("wildcard matched apex")
	}
}

func TestContainsProtocolVersion(t *testing.T) {
	if !containsString([]string{"2025-03-26", "2024-11-05"}, "2024-11-05") {
		t.Fatal("declared protocol version was not found")
	}
	if containsString([]string{"2024-11-05"}, "2025-03-26") {
		t.Fatal("undeclared protocol version was accepted")
	}
}

func TestToolSetDigestIgnoresInputSchemaObjectKeyOrder(t *testing.T) {
	firstSchema := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"count":{"type":"number"}}}`)
	secondSchema := json.RawMessage(`{"properties":{"count":{"type":"number"},"query":{"type":"string"}},"type":"object"}`)
	canonical, err := canonicalJSON(firstSchema)
	if err != nil {
		t.Fatal(err)
	}
	schemaDigest := DigestBytes(canonical)

	first, err := ToolSetDigest([]Tool{{
		Name: "search", InputSchema: firstSchema, SchemaDigest: schemaDigest, Risk: "read",
	}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ToolSetDigest([]Tool{{
		Name: "search", InputSchema: secondSchema, SchemaDigest: schemaDigest, Risk: "read",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("digest depends on input schema object key order: %s != %s", first, second)
	}
}
