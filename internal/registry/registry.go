// Package registry resolves a check target into a normalized MCP server
// descriptor. A target may be an official-registry server name
// (for example "io.github.owner/name"), a GitHub repository URL, or a remote
// endpoint URL. Every lookup is keyless: it reads the public MCP registry
// API and touches no user secret.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// DefaultBaseURL is the official, keyless MCP registry servers endpoint.
const DefaultBaseURL = "https://registry.modelcontextprotocol.io/v0/servers"

// DefaultUserAgent identifies akashi politely to the registry.
const DefaultUserAgent = "akashi (mcp health; keyless; +https://roninforge.org)"

// Repository points at a server's source.
type Repository struct {
	URL    string `json:"url"`
	Source string `json:"source,omitempty"`
}

// Package is one installable entrypoint (npm, pypi, oci, ...).
type Package struct {
	RegistryType string `json:"registryType"`
	Identifier   string `json:"identifier"`
	Version      string `json:"version,omitempty"`
	Transport    string `json:"transport,omitempty"`
}

// Remote is one hosted endpoint a server exposes.
type Remote struct {
	Type string `json:"type,omitempty"`
	URL  string `json:"url"`
}

// Server is the normalized descriptor the probe engine consumes. Origin
// records how the target was resolved, which the report uses to decide which
// checks apply (a bare remote URL has no registry status to check, etc.).
type Server struct {
	Name           string      `json:"name"`
	Title          string      `json:"title,omitempty"`
	Version        string      `json:"version,omitempty"`
	Description    string      `json:"description,omitempty"`
	Repository     *Repository `json:"repository,omitempty"`
	Packages       []Package   `json:"packages,omitempty"`
	Remotes        []Remote    `json:"remotes,omitempty"`
	RegistryStatus string      `json:"registryStatus,omitempty"` // active|deprecated|deleted
	Origin         string      `json:"origin"`                   // registry|repo|remote
	// RawServer is the exact server.json object bytes as published, retained so
	// the probe can validate it against its declared JSON Schema. Only set for
	// registry-resolved servers.
	RawServer json.RawMessage `json:"-"`
}

// Client talks to the MCP registry over plain keyless HTTP.
type Client struct {
	HTTP      *http.Client
	BaseURL   string
	UserAgent string
}

// NewClient returns a Client with sane defaults.
func NewClient() *Client {
	return &Client{
		HTTP:      &http.Client{Timeout: 20 * time.Second},
		BaseURL:   DefaultBaseURL,
		UserAgent: DefaultUserAgent,
	}
}

// --- registry wire format ---

type rawOfficialMeta struct {
	Status   string `json:"status"`
	IsLatest bool   `json:"isLatest"`
}

// rawServerObj is the typed view of a server.json object.
type rawServerObj struct {
	Name        string      `json:"name"`
	Title       string      `json:"title"`
	Version     string      `json:"version"`
	Description string      `json:"description"`
	Repository  *Repository `json:"repository"`
	Packages    []struct {
		RegistryType string `json:"registryType"`
		Identifier   string `json:"identifier"`
		Version      string `json:"version"`
		Transport    struct {
			Type string `json:"type"`
		} `json:"transport"`
	} `json:"packages"`
	Remotes []Remote `json:"remotes"`
}

// rawEntry keeps the server object as raw bytes (for schema validation) and
// decodes it on demand.
type rawEntry struct {
	Server json.RawMessage `json:"server"`
	Meta   struct {
		Official rawOfficialMeta `json:"io.modelcontextprotocol.registry/official"`
	} `json:"_meta"`
}

type rawList struct {
	Servers  []rawEntry `json:"servers"`
	Metadata struct {
		NextCursor string `json:"nextCursor"`
	} `json:"metadata"`
}

func (e rawEntry) normalize() Server {
	var obj rawServerObj
	_ = json.Unmarshal(e.Server, &obj) // best-effort; a malformed record yields a mostly-empty Server
	s := Server{
		Name:           obj.Name,
		Title:          obj.Title,
		Version:        obj.Version,
		Description:    obj.Description,
		Repository:     obj.Repository,
		Remotes:        obj.Remotes,
		RegistryStatus: e.Meta.Official.Status,
		Origin:         "registry",
		RawServer:      e.Server,
	}
	for _, p := range obj.Packages {
		s.Packages = append(s.Packages, Package{
			RegistryType: p.RegistryType,
			Identifier:   p.Identifier,
			Version:      p.Version,
			Transport:    p.Transport.Type,
		})
	}
	return s
}

