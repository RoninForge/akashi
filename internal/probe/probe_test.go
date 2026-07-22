package probe

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RoninForge/akashi/internal/registry"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func resp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func testEngine(route func(*http.Request) (*http.Response, error)) *Engine {
	e := NewEngine()
	e.HTTP = &http.Client{Transport: rtFunc(route)}
	e.Now = func() time.Time { return time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC) }
	return e
}

func TestProbeServerHealthy(t *testing.T) {
	e := testEngine(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "api.github.com":
			return resp(200, `{"archived":false,"pushed_at":"2026-06-25T00:00:00Z","stargazers_count":9,"license":{"spdx_id":"MIT"}}`), nil
		case "registry.npmjs.org":
			return resp(200, `{"versions":{"1.0.0":{}},"dist-tags":{"latest":"1.0.0"}}`), nil
		}
		t.Fatalf("unexpected host %s", r.URL.Host)
		return nil, nil
	})

	s := registry.Server{
		Name:           "io.github.acme/thing",
		Title:          "Thing",
		Description:    "Does the thing.",
		RegistryStatus: "active",
		Repository:     &registry.Repository{URL: "https://github.com/acme/thing"},
		Packages:       []registry.Package{{RegistryType: "npm", Identifier: "thing"}},
	}
	got := e.ProbeServer(context.Background(), s)

	if got.Verdict != Healthy {
		t.Fatalf("verdict = %q, want healthy (reasons %v)", got.Verdict, got.Reasons)
	}
	if got.CheckedAt != "2026-07-01" {
		t.Errorf("CheckedAt = %q, want 2026-07-01", got.CheckedAt)
	}
	if got.Title != "Thing" || got.Description != "Does the thing." {
		t.Errorf("title/description = %q/%q, want carried through from the registry.Server", got.Title, got.Description)
	}
	if got.Signals.Repo.License != "MIT" {
		t.Errorf("license = %q, want MIT", got.Signals.Repo.License)
	}
	if got.AliveEntrypoints != 2 {
		t.Errorf("alive entrypoints = %d, want 2", got.AliveEntrypoints)
	}
}

func TestCheckRepoMissing(t *testing.T) {
	e := testEngine(func(_ *http.Request) (*http.Response, error) {
		return resp(404, ``), nil
	})
	got := e.checkRepo(context.Background(), &registry.Repository{URL: "https://github.com/gone/repo"})
	if got.Status != "missing" || got.HTTPStatus != 404 {
		t.Errorf("got %+v, want missing/404", got)
	}
}

func TestCheckRepoNonGitHubIsUnprobed(t *testing.T) {
	e := testEngine(func(_ *http.Request) (*http.Response, error) {
		t.Fatal("should not make a request for a non-GitHub host")
		return nil, nil
	})
	got := e.checkRepo(context.Background(), &registry.Repository{URL: "https://gitlab.com/x/y"})
	if got.Status != "unprobed" {
		t.Errorf("status = %q, want unprobed", got.Status)
	}
}

func TestCheckRemoteInitializeOK(t *testing.T) {
	// A conformant initialize response proves alive + conformant in one probe;
	// no GET should be needed.
	getSeen := false
	e := testEngine(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			getSeen = true
		}
		return resp(200, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"x"}}}`), nil
	})
	got := e.checkRemote(context.Background(), registry.Remote{URL: "https://mcp.example.com/mcp"})
	if got.Status != "reachable" || got.Conformance != "initialize_ok" {
		t.Fatalf("got %+v, want reachable/initialize_ok", got)
	}
	if got.IDEchoed == nil || !*got.IDEchoed {
		t.Errorf("IDEchoed = %v, want true", got.IDEchoed)
	}
	if getSeen {
		t.Error("a GET was made even though initialize already succeeded")
	}
}

func TestCheckRemoteInitializeEvidence(t *testing.T) {
	// A conformant initialize also records the readiness observables: the
	// negotiated protocol version, the sorted capability keys, and whether an
	// Mcp-Session-Id header was issued.
	e := testEngine(func(_ *http.Request) (*http.Response, error) {
		r := resp(200, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{"listChanged":true},"logging":{}},"serverInfo":{"name":"x"}}}`)
		r.Header.Set("Mcp-Session-Id", "abc123")
		return r, nil
	})
	got := e.checkRemote(context.Background(), registry.Remote{URL: "https://mcp.example.com/mcp"})
	if got.ProtocolVersion != "2025-06-18" {
		t.Errorf("ProtocolVersion = %q, want 2025-06-18", got.ProtocolVersion)
	}
	if len(got.Capabilities) != 2 || got.Capabilities[0] != "logging" || got.Capabilities[1] != "tools" {
		t.Errorf("Capabilities = %v, want [logging tools]", got.Capabilities)
	}
	if got.SessionIssued == nil || !*got.SessionIssued {
		t.Errorf("SessionIssued = %v, want true", got.SessionIssued)
	}
}

