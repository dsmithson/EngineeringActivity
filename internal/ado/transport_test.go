package ado

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/microsoft/azure-devops-go-api/azuredevops/v7"
	"github.com/microsoft/azure-devops-go-api/azuredevops/v7/git"
	"github.com/microsoft/azure-devops-go-api/azuredevops/v7/webapi"
)

// countingTokens records Invalidate calls.
type countingTokens struct {
	mu          sync.Mutex
	invalidated int
	header      string
}

func (c *countingTokens) Header(context.Context) (string, error) { return c.header, nil }
func (c *countingTokens) Invalidate() {
	c.mu.Lock()
	c.invalidated++
	c.mu.Unlock()
}

type sleepRecorder struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.delays = append(s.delays, d)
	s.mu.Unlock()
	return ctx.Err()
}

func newTransport(tokens TokenSource, rec *sleepRecorder) (*Transport, *Counters) {
	counters := &Counters{}
	t := &Transport{Tokens: tokens, Counters: counters}
	if rec != nil {
		t.sleep = rec.sleep
	}
	return t, counters
}

func get(t *testing.T, tr http.RoundTripper, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestPATHeader(t *testing.T) {
	h, err := PAT("secret").Header(context.Background())
	if err != nil || h != "Basic "+base64.StdEncoding.EncodeToString([]byte(":secret")) {
		t.Fatalf("header %q %v", h, err)
	}
	if b, _ := Bearer("tok").Header(context.Background()); b != "Bearer tok" {
		t.Fatalf("bearer %q", b)
	}
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r.Header.Get("Authorization") }))
	defer srv.Close()
	tr, _ := newTransport(PAT("secret"), nil)
	get(t, tr, srv.URL).Body.Close()
	if seen != "Basic OnNlY3JldA==" {
		t.Fatalf("server saw %q", seen)
	}
}

func TestRetryOn500ThenSuccess(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	rec := &sleepRecorder{}
	tr, c := newTransport(nil, rec)
	res := get(t, tr, srv.URL)
	defer res.Body.Close()
	if res.StatusCode != 200 || calls != 3 || c.Retries.Load() != 2 || c.Throttles.Load() != 0 || c.Requests.Load() != 3 {
		t.Fatalf("status %d calls %d retries %d throttles %d", res.StatusCode, calls, c.Retries.Load(), c.Throttles.Load())
	}
	if len(rec.delays) != 2 || rec.delays[0] != 2*time.Second || rec.delays[1] != 4*time.Second {
		t.Fatalf("exponential backoff delays %v", rec.delays)
	}
}

func TestThrottleRetryAfter(t *testing.T) {
	for name, header := range map[string]string{
		"seconds":   "7",
		"http-date": time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat),
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					w.Header().Set("Retry-After", header)
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				w.Write([]byte("ok"))
			}))
			defer srv.Close()
			rec := &sleepRecorder{}
			tr, c := newTransport(nil, rec)
			res := get(t, tr, srv.URL)
			res.Body.Close()
			if res.StatusCode != 200 || c.Throttles.Load() != 1 || c.Retries.Load() != 1 {
				t.Fatalf("status %d throttles %d retries %d", res.StatusCode, c.Throttles.Load(), c.Retries.Load())
			}
			if len(rec.delays) != 1 {
				t.Fatalf("delays %v", rec.delays)
			}
			d := rec.delays[0]
			if name == "seconds" && d != 7*time.Second {
				t.Fatalf("delay %v", d)
			}
			if name == "http-date" && (d < 20*time.Second || d > 31*time.Second) {
				t.Fatalf("http-date delay %v", d)
			}
		})
	}
}

func TestRetryAfterCappedByMaxDelay(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	rec := &sleepRecorder{}
	tr, c := newTransport(nil, rec)
	tr.MaxDelay = 5 * time.Second
	res := get(t, tr, srv.URL)
	res.Body.Close()
	if res.StatusCode != 200 || c.Throttles.Load() != 1 || len(rec.delays) != 1 || rec.delays[0] != 5*time.Second {
		t.Fatalf("503 Retry-After: status %d throttles %d delays %v", res.StatusCode, c.Throttles.Load(), rec.delays)
	}
}