// Resolve turns a raw target string into a Server. It recognizes, in order:
// an http(s) GitHub repository URL, an http(s) remote endpoint URL, and
// otherwise a registry server name looked up via the registry API.
func (c *Client) Resolve(ctx context.Context, target string) (*Server, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, fmt.Errorf("empty target")
	}

	if u, err := url.Parse(target); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		// Exactly github.com, not gist.github.com or any other subdomain: those
		// are not source repositories and would be misprobed as one.
		if strings.ToLower(u.Hostname()) == "github.com" {
			owner, repo := githubParts(u.Path)
			name := target
			if owner != "" {
				name = "repo:" + owner + "/" + repo
			}
			return &Server{
				Name:       name,
				Repository: &Repository{URL: target, Source: "github"},
				Origin:     "repo",
			}, nil
		}
		return &Server{
			Name:    "remote:" + target,
			Remotes: []Remote{{URL: target, Type: remoteTransport(u.Path)}},
			Origin:  "remote",
		}, nil
	}

	return c.ByName(ctx, target)
}

// ByName looks up a single server by its exact registry name and returns the
// latest published version. The registry search is a substring match, so we
// filter to an exact name and isLatest==true, following the result cursor
// (up to maxByNamePages) in case the exact match is not on the first page.
func (c *Client) ByName(ctx context.Context, name string) (*Server, error) {
	const maxByNamePages = 10

	var firstSameName *Server
	var nearby []string
	cursor := ""

	for page := 0; page < maxByNamePages; page++ {
		u, err := url.Parse(c.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("bad registry base URL: %w", err)
		}
		q := u.Query()
		q.Set("search", name)
		q.Set("limit", "100")
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		u.RawQuery = q.Encode()

		list, err := c.getList(ctx, u.String())
		if err != nil {
			return nil, err
		}

		for _, e := range list.Servers {
			s := e.normalize()
			switch {
			case s.Name == name && e.Meta.Official.IsLatest:
				return &s, nil
			case s.Name == name && firstSameName == nil:
				saved := s
				firstSameName = &saved
			}
			nearby = append(nearby, s.Name)
		}

		cursor = list.Metadata.NextCursor
		if cursor == "" {
			break
		}
	}

	// No row was flagged isLatest for this name (a registry-data edge case).
	// Fall back to the first same-name row we encountered.
	if firstSameName != nil {
		return firstSameName, nil
	}
	if len(nearby) > 0 {
		return nil, fmt.Errorf("no registry server named %q (did you mean one of: %s)", name, strings.Join(dedupeLimit(nearby, 5), ", "))
	}
	return nil, fmt.Errorf("no registry server named %q", name)
}

// Drain pages the whole registry (latest version of each server, deduped by
// name) up to max entries. It is the population source for the scan/index
// command. max<=0 means "the whole registry".
func (c *Client) Drain(ctx context.Context, max int) ([]Server, error) {
	seen := make(map[string]Server)
	cursor := ""
	for {
		u, err := url.Parse(c.BaseURL)
		if err != nil {
			return nil, err
		}
		q := u.Query()
		q.Set("limit", "100")
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		u.RawQuery = q.Encode()

		list, err := c.getList(ctx, u.String())
		if err != nil {
			return nil, err
		}
		for _, e := range list.Servers {
			if !e.Meta.Official.IsLatest {
				continue
			}
			s := e.normalize()
			seen[s.Name] = s
		}
		if max > 0 && len(seen) >= max {
			break
		}
		cursor = list.Metadata.NextCursor
		if cursor == "" {
			break
		}
		// Polite pacing between pages.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	out := make([]Server, 0, len(seen))
	for _, s := range seen {
		out = append(out, s)
	}
	// Sort by name so a max-limited drain is deterministic: ranging a Go map is
	// randomized, which would return a different subset every run and break any
	// dated, reproducible index snapshot built on top of Drain.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out, nil
}

// remoteTransport guesses the MCP transport from a remote URL path. Cosmetic:
// the transport does not change how the endpoint is probed.
func remoteTransport(path string) string {
	if strings.HasSuffix(strings.ToLower(strings.TrimRight(path, "/")), "sse") {
		return "sse"
	}
	return "streamable-http"
}

func (c *Client) getList(ctx context.Context, u string) (*rawList, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry request: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry returned HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var list rawList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode registry response: %w", err)
	}
	return &list, nil
}

// githubParts extracts owner and repo from a github.com URL path.
func githubParts(p string) (owner, repo string) {
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, ".git")
	parts := strings.Split(p, "/")
	if len(parts) >= 2 {
		return parts[0], parts[1]
	}
	return "", ""
}

func dedupeLimit(in []string, n int) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) >= n {
			break
		}
	}
	return out
}
