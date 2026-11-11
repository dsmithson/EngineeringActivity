package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dsmithson/EngineeringActivity/internal/store"
)

// RequestMetrics counts web requests by route class and status class. Labels are bounded:
// paths are reduced to a fixed set of routes.
type RequestMetrics struct {
	mu       sync.Mutex
	counts   map[[3]string]int64 // route, method, code class
	duration map[string]*histogram
}

var durationBuckets = []float64{0.005, 0.025, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type histogram struct {
	counts []int64
	sum    float64
	total  int64
}

func NewRequestMetrics() *RequestMetrics {
	return &RequestMetrics{counts: map[[3]string]int64{}, duration: map[string]*histogram{}}
}

func route(path string) string {
	switch {
	case path == "/healthz" || path == "/readyz":
		return "health"
	case path == "/api/activity" || path == "/api/v1/activity":
		return "activity"
	case strings.HasPrefix(path, "/api/v1/"):
		name := strings.TrimPrefix(path, "/api/v1/")
		switch name {
		case "identities", "status", "groups", "repositories", "config":
			return name
		}
		return "api_other"
	case strings.HasPrefix(path, "/api/"):
		return "api_other"
	}
	return "static"
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Wrap records metrics for every request handled by next.
func (m *RequestMetrics) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		rt := route(r.URL.Path)
		method := r.Method
		if method != http.MethodGet && method != http.MethodHead {
			method = "other"
		}
		elapsed := time.Since(start).Seconds()
		m.mu.Lock()
		m.counts[[3]string{rt, method, strconv.Itoa(sw.status/100) + "xx"}]++
		h := m.duration[rt]
		if h == nil {
			h = &histogram{counts: make([]int64, len(durationBuckets))}
			m.duration[rt] = h
		}
		for i, b := range durationBuckets {
			if elapsed <= b {
				h.counts[i]++
			}
		}
		h.sum += elapsed
		h.total++
		m.mu.Unlock()
	})
}

func (m *RequestMetrics) write(b *strings.Builder) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b.WriteString("# HELP activity_web_requests_total Web requests by route, method and status class.\n# TYPE activity_web_requests_total counter\n")
	keys := make([][3]string, 0, len(m.counts))
	for k := range m.counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return strings.Join(keys[i][:], ",") < strings.Join(keys[j][:], ",") })
	for _, k := range keys {
		fmt.Fprintf(b, "activity_web_requests_total{route=%q,method=%q,code=%q} %d\n", k[0], k[1], k[2], m.counts[k])
	}
	b.WriteString("# HELP activity_web_request_duration_seconds Web request latency.\n# TYPE activity_web_request_duration_seconds histogram\n")
	routes := make([]string, 0, len(m.duration))
	for r := range m.duration {
		routes = append(routes, r)
	}
	sort.Strings(routes)
	for _, r := range routes {
		h := m.duration[r]
		for i, le := range durationBuckets {
			fmt.Fprintf(b, "activity_web_request_duration_seconds_bucket{route=%q,le=%q} %d\n", r, strconv.FormatFloat(le, 'g', -1, 64), h.counts[i])
		}
		fmt.Fprintf(b, "activity_web_request_duration_seconds_bucket{route=%q,le=\"+Inf\"} %d\n", r, h.total)
		fmt.Fprintf(b, "activity_web_request_duration_seconds_sum{route=%q} %g\n", r, h.sum)
		fmt.Fprintf(b, "activity_web_request_duration_seconds_count{route=%q} %d\n", r, h.total)
	}
}

func epoch(t *time.Time) string {
	if t == nil {
		return "NaN"
	}
	return strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', 3, 64)
}

// MetricsHandler serves Prometheus text format. Collector values come from summaries the
// collector persisted in PostgreSQL, so they survive the batch job exiting. When the
// database is unavailable, activity_exporter_up is 0 and collector series are omitted
// rather than reported as zeros. Labels never include people, PRs, SHAs or run ids.
func (s *Server) MetricsHandler() http.Handler {
	if s.Requests == nil {
		s.Requests = NewRequestMetrics()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		s.Requests.write(&b)
		up, dbUp := 1, 0
		var body strings.Builder
		if s.Demo || s.DB == nil {
			up = 0
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			if s.DB.Ping(ctx) == nil {
				dbUp = 1
			}
			snap, err := s.DB.Metrics(ctx)
			if err != nil {
				up = 0
			} else {
				writeCollector(&body, snap, s.now())
			}
		}
		b.WriteString("# HELP activity_exporter_up Whether collector metrics could be read from PostgreSQL.\n# TYPE activity_exporter_up gauge\n")
		fmt.Fprintf(&b, "activity_exporter_up %d\n", up)
		b.WriteString("# HELP activity_database_up Whether the web process can reach PostgreSQL.\n# TYPE activity_database_up gauge\n")
		fmt.Fprintf(&b, "activity_database_up %d\n", dbUp)
		b.WriteString(body.String())
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	})
}

