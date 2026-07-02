package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RoninForge/akashi/internal/registry"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newTestMCPServer starts a real go-sdk MCP server over HTTP with the given
// tool names, so the tools/list probe can be exercised end-to-end offline.
func newTestMCPServer(t *testing.T, toolNames ...string) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	for _, name := range toolNames {
		mcp.AddTool(srv, &mcp.Tool{Name: name, Description: "test tool"},
			func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
				return &mcp.CallToolResult{}, struct{}{}, nil
			})
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

func TestMcpToolsListIntegration(t *testing.T) {
	ts := newTestMCPServer(t, "echo", "ping", "search")

	e := NewEngine()
	got := e.mcpToolsList(context.Background(), ts.URL)

	if got.status != "ok" {
		t.Fatalf("status = %q, want ok", got.status)
	}
	if got.count != 3 {
		t.Errorf("count = %d, want 3", got.count)
	}
	if len(got.names) != 3 {
		t.Errorf("names = %v, want 3 names", got.names)
	}
}

func TestMcpToolsListEmpty(t *testing.T) {
	ts := newTestMCPServer(t) // no tools

	e := NewEngine()
	got := e.mcpToolsList(context.Background(), ts.URL)
	if got.status != "empty" {
		t.Errorf("status = %q, want empty", got.status)
	}
}

func TestMcpToolsListConnectFailed(t *testing.T) {
	// A plain HTTP server that is not an MCP server: connect must fail cleanly.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("hello"))
	}))
	defer ts.Close()

	e := NewEngine()
	got := e.mcpToolsList(context.Background(), ts.URL)
	if got.status != "connect_failed" {
		t.Errorf("status = %q, want connect_failed", got.status)
	}
}

func TestCheckRemoteFullMCPSession(t *testing.T) {
	// End-to-end: the raw initialize probe confirms conformance, then the
	// go-sdk tools/list probe lists tools, all against a real MCP server.
	ts := newTestMCPServer(t, "alpha", "beta")

	e := NewEngine()
	got := e.checkRemote(context.Background(), registry.Remote{URL: ts.URL})

	if got.Status != "reachable" || got.Conformance != "initialize_ok" {
		t.Fatalf("got %+v, want reachable/initialize_ok", got)
	}
	if got.ToolsStatus != "ok" || got.ToolCount != 2 {
		t.Errorf("tools = %q/%d, want ok/2", got.ToolsStatus, got.ToolCount)
	}
}

// --- server.json validation ---

const testSchema = `{
	"$schema": "http://json-schema.org/draft-07/schema#",
	"$id": "https://schema.test/server.schema.json",
	"type": "object",
	"required": ["name", "version"],
	"properties": {
		"name": { "type": "string" },
		"version": { "type": "string" }
	}
}`

// schemaEngine returns an engine whose HTTP client serves testSchema for the
// schema URL, so validateServerJSON runs fully offline.
func schemaEngine() *Engine {
	e := NewEngine()
	e.HTTP = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == "https://schema.test/server.schema.json" {
			return resp(200, testSchema), nil
		}
		return resp(404, ""), nil
	})}
	return e
}

func TestValidateServerJSONValid(t *testing.T) {
	e := schemaEngine()
	raw := []byte(`{"$schema":"https://schema.test/server.schema.json","name":"io.x/y","version":"1.0.0"}`)
	got := e.validateServerJSON(context.Background(), raw)
	if got.Status != "valid" {
		t.Errorf("status = %q (%v), want valid", got.Status, got.Errors)
	}
}

func TestValidateServerJSONInvalid(t *testing.T) {
	e := schemaEngine()
	// version is a number, not a string, and required "name" is missing.
	raw := []byte(`{"$schema":"https://schema.test/server.schema.json","version":123}`)
	got := e.validateServerJSON(context.Background(), raw)
	if got.Status != "invalid" {
		t.Fatalf("status = %q, want invalid", got.Status)
	}
	if len(got.Errors) == 0 {
		t.Error("expected validation error detail")
	}
}

func TestValidateServerJSONNoSchema(t *testing.T) {
	e := schemaEngine()
	raw := []byte(`{"name":"io.x/y","version":"1.0.0"}`)
	got := e.validateServerJSON(context.Background(), raw)
	if got.Status != "no_schema" {
		t.Errorf("status = %q, want no_schema", got.Status)
	}
}

func TestValidateServerJSONSchemaCached(t *testing.T) {
	// Two validations should fetch the schema only once.
	fetches := 0
	e := NewEngine()
	e.HTTP = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == "https://schema.test/server.schema.json" {
			fetches++
			return resp(200, testSchema), nil
		}
		return resp(404, ""), nil
	})}
	raw := []byte(`{"$schema":"https://schema.test/server.schema.json","name":"a","version":"1"}`)
	_ = e.validateServerJSON(context.Background(), raw)
	_ = e.validateServerJSON(context.Background(), raw)
	if fetches != 1 {
		t.Errorf("schema fetched %d times, want 1 (cache)", fetches)
	}
}