func TestPostBodyReplayedOnRetry(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if len(bodies) == 1 {
			w.WriteHeader(502)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	tr, _ := newTransport(nil, &sleepRecorder{})
	req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader([]byte(`{"query":"SELECT 1"}`)))
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(bodies) != 2 || bodies[0] != bodies[1] || bodies[1] != `{"query":"SELECT 1"}` {
		t.Fatalf("bodies %q", bodies)
	}
}

func TestUnauthorizedInvalidatesOnce(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	tokens := &countingTokens{header: "Bearer x"}
	rec := &sleepRecorder{}
	tr, c := newTransport(tokens, rec)
	res := get(t, tr, srv.URL)
	res.Body.Close()
	if res.StatusCode != 401 || calls != 2 || tokens.invalidated != 1 || c.Retries.Load() != 0 || len(rec.delays) != 0 {
		t.Fatalf("status %d calls %d invalidated %d retries %d", res.StatusCode, calls, tokens.invalidated, c.Retries.Load())
	}
}

func TestMaxAttempts(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(500)
	}))
	defer srv.Close()
	tr, c := newTransport(nil, &sleepRecorder{})
	tr.MaxAttempts = 3
	res := get(t, tr, srv.URL)
	res.Body.Close()
	if res.StatusCode != 500 || calls != 3 || c.Retries.Load() != 2 {
		t.Fatalf("status %d calls %d retries %d", res.StatusCode, calls, c.Retries.Load())
	}
}

func TestCancellationDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	tr, _ := newTransport(nil, nil) // real sleep
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	start := time.Now()
	_, err := tr.RoundTrip(req)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	destinationHit := false
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationHit = true
		if r.Header.Get("Authorization") != "" {
			t.Error("credential forwarded to redirect target")
		}
	}))
	defer dest.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, dest.URL, http.StatusFound) }))
	defer srv.Close()
	client := NewHTTPClient(PAT("secret-value"), &Counters{}, 5*time.Second)
	res, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound || destinationHit {
		t.Fatalf("redirect followed: status %d hit %v", res.StatusCode, destinationHit)
	}
}

// TestHelperProcess is re-executed by CommandToken tests as a fake token command.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("ADO_TOKEN_HELPER") != "1" {
		return
	}
	if f := os.Getenv("ADO_TOKEN_HELPER_COUNT"); f != "" {
		fh, _ := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		fh.WriteString("x")
		fh.Close()
	}
	fmt.Print(os.Getenv("ADO_TOKEN_HELPER_OUTPUT"))
	os.Exit(0)
}

func helperToken(t *testing.T, output string, ttl time.Duration) (*CommandToken, func() int) {
	t.Helper()
	count := filepath.Join(t.TempDir(), "count")
	t.Setenv("ADO_TOKEN_HELPER", "1")
	t.Setenv("ADO_TOKEN_HELPER_OUTPUT", output)
	t.Setenv("ADO_TOKEN_HELPER_COUNT", count)
	ct := &CommandToken{Command: []string{os.Args[0], "-test.run=^TestHelperProcess$"}, TTL: ttl}
	return ct, func() int { b, _ := os.ReadFile(count); return len(b) }
}

func TestCommandTokenCachingAndInvalidate(t *testing.T) {
	ct, calls := helperToken(t, "tok-123\n", time.Hour)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		h, err := ct.Header(ctx)
		if err != nil || h != "Bearer tok-123" {
			t.Fatalf("header %q %v", h, err)
		}
	}
	if calls() != 1 {
		t.Fatalf("command ran %d times; expected cached token", calls())
	}
	ct.Invalidate()
	if _, err := ct.Header(ctx); err != nil || calls() != 2 {
		t.Fatalf("invalidate did not refresh: %d %v", calls(), err)
	}
}