func writeCollector(b *strings.Builder, snap MetricsSnapshotView, now time.Time) {
	gauge := func(name, help string) {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}
	counter := func(name, help string) {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	}
	labels := func(source, scope string) string {
		return fmt.Sprintf("source=%q,scope=%q", source, scope)
	}
	gauge("activity_sync_last_success_timestamp_seconds", "Unix time of the last successful sync per source.")
	for _, s := range snap.Sources {
		fmt.Fprintf(b, "activity_sync_last_success_timestamp_seconds{%s} %s\n", labels(s.Source, s.ScopeName), epoch(s.LastSuccess))
	}
	gauge("activity_sync_last_attempt_timestamp_seconds", "Unix time of the last attempted sync per source.")
	for _, s := range snap.Sources {
		fmt.Fprintf(b, "activity_sync_last_attempt_timestamp_seconds{%s} %s\n", labels(s.Source, s.ScopeName), epoch(s.LastAttempt))
	}
	gauge("activity_sync_watermark_timestamp_seconds", "Facts are complete up to this Unix time per source.")
	for _, s := range snap.Sources {
		fmt.Fprintf(b, "activity_sync_watermark_timestamp_seconds{%s} %s\n", labels(s.Source, s.ScopeName), epoch(s.Watermark))
	}
	gauge("activity_sync_coverage_start_timestamp_seconds", "Earliest Unix time covered by backfill per source.")
	for _, s := range snap.Sources {
		fmt.Fprintf(b, "activity_sync_coverage_start_timestamp_seconds{%s} %s\n", labels(s.Source, s.ScopeName), epoch(s.CoverageStart))
	}
	gauge("activity_sync_lag_seconds", "Seconds between now and the source watermark.")
	for _, s := range snap.Sources {
		lag := "NaN"
		if s.Watermark != nil {
			lag = strconv.FormatFloat(now.Sub(*s.Watermark).Seconds(), 'f', 0, 64)
		}
		fmt.Fprintf(b, "activity_sync_lag_seconds{%s} %s\n", labels(s.Source, s.ScopeName), lag)
	}
	gauge("activity_sync_last_outcome", "1 for the most recent outcome of each source.")
	for _, s := range snap.Sources {
		for _, o := range []string{"success", "failed"} {
			v := 0
			if s.Outcome == o {
				v = 1
			}
			fmt.Fprintf(b, "activity_sync_last_outcome{%s,outcome=%q} %d\n", labels(s.Source, s.ScopeName), o, v)
		}
	}
	type lastField struct {
		name, help string
		get        func(*RunTotalsView) float64
	}
	for _, f := range []lastField{
		{"activity_last_run_fetched", "Records fetched in the source's last run.", func(t *RunTotalsView) float64 { return float64(t.Fetched) }},
		{"activity_last_run_inserted", "Rows inserted in the source's last run.", func(t *RunTotalsView) float64 { return float64(t.Inserted) }},
		{"activity_last_run_updated", "Rows updated in the source's last run.", func(t *RunTotalsView) float64 { return float64(t.Updated) }},
		{"activity_last_run_unchanged", "Rows unchanged in the source's last run.", func(t *RunTotalsView) float64 { return float64(t.Unchanged) }},
		{"activity_last_run_unknown_statistics", "Records with unavailable statistics in the source's last run.", func(t *RunTotalsView) float64 { return float64(t.Unknown) }},
		{"activity_last_run_duration_seconds", "Duration of the source's last run.", func(t *RunTotalsView) float64 { return t.DurationSeconds }},
	} {
		gauge(f.name, f.help)
		for _, s := range snap.Sources {
			if s.LastRun != nil {
				fmt.Fprintf(b, "%s{%s} %g\n", f.name, labels(s.Source, s.ScopeName), f.get(s.LastRun))
			}
		}
	}
	counter("activity_source_runs_total", "Persisted source runs by outcome.")
	for _, s := range snap.Sources {
		outcomes := make([]string, 0, len(s.Totals))
		for o := range s.Totals {
			outcomes = append(outcomes, o)
		}
		sort.Strings(outcomes)
		for _, o := range outcomes {
			fmt.Fprintf(b, "activity_source_runs_total{%s,outcome=%q} %d\n", labels(s.Source, s.ScopeName), o, s.Totals[o])
		}
	}
	counter("activity_source_retries_total", "Persisted Azure DevOps request retries per source.")
	for _, s := range snap.Sources {
		fmt.Fprintf(b, "activity_source_retries_total{%s} %d\n", labels(s.Source, s.ScopeName), s.Retries)
	}
	counter("activity_source_throttles_total", "Persisted Azure DevOps throttling responses per source.")
	for _, s := range snap.Sources {
		fmt.Fprintf(b, "activity_source_throttles_total{%s} %d\n", labels(s.Source, s.ScopeName), s.Throttles)
	}
	counter("activity_collection_runs_total", "Persisted collector runs by outcome.")
	for _, o := range []string{"success", "partial", "failed"} {
		fmt.Fprintf(b, "activity_collection_runs_total{outcome=%q} %d\n", o, snap.Runs[o])
	}
	if snap.Last != nil {
		gauge("activity_collection_last_run_timestamp_seconds", "Start time of the most recent finished collector run.")
		fmt.Fprintf(b, "activity_collection_last_run_timestamp_seconds %s\n", epoch(&snap.Last.StartedAt))
		gauge("activity_collection_last_run_duration_seconds", "Duration of the most recent finished collector run.")
		fmt.Fprintf(b, "activity_collection_last_run_duration_seconds %g\n", snap.Last.DurationSeconds)
		gauge("activity_collection_last_run_outcome", "1 for the outcome of the most recent finished collector run.")
		for _, o := range []string{"success", "partial", "failed"} {
			v := 0
			if snap.Last.Outcome == o {
				v = 1
			}
			fmt.Fprintf(b, "activity_collection_last_run_outcome{outcome=%q} %d\n", o, v)
		}
	}
}

type (
	MetricsSnapshotView = store.MetricsSnapshot
	RunTotalsView       = store.RunTotals
)
