package scan

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func resp(status int, header http.Header) *http.Response {
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(""))}
}

// noWaitTransport builds a githubBackoffTransport whose "after" fires
// immediately, so tests never actually sleep, and records every requested
// duration.
func noWaitTransport(base http.RoundTripper) (*githubBackoffTransport, *[]time.Duration) {
	var waited []time.Duration
	t := &githubBackoffTransport{
		base: base,
		after: func(d time.Duration) <-chan time.Time {
			waited = append(waited, d)
			ch := make(chan time.Time, 1)
			ch <- time.Now()
			return ch
		},
	}
	return t, &waited
}

func githubRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/repos/acme/thing", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestGitHubBackoffTransportRetriesOnPrimaryRateLimit(t *testing.T) {
	calls := 0
	base := rtFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls < 3 {
			h := http.Header{}
			h.Set("X-RateLimit-Remaining", "0")
			return resp(http.StatusForbidden, h), nil
		}
		return resp(http.StatusOK, nil), nil
	})
	tr, waited := noWaitTransport(base)

	res, err := tr.RoundTrip(githubRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (two rate-limited, one success)", calls)
	}
	if len(*waited) != 2 {
		t.Errorf("waited %d times, want 2", len(*waited))
	}
}

func TestGitHubBackoffTransportHonorsRetryAfter(t *testing.T) {
	calls := 0
	base := rtFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			h := http.Header{}
			h.Set("Retry-After", "7")
			return resp(http.StatusForbidden, h), nil
		}
		return resp(http.StatusOK, nil), nil
	})
	tr, waited := noWaitTransport(base)

	res, err := tr.RoundTrip(githubRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if len(*waited) != 1 || (*waited)[0] != 7*time.Second {
		t.Errorf("waited = %v, want [7s]", *waited)
	}
}

func TestGitHubBackoffTransportGivesUpAfterMaxRetries(t *testing.T) {
	calls := 0
	base := rtFunc(func(*http.Request) (*http.Response, error) {
		calls++
		h := http.Header{}
		h.Set("X-RateLimit-Remaining", "0")
		return resp(http.StatusForbidden, h), nil
	})
	tr, _ := noWaitTransport(base)

	res, err := tr.RoundTrip(githubRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (still rate-limited after giving up)", res.StatusCode)
	}
	if calls != maxGitHubRetries+1 {
		t.Errorf("calls = %d, want %d", calls, maxGitHubRetries+1)
	}
}

func TestGitHubBackoffTransportPassesThroughNonGitHubHost(t *testing.T) {
	calls := 0
	base := rtFunc(func(*http.Request) (*http.Response, error) {
		calls++
		h := http.Header{}
		h.Set("X-RateLimit-Remaining", "0") // would look rate-limited, but this is not GitHub
		return resp(http.StatusForbidden, h), nil
	})
	tr, waited := noWaitTransport(base)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://registry.npmjs.org/thing", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (passed straight through)", res.StatusCode)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry for a non-GitHub host)", calls)
	}
	if len(*waited) != 0 {
		t.Errorf("waited %v, want no backoff for a non-GitHub host", *waited)
	}
}

func TestGitHubBackoffTransportPassesThroughOrdinaryResponses(t *testing.T) {
	calls := 0
	base := rtFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return resp(http.StatusOK, nil), nil
	})
	tr, waited := noWaitTransport(base)

	res, err := tr.RoundTrip(githubRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || calls != 1 || len(*waited) != 0 {
		t.Errorf("status=%d calls=%d waited=%v, want a single passthrough call", res.StatusCode, calls, *waited)
	}
}

func TestGitHubBackoffTransportStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	base := rtFunc(func(*http.Request) (*http.Response, error) {
		calls++
		h := http.Header{}
		h.Set("X-RateLimit-Remaining", "0")
		return resp(http.StatusForbidden, h), nil
	})
	t2 := &githubBackoffTransport{
		base: base,
		after: func(time.Duration) <-chan time.Time {
			cancel() // simulate the deadline passing during the wait
			return make(chan time.Time)
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/acme/thing", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := t2.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (returned rather than retried forever)", res.StatusCode)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (stopped waiting once the context was done)", calls)
	}
}

func TestNewGitHubBackoffTransportDefaultsBase(t *testing.T) {
	if NewGitHubBackoffTransport(nil) == nil {
		t.Fatal("expected a non-nil transport")
	}
}