func TestCommandTokenTTLExpiry(t *testing.T) {
	ct, calls := helperToken(t, "tok", time.Nanosecond)
	ctx := context.Background()
	ct.Header(ctx)
	time.Sleep(2 * time.Millisecond)
	ct.Header(ctx)
	if calls() != 2 {
		t.Fatalf("expired token not refreshed: %d", calls())
	}
}

func TestCommandTokenRejectsBadOutput(t *testing.T) {
	for _, out := range []string{"two words", "line1\nline2", "   "} {
		ct, _ := helperToken(t, out, time.Hour)
		if h, err := ct.Header(context.Background()); err == nil {
			t.Errorf("output %q accepted as %q", out, h)
		}
	}
	if _, err := (&CommandToken{}).Header(context.Background()); err == nil {
		t.Error("empty command accepted")
	}
}

func ptr[T any](v T) *T { return &v }

func TestConvertPullRequest(t *testing.T) {
	created := time.Date(2026, 10, 1, 12, 0, 0, 0, time.FixedZone("x", -7*3600))
	closed := created.Add(5 * time.Hour)
	completed := git.PullRequestStatus("completed")
	p := git.GitPullRequest{
		PullRequestId:         ptr(17),
		Title:                 ptr("Title"),
		Status:                &completed,
		CreationDate:          &azuredevops.Time{Time: created},
		ClosedDate:            &azuredevops.Time{Time: closed},
		CreatedBy:             &webapi.IdentityRef{Id: ptr("ABCD-1"), DisplayName: ptr("Ann"), UniqueName: ptr("ann@example.test")},
		LastMergeSourceCommit: &git.GitCommitRef{CommitId: ptr("ABCDEF0123456789ABCDEF0123456789ABCDEF01")},
		LastMergeTargetCommit: &git.GitCommitRef{CommitId: ptr("1111111111111111111111111111111111111111")},
		SourceRefName:         ptr("refs/heads/f"),
		TargetRefName:         ptr("refs/heads/main"),
		IsDraft:               ptr(false),
	}
	pr, err := ConvertPullRequest("DF164785-6F11-4E78-BCB1-AB75BA9677F0", p)
	if err != nil {
		t.Fatal(err)
	}
	if pr.RepositoryID != "df164785-6f11-4e78-bcb1-ab75ba9677f0" || pr.ID != 17 || pr.Status != "completed" {
		t.Fatalf("pr %+v", pr)
	}
	if pr.CompletedAt == nil || !pr.CompletedAt.Equal(closed) || pr.CreatedAt.Location() != time.UTC {
		t.Fatalf("completion %+v", pr)
	}
	if pr.LastMergeSourceSHA != "abcdef0123456789abcdef0123456789abcdef01" {
		t.Fatalf("sha not lowercased: %s", pr.LastMergeSourceSHA)
	}
	if pr.Creator.ID != "abcd-1" || pr.Creator.DisplayName != "Ann" || pr.Creator.AliasEmail() != "ann@example.test" {
		t.Fatalf("creator %+v", pr.Creator)
	}

	active := git.PullRequestStatus("active")
	p.Status = &active
	p.ClosedDate = &azuredevops.Time{Time: time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)}
	p.CreatedBy = nil
	pr, err = ConvertPullRequest("r", p)
	if err != nil {
		t.Fatal(err)
	}
	if pr.CompletedAt != nil || pr.ClosedAt != nil || pr.Creator.ID != "unknown" {
		t.Fatalf("active pr %+v", pr)
	}
	abandoned := git.PullRequestStatus("abandoned")
	p.Status = &abandoned
	p.ClosedDate = &azuredevops.Time{Time: closed}
	pr, _ = ConvertPullRequest("r", p)
	if pr.ClosedAt == nil || pr.CompletedAt != nil {
		t.Fatalf("abandoned must close without completing: %+v", pr)
	}
	p.PullRequestId = nil
	if _, err := ConvertPullRequest("r", p); err == nil {
		t.Fatal("missing id accepted")
	}
	if !strings.Contains(fmt.Sprint(wiqlString("o'brien")), "o''brien") {
		t.Fatal("wiql escaping")
	}
}
