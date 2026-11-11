package ado

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TokenSource supplies the Authorization header value for Azure DevOps requests.
type TokenSource interface {
	Header(ctx context.Context) (string, error)
	// Invalidate discards a cached token after the service rejected it.
	Invalidate()
}

// PAT authenticates with a personal access token using HTTP Basic.
type PAT string

func (p PAT) Header(context.Context) (string, error) {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+string(p))), nil
}
func (PAT) Invalidate() {}

// Bearer uses a supplied Entra access token; renewal is the caller's responsibility.
type Bearer string

func (b Bearer) Header(context.Context) (string, error) { return "Bearer " + string(b), nil }
func (Bearer) Invalidate()                              {}

// CommandToken runs a command that prints an Entra access token for Azure DevOps (for
// example `az account get-access-token --resource 499b84ac-1321-427f-aa17-267ca6975798
// --query accessToken -o tsv`) and caches it, so long backfills survive token expiry.
type CommandToken struct {
	Command []string
	TTL     time.Duration
	mu      sync.Mutex
	token   string
	expires time.Time
}

func (c *CommandToken) Header(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return "Bearer " + c.token, nil
	}
	if len(c.Command) == 0 {
		return "", errors.New("token command not configured")
	}
	cmd := exec.CommandContext(ctx, c.Command[0], c.Command[1:]...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("token command failed (output withheld)")
	}
	token := strings.TrimSpace(string(out))
	if token == "" || strings.ContainsAny(token, " \r\n") {
		return "", errors.New("token command returned an unexpected value")
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	c.token, c.expires = token, time.Now().Add(ttl)
	return "Bearer " + token, nil
}

func (c *CommandToken) Invalidate() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

// Counters accumulate request outcomes for collection monitoring.
type Counters struct {
	Requests  atomic.Int64
	Retries   atomic.Int64
	Throttles atomic.Int64
}

// Transport adds authentication, bounded retry with backoff (honoring Retry-After), and
// accounting to every SDK request. It never follows redirects with credentials attached.
type Transport struct {
	Base        http.RoundTripper
	Tokens      TokenSource
	Counters    *Counters
	MaxAttempts int
	MaxDelay    time.Duration
	sleep       func(context.Context, time.Duration) error
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	attempts := t.MaxAttempts
	if attempts <= 0 {
		attempts = 6
	}
	maxDelay := t.MaxDelay
	if maxDelay <= 0 {
		maxDelay = 90 * time.Second
	}
	sleep := t.sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	var body []byte
	if req.Body != nil && req.Body != http.NoBody {
		b, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
	}
	reauthorized := false
	for attempt := 1; ; attempt++ {
		r := req.Clone(req.Context())
		if body != nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		if t.Tokens != nil {
			h, err := t.Tokens.Header(req.Context())
			if err != nil {
				return nil, err
			}
			r.Header.Set("Authorization", h)
		}
		if t.Counters != nil {
			t.Counters.Requests.Add(1)
		}
		res, err := base.RoundTrip(r)
		retryable := err != nil && req.Context().Err() == nil
		var delay time.Duration
		if err == nil {
			switch {
			case res.StatusCode == http.StatusUnauthorized && !reauthorized && t.Tokens != nil:
				// An expired cached token: refresh once and retry immediately.
				reauthorized = true
				t.Tokens.Invalidate()
				drain(res)
				attempt--
				continue
			case res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusServiceUnavailable && res.Header.Get("Retry-After") != "":
				if t.Counters != nil {
					t.Counters.Throttles.Add(1)
				}
				retryable, delay = true, retryAfter(res.Header.Get("Retry-After"))
			case res.StatusCode >= 500:
				retryable = true
			default:
				// Azure DevOps advertises upcoming throttling; slow down before it rejects us.
				if d := retryAfter(res.Header.Get("Retry-After")); d > 0 && res.StatusCode < 300 {
					if t.Counters != nil {
						t.Counters.Throttles.Add(1)
					}
					if d > maxDelay {
						d = maxDelay
					}
					_ = sleep(req.Context(), d)
				}
				return res, nil
			}
		}
		if !retryable || attempt >= attempts {
			return res, err
		}
		if err == nil {
			drain(res)
		}
		if delay <= 0 {
			delay = time.Duration(1<<min(attempt, 6)) * time.Second
		}
		if delay > maxDelay {
			delay = maxDelay
		}
		if t.Counters != nil {
			t.Counters.Retries.Add(1)
		}
		if err := sleep(req.Context(), delay); err != nil {
			return nil, err
		}
	}
}

func drain(res *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	res.Body.Close()
}

func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil && s > 0 {
		return time.Duration(s * float64(time.Second))
	}
	if at, err := http.ParseTime(v); err == nil {
		return time.Until(at)
	}
	return 0
}

// NewHTTPClient builds the HTTP client used by all SDK clients.
func NewHTTPClient(tokens TokenSource, counters *Counters, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &Transport{Tokens: tokens, Counters: counters},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
