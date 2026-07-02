package scan

import (
	"io"
	"net/http"
	"strconv"
	"time"
)

// githubAPIHost is the only host githubBackoffTransport treats specially.
const githubAPIHost = "api.github.com"

// maxGitHubRetries bounds how many times a single GitHub request is retried
// after a rate-limit response, so a run that stays rate-limited still
// terminates instead of retrying forever.
const maxGitHubRetries = 5

// maxGitHubBackoff caps the exponential-backoff fallback delay, so a missing
// or malformed rate-limit header never stalls a worker for an unreasonable
// time.
const maxGitHubBackoff = 60 * time.Second

// githubBackoffTransport wraps an http.RoundTripper and adds bounded
// backoff-and-retry for GitHub's primary and secondary rate limits. A
// census that touches thousands of repositories will hit both over a long
// run; without this, the run would abort partway through instead of
// slowing down and finishing. Every other host passes through unchanged.
//
// This is backoff-only, not conditional (ETag) requests: checkRepo issues at
// most one GitHub request per server per run, so there is never a second
// request for the same resource to make conditional, and an ETag cache
// would add state for no benefit here.
type githubBackoffTransport struct {
	base http.RoundTripper
	// after stands in for time.After so tests do not actually wait.
	after func(time.Duration) <-chan time.Time
}

// NewGitHubBackoffTransport wraps base with GitHub rate-limit backoff. A nil
// base falls back to http.DefaultTransport.
func NewGitHubBackoffTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &githubBackoffTransport{base: base, after: time.After}
}

// RoundTrip implements http.RoundTripper. GitHub GETs issued by the probe
// engine carry no body, so the same *http.Request can be safely replayed on
// every retry.
func (t *githubBackoffTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Hostname() != githubAPIHost {
		return t.base.RoundTrip(req)
	}

	var res *http.Response
	var err error
	for attempt := 0; attempt <= maxGitHubRetries; attempt++ {
		res, err = t.base.RoundTrip(req)
		if err != nil || !rateLimited(res) {
			return res, err
		}
		if attempt == maxGitHubRetries {
			return res, err
		}

		wait := backoffDelay(res, attempt)
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()

		select {
		case <-req.Context().Done():
			return res, err // the caller's own timeout won; hand back the last response
		case <-t.after(wait):
		}
	}
	return res, err
}

// rateLimited reports whether res is a GitHub primary or secondary
// rate-limit response. Primary: 403/429 with X-RateLimit-Remaining: 0.
// Secondary: 403/429 with a Retry-After header.
func rateLimited(res *http.Response) bool {
	if res == nil {
		return false
	}
	if res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusTooManyRequests {
		return false
	}
	return res.Header.Get("X-RateLimit-Remaining") == "0" || res.Header.Get("Retry-After") != ""
}

// backoffDelay picks how long to wait before retrying a rate-limited
// request: GitHub's own Retry-After or X-RateLimit-Reset header when
// present, otherwise capped exponential backoff.
func backoffDelay(res *http.Response, attempt int) time.Duration {
	if ra := res.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	if reset := res.Header.Get("X-RateLimit-Reset"); reset != "" {
		if epoch, err := strconv.ParseInt(reset, 10, 64); err == nil {
			if d := time.Until(time.Unix(epoch, 0)); d > 0 {
				return d
			}
		}
	}
	d := time.Duration(1<<uint(attempt)) * time.Second
	if d > maxGitHubBackoff {
		d = maxGitHubBackoff
	}
	return d
}
