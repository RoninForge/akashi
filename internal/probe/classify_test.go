package probe

import (
	"slices"
	"testing"

	"github.com/RoninForge/akashi/internal/registry"
)

func TestClassifyVerdicts(t *testing.T) {
	alivePush := 10
	stalePush := 800

	tests := []struct {
		name        string
		server      registry.Server
		signals     Signals
		wantVerdict Verdict
		wantReasons []string // subset that must be present
	}{
		{
			name:        "healthy repo plus published npm",
			server:      registry.Server{RegistryStatus: "active"},
			signals:     Signals{Repo: RepoSignal{Status: "alive", AgeDays: &alivePush}, Packages: []PackageSignal{{Type: "npm", Status: "published"}}},
			wantVerdict: Healthy,
		},
		{
			name:        "repo 404 but package still installs is degraded",
			server:      registry.Server{RegistryStatus: "active"},
			signals:     Signals{Repo: RepoSignal{Status: "missing"}, Packages: []PackageSignal{{Type: "npm", Status: "published"}}},
			wantVerdict: Degraded,
			wantReasons: []string{"repo_404"},
		},
		{
			name:        "alive but stale over a year is degraded",
			server:      registry.Server{RegistryStatus: "active"},
			signals:     Signals{Repo: RepoSignal{Status: "alive", AgeDays: &stalePush}},
			wantVerdict: Degraded,
			wantReasons: []string{"repo_stale_1yr"},
		},
		{
			name:        "everything broken is dead",
			server:      registry.Server{RegistryStatus: "active"},
			signals:     Signals{Repo: RepoSignal{Status: "missing"}, Remotes: []RemoteSignal{{Status: "unreachable"}}},
			wantVerdict: Dead,
			wantReasons: []string{"repo_404", "remote_unreachable"},
		},
		{
			name:        "registry deleted overrides live entrypoints",
			server:      registry.Server{RegistryStatus: "deleted"},
			signals:     Signals{Repo: RepoSignal{Status: "alive", AgeDays: &alivePush}},
			wantVerdict: Dead,
		},
		{
			name:        "only unprobeable entrypoint is unknown",
			server:      registry.Server{RegistryStatus: "active"},
			signals:     Signals{Repo: RepoSignal{Status: "none"}, Packages: []PackageSignal{{Type: "oci", Status: "unprobed"}}},
			wantVerdict: Unknown,
		},
		{
			name:        "deprecated but working downgrades to degraded",
			server:      registry.Server{RegistryStatus: "deprecated"},
			signals:     Signals{Repo: RepoSignal{Status: "alive", AgeDays: &alivePush}, Packages: []PackageSignal{{Type: "npm", Status: "published"}}},
			wantVerdict: Degraded,
			wantReasons: []string{"registry_deprecated"},
		},
		{
			name:        "auth-gated remote counts as alive and healthy",
			server:      registry.Server{RegistryStatus: "active"},
			signals:     Signals{Repo: RepoSignal{Status: "none"}, Remotes: []RemoteSignal{{Status: "reachable", Conformance: "auth_gated"}}},
			wantVerdict: Healthy,
		},
		{
			name:        "reachable but not an MCP server is degraded",
			server:      registry.Server{RegistryStatus: "active"},
			signals:     Signals{Repo: RepoSignal{Status: "none"}, Remotes: []RemoteSignal{{Status: "reachable", Conformance: "reachable_nonconformant"}}},
			wantVerdict: Degraded,
			wantReasons: []string{"remote_nonconformant"},
		},
		{
			name:        "unverifiable transport remote stays healthy",
			server:      registry.Server{RegistryStatus: "active"},
			signals:     Signals{Repo: RepoSignal{Status: "none"}, Remotes: []RemoteSignal{{Status: "reachable", Conformance: "unverified"}}},
			wantVerdict: Healthy,
		},
		{
			name:        "invalid server.json is degraded",
			server:      registry.Server{RegistryStatus: "active"},
			signals:     Signals{Repo: RepoSignal{Status: "alive", AgeDays: &alivePush}, ServerJSON: ServerJSONSignal{Status: "invalid"}},
			wantVerdict: Degraded,
			wantReasons: []string{"server_json_invalid"},
		},
		{
			name:        "tools/list failure does not downgrade a working server",
			server:      registry.Server{RegistryStatus: "active"},
			signals:     Signals{Repo: RepoSignal{Status: "none"}, Remotes: []RemoteSignal{{Status: "reachable", Conformance: "initialize_ok", ToolsStatus: "error"}}},
			wantVerdict: Healthy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classify(tt.server, tt.signals)
			if got.Verdict != tt.wantVerdict {
				t.Errorf("verdict = %q, want %q (reasons: %v)", got.Verdict, tt.wantVerdict, got.Reasons)
			}
			for _, r := range tt.wantReasons {
				if !slices.Contains(got.Reasons, r) {
					t.Errorf("missing reason %q in %v", r, got.Reasons)
				}
			}
		})
	}
}

func TestClassifyReasonsAreDeduped(t *testing.T) {
	sig := Signals{
		Repo:    RepoSignal{Status: "missing"},
		Remotes: []RemoteSignal{{Status: "unreachable"}, {Status: "unreachable"}},
	}
	got := classify(registry.Server{RegistryStatus: "active"}, sig)
	count := 0
	for _, r := range got.Reasons {
		if r == "remote_unreachable" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("remote_unreachable appears %d times, want 1 (reasons: %v)", count, got.Reasons)
	}
}

func TestBuildChecksSkipsInapplicable(t *testing.T) {
	// A bare remote-URL target has no registry status and no repo, so those
	// checks must be Skip, not Fail.
	sig := Signals{Repo: RepoSignal{Status: "none"}, Remotes: []RemoteSignal{{Status: "reachable", Conformance: "initialize_ok"}}}
	got := classify(registry.Server{}, sig) // empty RegistryStatus

	byName := map[string]Status{}
	for _, c := range got.Checks {
		byName[c.Name] = c.Status
	}
	if byName["registry status"] != Skip {
		t.Errorf("registry status check = %q, want skip", byName["registry status"])
	}
	if byName["repo reachable"] != Skip {
		t.Errorf("repo reachable check = %q, want skip", byName["repo reachable"])
	}
	if byName["MCP conformance"] != Pass {
		t.Errorf("MCP conformance check = %q, want pass", byName["MCP conformance"])
	}
}
