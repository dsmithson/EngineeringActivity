package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dsmithson/EngineeringActivity/internal/activity"
	"github.com/dsmithson/EngineeringActivity/internal/store"
)

func TestDemoAndAuthentication(t *testing.T) {
	s := Server{Demo: true, Username: "office", Password: "test-only"}
	h := s.Handler()
	for _, path := range []string{"/api/activity", "/api/v1/activity"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("%s unauthenticated: %d", path, w.Code)
		}
		r.SetBasicAuth("office", "test-only")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		var result activity.Result
		if e := json.Unmarshal(w.Body.Bytes(), &result); e != nil {
			t.Fatal(e)
		}
		if !result.Demo || len(result.Buckets) != 168 {
			t.Fatal("demo contract")
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("sensitive results should not be cached")
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 200 {
		t.Fatal("health endpoint must not require login")
	}
}

type fakeReader struct {
	err        error
	metricsErr error
	lastQuery  store.ActivityQuery
	sources    []store.SourceStatus
	snapshot   store.MetricsSnapshot
}

func (f *fakeReader) Ping(context.Context) error { return f.err }
func (f *fakeReader) Activity(_ context.Context, q store.ActivityQuery) ([]store.Bucket, error) {
	f.lastQuery = q
	if f.err != nil {
		return nil, f.err
	}
	return []store.Bucket{{At: q.From, Commits: 2}}, nil
}
func (f *fakeReader) ActiveIdentities(context.Context, time.Time, time.Time) ([]store.IdentitySummary, error) {
	return []store.IdentitySummary{{ID: "a", Name: "Ann", Kind: "ado", Groups: []string{}}}, f.err
}
func (f *fakeReader) SourceStatuses(context.Context) ([]store.SourceStatus, error) {
	return f.sources, f.err
}
func (f *fakeReader) Groups(context.Context) ([]store.Group, error) { return nil, f.err }
func (f *fakeReader) Repositories(context.Context) ([]store.RepositorySummary, error) {
	return nil, f.err
}
func (f *fakeReader) Dataset(context.Context) (string, error) { return "fixture", f.err }
func (f *fakeReader) WorkAreas(context.Context) ([]store.WorkArea, error) {
	return []store.WorkArea{{Area: "gui", Label: "GUI software", Kind: "software", Color: "#3987e5"}}, f.err
}
func (f *fakeReader) AreaActivity(_ context.Context, q store.ActivityQuery) (map[time.Time]map[string]store.AreaValue, error) {
	return map[time.Time]map[string]store.AreaValue{q.From: {"gui": {Lines: 7, Commits: 1, Files: 2}}}, f.err
}
func (f *fakeReader) ValidTimeZone(_ context.Context, tz string) bool {
	return tz == "UTC" || tz == "America/Phoenix"
}
func (f *fakeReader) Metrics(context.Context) (store.MetricsSnapshot, error) {
	return f.snapshot, f.metricsErr
}

var fixedNow = time.Date(2026, 10, 15, 13, 25, 0, 0, time.UTC)

func newServer(r *fakeReader) *Server {
	return &Server{DB: r, Now: func() time.Time { return fixedNow }}
}

func do(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestActivityValidation(t *testing.T) {
	h := newServer(&fakeReader{}).Handler()
	for _, path := range []string{
		"/api/v1/activity?bucket=minute",
		"/api/v1/activity?tz=Mars/Base",
		"/api/v1/activity?from=2020-01-01",
		"/api/v1/activity?from=2026-10-10&to=2026-10-10",
		"/api/v1/activity?from=2026-10-11&to=2026-10-10",
		"/api/v1/activity?repo=not-a-uuid",
		"/api/v1/activity?group=Bad%20Group",
		"/api/v1/activity?user=%27%3Bdrop",
		"/api/v1/activity?from=yesterday",
	} {
		if w := do(h, path); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	if w := do(h, "/api/v1/activity?from=2025-10-01&to=2026-10-01&bucket=day&tz=America/Phoenix"); w.Code != 200 {
		t.Fatalf("one-year daily range rejected: %d %s", w.Code, w.Body.String())
	}
}

func TestActivityDefaultsAndCoverage(t *testing.T) {
	early := fixedNow.Truncate(time.Hour).Add(-3 * time.Hour)
	late := fixedNow.Truncate(time.Hour)
	r := &fakeReader{sources: []store.SourceStatus{
		{Source: "commits", Scope: "x", Watermark: &late},
		{Source: "tasks", Scope: "p", Watermark: &early},
		{Source: "pull_requests", Scope: "x"},
	}}
	w := do(newServer(r).Handler(), "/api/v1/activity")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	q := r.lastQuery
	wantTo := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	if q.Unit != "hour" || q.TimeZone != "UTC" || !q.To.Equal(wantTo) || q.To.Sub(q.From) != 168*time.Hour {
		t.Fatalf("defaults %+v", q)
	}
	var resp ActivityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.CollectedThrough == nil || !resp.CollectedThrough.Equal(early) {
		t.Fatalf("collectedThrough %v", resp.CollectedThrough)
	}
	if len(resp.WorkAreas) != 1 {
		t.Fatalf("work areas %+v", resp.WorkAreas)
	}
	if resp.Dataset != "fixture" || resp.Demo || !resp.PartialLastBucket || len(resp.Identities) != 1 || len(resp.Sources) != 3 {
		t.Fatalf("response %+v", resp)
	}
	do(newServer(r).Handler(), "/api/v1/activity?user=git:a@b.test&group=fpga&repo=df164785-6f11-4e78-bcb1-ab75ba9677f0&bucket=week&tz=America/Phoenix")
	if r.lastQuery.IdentityID != "git:a@b.test" || r.lastQuery.Group != "fpga" || r.lastQuery.Unit != "week" || r.lastQuery.TimeZone != "America/Phoenix" {
		t.Fatalf("filters not passed: %+v", r.lastQuery)
	}
}

func TestReaderErrorsAreNotLeaked(t *testing.T) {
	h := newServer(&fakeReader{err: errors.New("pq: password authentication failed for secret-host")}).Handler()
	for _, path := range []string{"/api/v1/activity", "/api/v1/identities", "/api/v1/status", "/api/v1/groups", "/api/v1/repositories", "/readyz"} {
		w := do(h, path)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: %d", path, w.Code)
		}
		if strings.Contains(w.Body.String(), "secret-host") || strings.Contains(w.Body.String(), "pq:") {
			t.Errorf("%s leaked error: %s", path, w.Body.String())
		}
	}
}

func sampleSnapshot() store.MetricsSnapshot {
	wm := fixedNow.Truncate(time.Hour)
	return store.MetricsSnapshot{
		Sources: []store.SourceMetrics{{
			SourceStatus: store.SourceStatus{Source: "commits", Scope: "df164785-6f11-4e78-bcb1-ab75ba9677f0", ScopeName: "Widgets", Watermark: &wm, LastSuccess: &wm, LastAttempt: &wm, Outcome: "success"},
			LastRun:      &store.RunTotals{Fetched: 12, Inserted: 3, Unchanged: 9, DurationSeconds: 1.5, Outcome: "success"},
			Totals:       map[string]int64{"success": 4, "failed": 1},
			Retries:      2, Throttles: 1,
		}},
		Runs: map[string]int64{"success": 4, "partial": 1},
		Last: &store.LastRun{Mode: "scheduled", Outcome: "success", StartedAt: wm, DurationSeconds: 9},
	}
}

var labelPattern = regexp.MustCompile(`([a-z_]+)="`)

func TestMetricsExporter(t *testing.T) {
	s := newServer(&fakeReader{snapshot: sampleSnapshot()})
	app := s.Handler()
	do(app, "/api/v1/activity")
	do(app, "/no/such/path")
	do(app, "/api/whatever")
	do(app, "/api/v1/unknown")
	body := do(s.MetricsHandler(), "/metrics").Body.String()
	for _, want := range []string{
		"activity_exporter_up 1",
		"activity_database_up 1",
		`activity_sync_watermark_timestamp_seconds{source="commits",scope="Widgets"} ` + strconv.FormatInt(fixedNow.Truncate(time.Hour).Unix(), 10) + ".000",
		`activity_sync_lag_seconds{source="commits",scope="Widgets"} 1500`,
		`activity_last_run_fetched{source="commits",scope="Widgets"} 12`,
		`activity_source_runs_total{source="commits",scope="Widgets",outcome="failed"} 1`,
		`activity_source_retries_total{source="commits",scope="Widgets"} 2`,
		`activity_collection_runs_total{outcome="partial"} 1`,
		`activity_collection_last_run_outcome{outcome="success"} 1`,
		`activity_web_requests_total{route="activity",method="GET",code="2xx"} 1`,
		`activity_web_requests_total{route="static",method="GET",code="4xx"} 1`,
		`route="api_other"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	allowed := map[string]bool{"source": true, "scope": true, "outcome": true, "route": true, "method": true, "code": true, "le": true}
	for _, m := range labelPattern.FindAllStringSubmatch(body, -1) {
		if !allowed[m[1]] {
			t.Errorf("unbounded label %q", m[1])
		}
	}
	if strings.Contains(body, "df164785") {
		t.Error("metrics expose repository ids instead of names")
	}
	if strings.Contains(body, "/no/such/path") || strings.Contains(body, "unknown\"") {
		t.Error("raw paths used as labels")
	}
}

func TestMetricsExporterDown(t *testing.T) {
	down := newServer(&fakeReader{metricsErr: errors.New("db down")})
	body := do(down.MetricsHandler(), "/metrics").Body.String()
	if !strings.Contains(body, "activity_exporter_up 0") || strings.Contains(body, "activity_sync_") || strings.Contains(body, "db down") {
		t.Fatalf("db failure must report exporter_up 0 without collector series:\n%s", body)
	}
	demo := &Server{Demo: true}
	if body := do(demo.MetricsHandler(), "/metrics").Body.String(); !strings.Contains(body, "activity_exporter_up 0") {
		t.Fatal("demo mode must not claim collector metrics")
	}
}

func TestBrandingConfig(t *testing.T) {
	read := func(b Branding) Branding {
		s := newServer(&fakeReader{})
		s.Branding = b
		w := do(s.Handler(), "/api/v1/config")
		if w.Code != http.StatusOK {
			t.Fatalf("status %d", w.Code)
		}
		var got Branding
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	s := newServer(&fakeReader{})
	s.Version = "1.2.3"
	if w := do(s.Handler(), "/api/v1/config"); !strings.Contains(w.Body.String(), `"version":"1.2.3"`) {
		t.Errorf("config missing version: %s", w.Body.String())
	}
	if got := read(Branding{}); got != DefaultBranding {
		t.Errorf("defaults = %+v, want %+v", got, DefaultBranding)
	}
	got := read(Branding{Organization: "Acme", LinkURL: "https://dev.azure.com/acme/Platform/_git/Widgets"})
	if got.Organization != "Acme" || got.Team != DefaultBranding.Team || got.LinkLabel != "Open repository" {
		t.Errorf("partial branding = %+v", got)
	}
	// Non-http links are dropped rather than rendered.
	if got := read(Branding{LinkURL: "javascript:alert(1)", LinkLabel: "x"}); got.LinkURL != "" || got.LinkLabel != "" {
		t.Errorf("unsafe link kept: %+v", got)
	}
}

func TestRoute(t *testing.T) {
	for path, want := range map[string]string{
		"/healthz": "health", "/readyz": "health", "/api/activity": "activity", "/api/v1/activity": "activity",
		"/api/v1/status": "status", "/api/v1/groups": "groups", "/api/v1/config": "config", "/api/v1/x": "api_other", "/api/x": "api_other",
		"/": "static", "/app.js": "static", "/random/123": "static",
	} {
		if got := route(path); got != want {
			t.Errorf("route(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestActivityRanges(t *testing.T) {
	r := &fakeReader{}
	h := newServer(r).Handler()
	// fixedNow is a Thursday; Phoenix is UTC-7 with no DST.
	phoenix, _ := time.LoadLocation("America/Phoenix")
	local := fixedNow.In(phoenix)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, phoenix)
	monday := midnight.AddDate(0, 0, -((int(local.Weekday()) + 6) % 7))
	for _, tc := range []struct {
		rng, unit string
		from, to  time.Time
		buckets   int
	}{
		{"7d", "hour", fixedNow.Truncate(time.Hour).Add(-167 * time.Hour), fixedNow.Truncate(time.Hour).Add(time.Hour), 168},
		{"30d", "day", midnight.AddDate(0, 0, -29), midnight.AddDate(0, 0, 1), 30},
		{"12m", "week", monday.AddDate(0, 0, -51*7), monday.AddDate(0, 0, 7), 52},
		{"24m", "week", monday.AddDate(0, 0, -103*7), monday.AddDate(0, 0, 7), 104},
	} {
		w := do(h, "/api/v1/activity?range="+tc.rng+"&tz=America/Phoenix&group=fpga")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", tc.rng, w.Code, w.Body.String())
		}
		q := r.lastQuery
		if q.Unit != tc.unit || !q.From.Equal(tc.from) || !q.To.Equal(tc.to) || q.Group != "fpga" {
			t.Errorf("%s: got %s %v..%v want %s %v..%v", tc.rng, q.Unit, q.From, q.To, tc.unit, tc.from, tc.to)
		}
		if tc.unit == "week" && q.From.In(phoenix).Weekday() != time.Monday {
			t.Errorf("%s: weeks must start on Monday", tc.rng)
		}
		var resp ActivityResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Range != tc.rng || resp.Bucket != tc.unit {
			t.Errorf("%s: response range/bucket %q %q %v", tc.rng, resp.Range, resp.Bucket, err)
		}
	}
	if w := do(h, "/api/v1/activity?range=5y"); w.Code != 400 {
		t.Fatalf("unknown range accepted: %d", w.Code)
	}
	// Day boundaries follow DST zones too: 30 local days ending at local midnight.
	do(h, "/api/v1/activity?range=30d&tz=UTC")
	if r.lastQuery.To.Hour() != 0 || r.lastQuery.To.Sub(r.lastQuery.From) != 30*24*time.Hour {
		t.Fatalf("utc days %+v", r.lastQuery)
	}
}

func TestCollectedFromIsLatestCoverageStart(t *testing.T) {
	a, b := fixedNow.AddDate(-2, 0, 0), fixedNow.AddDate(-1, 0, 0)
	r := &fakeReader{sources: []store.SourceStatus{{Source: "commits", CoverageStart: &a}, {Source: "tasks", CoverageStart: &b}}}
	var resp ActivityResponse
	_ = json.Unmarshal(do(newServer(r).Handler(), "/api/v1/activity?range=24m&tz=UTC").Body.Bytes(), &resp)
	if resp.CollectedFrom == nil || !resp.CollectedFrom.Equal(b) {
		t.Fatalf("collectedFrom %v", resp.CollectedFrom)
	}
}
