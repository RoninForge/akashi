package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/RoninForge/akashi/internal/registry"
)

// DefaultUserAgent identifies akashi politely to every third party it probes.
const DefaultUserAgent = "akashi (mcp health probe; keyless; +https://roninforge.org)"

const (
	// staleDays: an alive repo with no push in over this many days is a
	// degraded (maintenance) signal.
	staleDays = 365
	// warnDays: freshness display turns from pass to warn past this age. It
	// does not, on its own, change the verdict.
	warnDays = 90
)

// Engine runs keyless probes. The zero value is not usable; call NewEngine.
type Engine struct {
	HTTP *http.Client
	// UserAgent is sent on every request.
	UserAgent string
	// GitHubToken, when set, is sent only to api.github.com to raise the rate
	// limit. It is never sent to a probed server.
	GitHubToken string
	// Now supplies the clock; overridable in tests.
	Now func() time.Time
	// RemoteTimeout bounds each remote-endpoint request.
	RemoteTimeout time.Duration
	// RequestTimeout bounds each registry/repo/package request.
	RequestTimeout time.Duration
}

// NewEngine returns an Engine with sane defaults.
func NewEngine() *Engine {
	return &Engine{
		HTTP:           &http.Client{},
		UserAgent:      DefaultUserAgent,
		Now:            time.Now,
		RemoteTimeout:  12 * time.Second,
		RequestTimeout: 15 * time.Second,
	}
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// ProbeServer runs the full keyless check set against one server and returns a
// classified Result.
func (e *Engine) ProbeServer(ctx context.Context, s registry.Server) Result {
	sig := Signals{Repo: e.checkRepo(ctx, s.Repository)}
	for _, p := range s.Packages {
		sig.Packages = append(sig.Packages, e.checkPackage(ctx, p))
	}
	for _, rm := range s.Remotes {
		sig.Remotes = append(sig.Remotes, e.checkRemote(ctx, rm))
	}

	res := classify(s, sig)
	res.CheckedAt = e.now().UTC().Format("2006-01-02")
	return res
}

// --- repository ---

func (e *Engine) checkRepo(ctx context.Context, repo *registry.Repository) RepoSignal {
	if repo == nil || repo.URL == "" {
		return RepoSignal{Kind: "none", Status: "none"}
	}
	owner, name, ok := parseGitHubRepo(repo.URL)
	if !ok {
		return RepoSignal{Kind: "other", Status: "unprobed", URL: repo.URL}
	}

	headers := map[string]string{"Accept": "application/vnd.github+json"}
	if e.GitHubToken != "" {
		headers["Authorization"] = "Bearer " + e.GitHubToken
	}
	r := e.fetch(ctx, http.MethodGet, "https://api.github.com/repos/"+owner+"/"+name, headers, nil, e.RequestTimeout, 1<<20)
	if r.err != nil {
		return RepoSignal{Kind: "github", Status: "error", URL: repo.URL, Detail: r.err.Error()}
	}
	switch {
	case r.status == http.StatusNotFound:
		return RepoSignal{Kind: "github", Status: "missing", HTTPStatus: 404, URL: repo.URL}
	case r.status != http.StatusOK:
		return RepoSignal{Kind: "github", Status: "error", HTTPStatus: r.status, URL: repo.URL}
	}

	var gh struct {
		Archived bool   `json:"archived"`
		Disabled bool   `json:"disabled"`
		PushedAt string `json:"pushed_at"`
		Stars    int    `json:"stargazers_count"`
		License  *struct {
			SPDXID string `json:"spdx_id"`
		} `json:"license"`
	}
	if err := json.Unmarshal(r.body, &gh); err != nil {
		return RepoSignal{Kind: "github", Status: "error", HTTPStatus: r.status, URL: repo.URL, Detail: "decode: " + err.Error()}
	}

	sig := RepoSignal{
		Kind:     "github",
		Status:   "alive",
		Archived: gh.Archived,
		PushedAt: gh.PushedAt,
		Stars:    intPtr(gh.Stars),
		URL:      repo.URL,
	}
	if gh.Archived {
		sig.Status = "archived"
	}
	if gh.License != nil && gh.License.SPDXID != "" && gh.License.SPDXID != "NOASSERTION" {
		sig.License = gh.License.SPDXID
	}
	if gh.PushedAt != "" {
		if t, err := time.Parse(time.RFC3339, gh.PushedAt); err == nil {
			days := int(e.now().Sub(t).Hours() / 24)
			sig.AgeDays = intPtr(days)
		}
	}
	return sig
}

// parseGitHubRepo extracts owner and repo from a repository URL, requiring the
// host to be exactly github.com. Parsing with net/url (rather than a substring
// regex) means a crafted URL like https://evil.example/?u=github.com/o/r, or a
// gist.github.com URL, is not mistaken for a real GitHub repository.
func parseGitHubRepo(raw string) (owner, repo string, ok bool) {
	u, err := url.Parse(strings.TrimSuffix(raw, ".git"))
	if err != nil {
		return "", "", false
	}
	if strings.ToLower(u.Hostname()) != "github.com" {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// --- packages ---

func (e *Engine) checkPackage(ctx context.Context, p registry.Package) PackageSignal {
	switch p.RegistryType {
	case "npm":
		return e.checkNpm(ctx, p.Identifier)
	case "pypi":
		return e.checkPypi(ctx, p.Identifier)
	case "oci":
		return e.checkOci(ctx, p.Identifier)
	default:
		return PackageSignal{Type: p.RegistryType, ID: p.Identifier, Status: "unprobed"}
	}
}

func (e *Engine) checkNpm(ctx context.Context, id string) PackageSignal {
	out := PackageSignal{Type: "npm", ID: id}
	// url.PathEscape encodes a scoped name "@scope/name" to the canonical
	// "@scope%2Fname" the npm registry expects.
	r := e.fetch(ctx, http.MethodGet, "https://registry.npmjs.org/"+url.PathEscape(id), jsonHeaders(), nil, e.RequestTimeout, 16<<20)
	if r.err != nil {
		out.Status = "error"
		out.Detail = r.err.Error()
		return out
	}
	if r.status == http.StatusNotFound {
		out.Status = "missing"
		out.HTTPStatus = 404
		return out
	}
	if r.status != http.StatusOK {
		out.Status = "error"
		out.HTTPStatus = r.status
		return out
	}
	var doc struct {
		Time     map[string]json.RawMessage `json:"time"`
		Versions map[string]json.RawMessage `json:"versions"`
		DistTags struct {
			Latest string `json:"latest"`
		} `json:"dist-tags"`
	}
	if err := json.Unmarshal(r.body, &doc); err != nil {
		// A 200 from the registry already proves the package exists; the
		// packument was only unparseable (very large or truncated at the read
		// cap). Treat it as published rather than dropping a live package into
		// an error state, which would wrongly discount it as an entrypoint.
		out.Status = "published"
		out.Detail = "packument parse incomplete"
		return out
	}
	if _, unpublished := doc.Time["unpublished"]; unpublished {
		out.Status = "unpublished"
		return out
	}
	if len(doc.Versions) == 0 {
		out.Status = "missing"
		return out
	}
	out.Status = "published"
	out.Latest = doc.DistTags.Latest
	return out
}

func (e *Engine) checkPypi(ctx context.Context, id string) PackageSignal {
	out := PackageSignal{Type: "pypi", ID: id}
	r := e.fetch(ctx, http.MethodGet, "https://pypi.org/pypi/"+url.PathEscape(id)+"/json", jsonHeaders(), nil, e.RequestTimeout, 16<<20)
	if r.err != nil {
		out.Status = "error"
		out.Detail = r.err.Error()
		return out
	}
	if r.status == http.StatusNotFound {
		out.Status = "missing"
		out.HTTPStatus = 404
		return out
	}
	if r.status != http.StatusOK {
		out.Status = "error"
		out.HTTPStatus = r.status
		return out
	}
	var doc struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
		Releases map[string]json.RawMessage `json:"releases"`
	}
	if err := json.Unmarshal(r.body, &doc); err != nil {
		// A 200 already proves the project exists; an unparseable/truncated
		// body should not discount a live package.
		out.Status = "published"
		out.Detail = "metadata parse incomplete"
		return out
	}
	if len(doc.Releases) == 0 {
		out.Status = "missing"
		return out
	}
	out.Status = "published"
	out.Latest = doc.Info.Version
	return out
}

var ociRepoRe = regexp.MustCompile(`^([\w.-]+/[\w.-]+)$`)

func (e *Engine) checkOci(ctx context.Context, identifier string) PackageSignal {
	out := PackageSignal{Type: "oci", ID: identifier}
	repo, tag := parseOCI(identifier)
	if repo == "" {
		out.Status = "unprobed"
		out.Detail = "not a Docker Hub reference; not keyless-probeable"
		return out
	}
	// Anonymous Docker Hub pull token, then a manifest HEAD. Anonymous = keyless.
	tokURL := "https://auth.docker.io/token?service=registry.docker.io&scope=repository:" + repo + ":pull"
	tokRes := e.fetch(ctx, http.MethodGet, tokURL, map[string]string{}, nil, e.RequestTimeout, 1<<20)
	if tokRes.err != nil || tokRes.status != http.StatusOK {
		out.Status = "unprobed"
		out.Detail = "could not obtain an anonymous pull token"
		return out
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(tokRes.body, &tok); err != nil || tok.Token == "" {
		out.Status = "unprobed"
		out.Detail = "anonymous pull token unavailable"
		return out
	}
	manHeaders := map[string]string{
		"Authorization": "Bearer " + tok.Token,
		"Accept":        "application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.docker.distribution.manifest.v2+json",
	}
	man := e.fetch(ctx, http.MethodHead, "https://registry-1.docker.io/v2/"+repo+"/manifests/"+url.PathEscape(tag), manHeaders, nil, e.RequestTimeout, 0)
	if man.err != nil {
		out.Status = "unprobed"
		out.Detail = "manifest request failed: " + man.err.Error()
		return out
	}
	switch {
	case man.status == http.StatusNotFound:
		out.Status = "missing"
		out.HTTPStatus = 404
	case man.status >= 200 && man.status < 300:
		out.Status = "published"
	default:
		out.Status = "unprobed"
		out.HTTPStatus = man.status
	}
	return out
}

// parseOCI splits a Docker Hub reference into repo (with library/ prefix for
// bare official images) and tag. Non-Docker-Hub or malformed refs return "".
func parseOCI(identifier string) (repo, tag string) {
	s := strings.TrimPrefix(identifier, "docker.io/")
	tag = "latest"
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s[i+1:], "/") {
		tag = s[i+1:]
		s = s[:i]
	}
	if !strings.Contains(s, "/") {
		s = "library/" + s
	}
	if strings.Count(s, "/") > 1 || !ociRepoRe.MatchString(s) {
		return "", "" // non-Docker-Hub registry: not keyless-probeable here
	}
	return s, tag
}

// --- remotes ---

func (e *Engine) checkRemote(ctx context.Context, rm registry.Remote) RemoteSignal {
	// Try the capability-only MCP `initialize` handshake first: it is the one
	// probe that yields a conformance signal, which is the whole point of a
	// certificate tool. If it reaches the endpoint (any conformance verdict,
	// including auth-gated), it is authoritative.
	if init := e.mcpInitialize(ctx, rm.URL); init.err == nil && init.sig.Status == "reachable" {
		init.sig.Type = rm.Type
		return init.sig
	}

	// initialize did not prove the endpoint alive (it errored, 404'd, or 5xx'd).
	// That is common for SSE-transport servers, where a POST to the base URL
	// does not fit. Confirm reachability with a plain GET before calling the
	// endpoint down, so a transport mismatch is never miscounted as dead.
	get := e.fetch(ctx, http.MethodGet, rm.URL,
		map[string]string{"Accept": "application/json, text/event-stream"},
		nil, e.RemoteTimeout, -1)
	if get.err == nil && get.status < 500 && get.status != http.StatusNotFound {
		// Up, but we could not verify MCP conformance over this transport.
		return RemoteSignal{URL: rm.URL, Type: rm.Type, Status: "reachable", HTTPStatus: get.status, Probe: "get"}
	}

	// Down. Prefer the most specific signal we have.
	out := RemoteSignal{URL: rm.URL, Type: rm.Type, Probe: "get"}
	switch {
	case get.err != nil:
		out.Status = "unreachable"
		out.Detail = get.err.Error()
	case get.status >= 500:
		out.Status = "server_error"
		out.HTTPStatus = get.status
	default:
		out.Status = "not_found"
		out.HTTPStatus = get.status
	}
	return out
}

type initResult struct {
	sig RemoteSignal
	err error
}

// mcpInitialize sends the MCP `initialize` request - a capability negotiation
// that runs no tool, so it is safe against an untrusted server. It maps the
// response to a reachability status and a conformance verdict:
//
//	401/403         -> reachable, auth_gated       (alive, cannot verify keyless)
//	5xx             -> server_error                (down)
//	404             -> not_found                   (down)
//	2xx + MCP body  -> reachable, initialize_ok    (conformant)
//	2xx + other body-> reachable, reachable_nonconformant  (answered 200 but is not MCP)
//	other (400/405) -> reachable, unverified       (up, but our POST did not fit; e.g. SSE)
func (e *Engine) mcpInitialize(ctx context.Context, u string) initResult {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"akashi","version":"0"}}}`)
	r := e.fetch(ctx, http.MethodPost, u, map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json, text/event-stream",
	}, body, e.RemoteTimeout, 256<<10)
	if r.err != nil {
		return initResult{err: r.err}
	}
	sig := RemoteSignal{URL: u, HTTPStatus: r.status, Probe: "initialize"}
	switch {
	case r.status == http.StatusUnauthorized || r.status == http.StatusForbidden:
		sig.Status = "reachable"
		sig.Conformance = "auth_gated"
	case r.status >= 500:
		sig.Status = "server_error"
	case r.status == http.StatusNotFound:
		sig.Status = "not_found"
	case r.status >= 200 && r.status < 300:
		sig.Status = "reachable"
		txt := string(r.body)
		if strings.Contains(txt, `"result"`) || strings.Contains(txt, "protocolVersion") || strings.Contains(txt, "serverInfo") {
			sig.Conformance = "initialize_ok"
			echoed := jsonRPCIDEchoed(r.body)
			sig.IDEchoed = &echoed
		} else {
			// A 200 that is not a JSON-RPC MCP response: an HTML page or proxy
			// masquerading as a server. This is a real conformance failure.
			sig.Conformance = "reachable_nonconformant"
		}
	default:
		// 400/405/406 and similar: the endpoint answered, so it is up, but our
		// POST-initialize did not fit its transport (often an SSE/GET-only
		// server). Alive; conformance is simply not verifiable this way.
		sig.Status = "reachable"
		sig.Conformance = "unverified"
	}
	return initResult{sig: sig}
}

// jsonRPCIDEchoed reports whether a JSON-RPC response echoes the request id
// (1). SSE frames prefix the JSON with "data:", so we scan the raw bytes for
// an id:1 token rather than strictly decoding.
var idEchoRe = regexp.MustCompile(`"id"\s*:\s*1\b`)

func jsonRPCIDEchoed(body []byte) bool {
	return idEchoRe.Match(body)
}

// --- shared HTTP ---

type httpResult struct {
	status int
	body   []byte
	err    error
}

// fetch performs one request fully within a per-call timeout: it reads up to
// maxBody bytes and cancels the context before returning, so no read outlives
// the deadline.
//
//	maxBody > 0  read up to maxBody bytes of the body
//	maxBody == 0 drain a little so the connection can be pooled (for HEAD)
//	maxBody < 0  status only: do not read the body at all. Used for the remote
//	             reachability GET, whose body is unused and which may be an SSE
//	             stream that never ends (reading it would block until timeout).
func (e *Engine) fetch(ctx context.Context, method, url string, headers map[string]string, body []byte, timeout time.Duration, maxBody int64) httpResult {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, url, rdr)
	if err != nil {
		return httpResult{err: err}
	}
	req.Header.Set("User-Agent", e.UserAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := e.HTTP.Do(req)
	if err != nil {
		return httpResult{err: err}
	}
	defer res.Body.Close()
	out := httpResult{status: res.StatusCode}
	switch {
	case maxBody > 0:
		out.body, err = io.ReadAll(io.LimitReader(res.Body, maxBody))
		if err != nil {
			return httpResult{status: res.StatusCode, err: err}
		}
	case maxBody == 0:
		// Drain a little so the connection can be reused (HEAD has no body).
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	default:
		// Status only: read nothing. The deferred Close discards the rest.
	}
	return out
}

func jsonHeaders() map[string]string {
	return map[string]string{"Accept": "application/json"}
}

func intPtr(i int) *int { return &i }
