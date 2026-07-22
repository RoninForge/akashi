package probe

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/RoninForge/akashi/internal/registry"
)

// serverWithRemote is a minimal registry server carrying one remote, enough
// for the readiness pass to have a probe target.
func serverWithRemote(url, typ string) registry.Server {
	return registry.Server{
		Name:           "example/server",
		RegistryStatus: "active",
		Origin:         "registry",
		Remotes:        []registry.Remote{{URL: url, Type: typ}},
	}
}

// readinessRoute builds a route function that answers per JSON-RPC method,
// reading the method from the request body. Paths outside the MCP endpoint
// (the well-known metadata fetch) 404 unless wellKnown is set.
func readinessRoute(t *testing.T, handler func(method string, r *http.Request) *http.Response, wellKnown string) func(*http.Request) (*http.Response, error) {
	t.Helper()
	return func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, ".well-known") {
			if wellKnown != "" {
				return resp(200, wellKnown), nil
			}
			return resp(404, "not found"), nil
		}
		if r.Method == http.MethodGet {
			return resp(405, "method not allowed"), nil
		}
		var body []byte
		if r.Body != nil {
			b := make([]byte, 4096)
			n, _ := r.Body.Read(b)
			body = b[:n]
		}
		method := ""
		for _, m := range []string{"initialize", "tools/list", "server/discover", "subscriptions/listen", "resources/read"} {
			if strings.Contains(string(body), `"method":"`+m+`"`) {
				method = m
				break
			}
		}
		return handler(method, r), nil
	}
}

func TestReadinessNeedsMigration(t *testing.T) {
	// The expected July 2026 profile: answers initialize, negotiates an old
	// version, accepts stateless tools/list, but has no server/discover and
	// ignores routing headers.
	e := testEngine(readinessRoute(t, func(method string, _ *http.Request) *http.Response {
		switch method {
		case "initialize":
			return resp(200, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"x"}}}`)
		case "tools/list":
			return resp(200, `{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`)
		default:
			return resp(200, `{"jsonrpc":"2.0","id":3,"error":{"code":-32601,"message":"method not found"}}`)
		}
	}, ""))
	e.ProbeTools = false
	got := e.ProbeServer(context.Background(), serverWithRemote("https://mcp.example.com/mcp", "streamable-http"))
	r := got.Readiness
	if r == nil {
		t.Fatal("Readiness = nil, want a signal")
	}
	if r.Verdict != ReadinessNeedsMigration {
		t.Fatalf("Verdict = %q (%v), want needs-migration", r.Verdict, r.Reasons)
	}
	if !r.StatelessAccepted || r.DiscoverSupported || r.SessionIssued || r.SubscriptionsListen {
		t.Errorf("evidence = %+v, want stateless only", r)
	}
	if r.HeaderEnforcement != "ignored" {
		t.Errorf("HeaderEnforcement = %q, want ignored", r.HeaderEnforcement)
	}
	if r.RulesetVersion != RulesetVersion {
		t.Errorf("RulesetVersion = %q, want %q", r.RulesetVersion, RulesetVersion)
	}
}

func TestReadinessReady(t *testing.T) {
	// A conformant 2026-07-28 server: no session, stateless accepted,
	// server/discover implemented, header mismatch rejected with the final
	// error code, cache metadata and resultType present.
	e := testEngine(readinessRoute(t, func(method string, r *http.Request) *http.Response {
		switch method {
		case "initialize":
			return resp(200, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2026-07-28","capabilities":{"tools":{}},"serverInfo":{"name":"x"}}}`)
		case "tools/list":
			if r.Header.Get("Mcp-Method") != "" && r.Header.Get("Mcp-Method") != "tools/list" {
				return resp(200, `{"jsonrpc":"2.0","id":4,"error":{"code":-32020,"message":"header mismatch"}}`)
			}
			return resp(200, `{"jsonrpc":"2.0","id":2,"result":{"tools":[],"resultType":"complete","ttlMs":60000,"cacheScope":"public"}}`)
		case "server/discover":
			return resp(200, `{"jsonrpc":"2.0","id":3,"result":{"protocolVersions":["2026-07-28","2025-11-25"]}}`)
		case "subscriptions/listen":
			return resp(200, `{"jsonrpc":"2.0","id":5,"result":{"resultType":"complete"}}`)
		default:
			return resp(200, `{"jsonrpc":"2.0","id":9,"error":{"code":-32601,"message":"method not found"}}`)
		}
	}, `{"resource":"https://mcp.example.com"}`))
	e.ProbeTools = false
	got := e.ProbeServer(context.Background(), serverWithRemote("https://mcp.example.com/mcp", "streamable-http"))
	r := got.Readiness
	if r == nil {
		t.Fatal("Readiness = nil, want a signal")
	}
	if r.Verdict != ReadinessReady {
		t.Fatalf("Verdict = %q (%v), want ready", r.Verdict, r.Reasons)
	}
	if r.HeaderEnforcement != "enforced-32020" {
		t.Errorf("HeaderEnforcement = %q, want enforced-32020", r.HeaderEnforcement)
	}
	if len(r.DiscoverVersions) != 2 || r.DiscoverVersions[0] != "2025-11-25" {
		t.Errorf("DiscoverVersions = %v, want sorted two versions", r.DiscoverVersions)
	}
	if !r.ResultTypePresent || !r.CacheMetaPresent {
		t.Errorf("conformance fields = %+v, want resultType and cache meta present", r)
	}
	if !r.SubscriptionsListen || !r.ProtectedResourceMetadata {
		t.Errorf("evidence = %+v, want subscriptions/listen and well-known metadata", r)
	}
	if len(r.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", r.Warnings)
	}
}