func TestCheckRemoteInitializeEvidenceSSE(t *testing.T) {
	// The same evidence must survive SSE framing, and a server that issues no
	// session id records SessionIssued=false (stateless), not nil.
	e := testEngine(func(_ *http.Request) (*http.Response, error) {
		body := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":\"2026-07-28\",\"capabilities\":{\"tools\":{}},\"serverInfo\":{\"name\":\"x\"}}}\n\n"
		return resp(200, body), nil
	})
	got := e.checkRemote(context.Background(), registry.Remote{URL: "https://mcp.example.com/mcp"})
	if got.ProtocolVersion != "2026-07-28" {
		t.Errorf("ProtocolVersion = %q, want 2026-07-28", got.ProtocolVersion)
	}
	if len(got.Capabilities) != 1 || got.Capabilities[0] != "tools" {
		t.Errorf("Capabilities = %v, want [tools]", got.Capabilities)
	}
	if got.SessionIssued == nil || *got.SessionIssued {
		t.Errorf("SessionIssued = %v, want false", got.SessionIssued)
	}
}

func TestCheckRemoteAuthGated(t *testing.T) {
	// A 401 to initialize means alive but auth-gated: reachable, not broken.
	e := testEngine(func(_ *http.Request) (*http.Response, error) {
		return resp(401, "unauthorized"), nil
	})
	got := e.checkRemote(context.Background(), registry.Remote{URL: "https://mcp.example.com/mcp"})
	if got.Status != "reachable" || got.Conformance != "auth_gated" {
		t.Errorf("got %+v, want reachable/auth_gated", got)
	}
}

func TestCheckRemoteImpostor(t *testing.T) {
	// A 200 that is not a JSON-RPC MCP response is a conformance failure.
	e := testEngine(func(_ *http.Request) (*http.Response, error) {
		return resp(200, "<html><body>hello</body></html>"), nil
	})
	got := e.checkRemote(context.Background(), registry.Remote{URL: "https://not-mcp.example.com/"})
	if got.Status != "reachable" || got.Conformance != "reachable_nonconformant" {
		t.Errorf("got %+v, want reachable/reachable_nonconformant", got)
	}
}

func TestCheckRemoteTransportUnverified(t *testing.T) {
	// A 405 to our POST means the endpoint is up but our transport did not fit
	// (e.g. an SSE/GET-only server). Alive, conformance unverified, no downgrade.
	e := testEngine(func(_ *http.Request) (*http.Response, error) {
		return resp(405, "method not allowed"), nil
	})
	got := e.checkRemote(context.Background(), registry.Remote{URL: "https://sse.example.com/rpc"})
	if got.Status != "reachable" || got.Conformance != "unverified" {
		t.Errorf("got %+v, want reachable/unverified", got)
	}
}

func TestCheckRemoteSSEFallbackToGet(t *testing.T) {
	// initialize POST 404s (SSE server, wrong URL for POST) but a GET succeeds:
	// the endpoint is alive, conformance not evaluated.
	e := testEngine(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return resp(200, "event: ping\n"), nil
		}
		return resp(404, "not found"), nil
	})
	got := e.checkRemote(context.Background(), registry.Remote{URL: "https://sse.example.com/sse"})
	if got.Status != "reachable" || got.Probe != "get" {
		t.Fatalf("got %+v, want reachable via get", got)
	}
	if got.Conformance != "" {
		t.Errorf("conformance = %q, want empty (not evaluated)", got.Conformance)
	}
}

func TestCheckRemoteDead(t *testing.T) {
	// initialize fails (connection error) and the GET fallback also 404s -> down.
	e := testEngine(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return resp(404, ""), nil
		}
		return nil, io.ErrUnexpectedEOF
	})
	got := e.checkRemote(context.Background(), registry.Remote{URL: "https://gone.example.com/mcp"})
	if got.Status != "not_found" {
		t.Errorf("got %+v, want not_found", got)
	}
}

func TestParseOCI(t *testing.T) {
	cases := map[string][2]string{
		"docker.io/acme/server:1.2": {"acme/server", "1.2"},
		"acme/server":               {"acme/server", "latest"},
		"redis":                     {"library/redis", "latest"},
		"ghcr.io/acme/server":       {"", ""}, // non-Docker-Hub, three segments
	}
	for in, want := range cases {
		repo, tag := parseOCI(in)
		if repo != want[0] || tag != want[1] {
			t.Errorf("parseOCI(%q) = (%q,%q), want (%q,%q)", in, repo, tag, want[0], want[1])
		}
	}
}
