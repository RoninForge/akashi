package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveGitHubURL(t *testing.T) {
	c := NewClient()
	got, err := c.Resolve(context.Background(), "https://github.com/owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != "repo" {
		t.Errorf("origin = %q, want repo", got.Origin)
	}
	if got.Repository == nil || got.Repository.URL != "https://github.com/owner/repo" {
		t.Errorf("repository = %+v", got.Repository)
	}
	if got.Name != "repo:owner/repo" {
		t.Errorf("name = %q, want repo:owner/repo", got.Name)
	}
}

func TestResolveRemoteURL(t *testing.T) {
	c := NewClient()
	got, err := c.Resolve(context.Background(), "https://mcp.example.com/sse")
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != "remote" {
		t.Errorf("origin = %q, want remote", got.Origin)
	}
	if len(got.Remotes) != 1 || got.Remotes[0].URL != "https://mcp.example.com/sse" {
		t.Errorf("remotes = %+v", got.Remotes)
	}
}

func TestResolveEmpty(t *testing.T) {
	c := NewClient()
	if _, err := c.Resolve(context.Background(), "   "); err == nil {
		t.Error("expected an error for an empty target")
	}
}

const sampleList = `{
  "servers": [
    {
      "server": {"name":"io.github.acme/thing","version":"0.9.0","repository":{"url":"https://github.com/acme/thing","source":"github"}},
      "_meta": {"io.modelcontextprotocol.registry/official":{"status":"active","isLatest":false}}
    },
    {
      "server": {"name":"io.github.acme/thing","version":"1.0.0","repository":{"url":"https://github.com/acme/thing","source":"github"},"packages":[{"registryType":"npm","identifier":"thing","transport":{"type":"stdio"}}]},
      "_meta": {"io.modelcontextprotocol.registry/official":{"status":"active","isLatest":true}}
    },
    {
      "server": {"name":"io.github.acme/other"},
      "_meta": {"io.modelcontextprotocol.registry/official":{"status":"active","isLatest":true}}
    }
  ],
  "metadata": {"nextCursor": ""}
}`

func TestByNamePicksLatestExact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleList))
	}))
	defer srv.Close()

	c := NewClient()
	c.BaseURL = srv.URL

	got, err := c.ByName(context.Background(), "io.github.acme/thing")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "1.0.0" {
		t.Errorf("version = %q, want 1.0.0 (the isLatest row)", got.Version)
	}
	if got.RegistryStatus != "active" {
		t.Errorf("status = %q, want active", got.RegistryStatus)
	}
	if got.Origin != "registry" {
		t.Errorf("origin = %q, want registry", got.Origin)
	}
	if len(got.Packages) != 1 || got.Packages[0].RegistryType != "npm" || got.Packages[0].Transport != "stdio" {
		t.Errorf("packages = %+v, want one npm/stdio package", got.Packages)
	}
}

func TestByNameNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleList))
	}))
	defer srv.Close()

	c := NewClient()
	c.BaseURL = srv.URL
	if _, err := c.ByName(context.Background(), "io.github.acme/missing"); err == nil {
		t.Error("expected not-found error")
	}
}