func TestReadinessAtRiskSessionRequired(t *testing.T) {
	// A stateful server: initialize mints a session, bare tools/list is
	// rejected, tools/list with the session header succeeds. That is hard
	// dependence on the removed session mechanism: at-risk.
	e := testEngine(readinessRoute(t, func(method string, r *http.Request) *http.Response {
		switch method {
		case "initialize":
			rs := resp(200, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"x"}}}`)
			rs.Header.Set("Mcp-Session-Id", "s-1")
			return rs
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") == "s-1" {
				return resp(200, `{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`)
			}
			return resp(400, `{"jsonrpc":"2.0","id":2,"error":{"code":-32600,"message":"no session"}}`)
		default:
			return resp(200, `{"jsonrpc":"2.0","id":3,"error":{"code":-32601,"message":"method not found"}}`)
		}
	}, ""))
	e.ProbeTools = false
	got := e.ProbeServer(context.Background(), serverWithRemote("https://mcp.example.com/mcp", "streamable-http"))
	r := got.Readiness
	if r == nil {
		t.Fatal("Readiness = nil, want a signal")
	}
	if r.Verdict != ReadinessAtRisk {
		t.Fatalf("Verdict = %q (%v), want at-risk", r.Verdict, r.Reasons)
	}
	if !r.SessionIssued || !r.SessionRequired || r.StatelessAccepted {
		t.Errorf("session evidence = %+v, want issued and required", r)
	}
}

func TestReadinessAtRiskDeclaredSSE(t *testing.T) {
	// A remote the registry declares as deprecated HTTP+SSE transport is
	// at-risk regardless of anything else it does.
	e := testEngine(readinessRoute(t, func(method string, _ *http.Request) *http.Response {
		switch method {
		case "initialize":
			return resp(200, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"x"}}}`)
		case "tools/list":
			return resp(200, `{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`)
		default:
			return resp(200, `{"jsonrpc":"2.0","id":3,"error":{"code":-32601,"message":"method not found"}}`)
		}
	}, ""))
	e.ProbeTools = false
	got := e.ProbeServer(context.Background(), serverWithRemote("https://sse.example.com/sse", "sse"))
	r := got.Readiness
	if r == nil {
		t.Fatal("Readiness = nil, want a signal")
	}
	if r.Verdict != ReadinessAtRisk {
		t.Fatalf("Verdict = %q (%v), want at-risk", r.Verdict, r.Reasons)
	}
	if r.DeclaredTransport != "sse" {
		t.Errorf("DeclaredTransport = %q, want sse", r.DeclaredTransport)
	}
}

func TestReadinessUnknownWhenUnreachable(t *testing.T) {
	// No conformant remote means no readiness signal at all: the unknown
	// bucket records absence, it does not guess.
	e := testEngine(func(_ *http.Request) (*http.Response, error) {
		return resp(401, "unauthorized"), nil
	})
	e.ProbeTools = false
	got := e.ProbeServer(context.Background(), serverWithRemote("https://mcp.example.com/mcp", "streamable-http"))
	if got.Readiness != nil {
		t.Fatalf("Readiness = %+v, want nil for an auth-gated server", got.Readiness)
	}
}
