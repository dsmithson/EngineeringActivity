package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/dsmithson/EngineeringActivity/internal/activity"
	"github.com/dsmithson/EngineeringActivity/internal/store"
)

//go:embed static/*
var assets embed.FS

// Reader is the read-only data access the web process needs.
type Reader interface {
	Ping(ctx context.Context) error
	Activity(ctx context.Context, q store.ActivityQuery) ([]store.Bucket, error)
	ActiveIdentities(ctx context.Context, from, to time.Time) ([]store.IdentitySummary, error)
	SourceStatuses(ctx context.Context) ([]store.SourceStatus, error)
	Groups(ctx context.Context) ([]store.Group, error)
	Repositories(ctx context.Context) ([]store.RepositorySummary, error)
	Dataset(ctx context.Context) (string, error)
	ValidTimeZone(ctx context.Context, tz string) bool
	Metrics(ctx context.Context) (store.MetricsSnapshot, error)
	WorkAreas(ctx context.Context) ([]store.WorkArea, error)
	AreaActivity(ctx context.Context, q store.ActivityQuery) (map[time.Time]map[string]store.AreaValue, error)
}

type Server struct {
	DB                 Reader
	Demo               bool
	Username, Password string
	Log                *slog.Logger
	Requests           *RequestMetrics
	Now                func() time.Time
	Branding           Branding
}

// Branding is the display text and outbound link shown in the page header. It is
// presentation only; collection scope comes from the collector's own configuration.
type Branding struct {
	Organization string `json:"organization"`
	Team         string `json:"team"`
	Scope        string `json:"scope"`
	LinkLabel    string `json:"linkLabel,omitempty"`
	LinkURL      string `json:"linkUrl,omitempty"`
}

// DefaultBranding is used for any field the deployment leaves empty.
var DefaultBranding = Branding{Organization: "Engineering", Team: "Activity", Scope: "Your repositories"}

func (b Branding) withDefaults() Branding {
	if b.Organization == "" {
		b.Organization = DefaultBranding.Organization
	}
	if b.Team == "" {
		b.Team = DefaultBranding.Team
	}
	if b.Scope == "" {
		b.Scope = DefaultBranding.Scope
	}
	// Only http(s) links are rendered, so a misconfigured value can't inject a script URL.
	if !strings.HasPrefix(b.LinkURL, "https://") && !strings.HasPrefix(b.LinkURL, "http://") {
		b.LinkURL, b.LinkLabel = "", ""
	}
	if b.LinkURL != "" && b.LinkLabel == "" {
		b.LinkLabel = "Open repository"
	}
	return b
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) Handler() http.Handler {
	if s.Requests == nil {
		s.Requests = NewRequestMetrics()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.DB != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if s.DB.Ping(ctx) != nil {
				http.Error(w, "database unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /api/activity", s.activity) // pre-v1 alias kept for bookmarked clients
	mux.HandleFunc("GET /api/v1/activity", s.activity)
	mux.HandleFunc("GET /api/v1/identities", s.identities)
	mux.HandleFunc("GET /api/v1/status", s.status)
	mux.HandleFunc("GET /api/v1/groups", s.groups)
	mux.HandleFunc("GET /api/v1/repositories", s.repositories)
	mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.Branding.withDefaults()) })
	sub, _ := fs.Sub(assets, "static")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	return s.Requests.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" && s.Username != "" {
			u, p, ok := r.BasicAuth()
			uh, ph := sha256.Sum256([]byte(u)), sha256.Sum256([]byte(p))
			wantU, wantP := sha256.Sum256([]byte(s.Username)), sha256.Sum256([]byte(s.Password))
			if !ok || subtle.ConstantTimeCompare(uh[:], wantU[:]) != 1 || subtle.ConstantTimeCompare(ph[:], wantP[:]) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="Engineering activity", charset="UTF-8"`)
				http.Error(w, "Authentication required", http.StatusUnauthorized)
				return
			}
		}
		if len(r.URL.RawQuery) > 2048 {
			http.Error(w, "query too large", http.StatusBadRequest)
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, message string, err error) {
	if err != nil && s.Log != nil {
		s.Log.Error("request failed", "path", r.URL.Path, "status", status)
	}
	http.Error(w, message, status)
}

// ActivityResponse is the /api/v1/activity contract. Buckets cover [start, end) with no
// gaps; bucket timestamps are UTC instants of bucket starts in the requested timezone.
type ActivityResponse struct {
	Start             time.Time               `json:"start"`
	End               time.Time               `json:"end"`
	Bucket            string                  `json:"bucket"`
	TimeZone          string                  `json:"timezone"`
	PartialLastBucket bool                    `json:"partialLastBucket"`
	PartialLastHour   bool                    `json:"partialLastHour"`
	CollectedThrough  *time.Time              `json:"collectedThrough"`
	CollectedFrom     *time.Time              `json:"collectedFrom"`
	Range             string                  `json:"range,omitempty"`
	Dataset           string                  `json:"dataset"`
	Demo              bool                    `json:"demo"`
	Buckets           any                     `json:"buckets"`
	Identities        []store.IdentitySummary `json:"identities"`
	Sources           []store.SourceStatus    `json:"sources"`
	Definitions       map[string]string       `json:"definitions"`
	WorkAreas         []store.WorkArea        `json:"workAreas"`
}

var definitions = map[string]string{
	"commits":         "Unique non-merge commits across all branches, at committer time, attributed to the Git author mapped to an Azure identity",
	"added/deleted":   "Text line additions/deletions; binary files count as files but have no line counts (see commitUnknown/prUnknown)",
	"prCompleted":     "Pull requests completed, attributed to the PR creator; PR lines/files/commits are the completion snapshot (merge base to source)",
	"taskCompleted":   "Entries into the completion states from outside them, attributed to the assignee in that revision; reopened tasks can complete again",
	"aiMarkedCommits": "Commits whose message carries an AI co-author trailer or tool banner. A lower bound: many people strip these markers",
}

// maxBuckets bounds query cost per bucket unit.
var maxBuckets = map[string]int{"hour": 24 * 93, "day": 3 * 370, "week": 6 * 53, "month": 10 * 12}

var safeID = regexp.MustCompile(`^[A-Za-z0-9:@._+-]{1,200}$`)
var safeGroup = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func parseInstant(v string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func approxStep(unit string) time.Duration {
	switch unit {
	case "hour":
		return time.Hour
	case "day":
		return 24 * time.Hour
	case "week":
		return 7 * 24 * time.Hour
	}
	return 28 * 24 * time.Hour
}

func (s *Server) parseActivity(r *http.Request) (store.ActivityQuery, string) {
	v := r.URL.Query()
	now := s.now().UTC()
	q := store.ActivityQuery{
		Unit:         strings.ToLower(v.Get("bucket")),
		TimeZone:     v.Get("tz"),
		IdentityID:   v.Get("user"),
		Group:        v.Get("group"),
		RepositoryID: strings.ToLower(v.Get("repo")),
		To:           now.Truncate(time.Hour).Add(time.Hour),
	}
	if q.Unit == "" {
		q.Unit = "hour"
	}
	if q.TimeZone == "" {
		q.TimeZone = "UTC"
	}
	if _, ok := maxBuckets[q.Unit]; !ok {
		return q, "bucket must be hour, day, week, or month"
	}
	if q.IdentityID != "" && !safeID.MatchString(q.IdentityID) {
		return q, "invalid user"
	}
	if q.Group != "" && !safeGroup.MatchString(q.Group) {
		return q, "invalid group"
	}
	if q.RepositoryID != "" && !uuidPattern.MatchString(q.RepositoryID) {
		return q, "invalid repo"
	}
	if t := v.Get("to"); t != "" {
		parsed, ok := parseInstant(t)
		if !ok {
			return q, "invalid to"
		}
		q.To = parsed
	}
	q.From = q.To.Add(-168 * time.Hour)
	if f := v.Get("from"); f != "" {
		parsed, ok := parseInstant(f)
		if !ok {
			return q, "invalid from"
		}
		q.From = parsed
	}
	if rng := v.Get("range"); rng != "" {
		if problem := applyRange(&q, rng, now); problem != "" {
			return q, problem
		}
	}
	if !q.From.Before(q.To) {
		return q, "from must be before to"
	}
	if int(q.To.Sub(q.From)/approxStep(q.Unit)) > maxBuckets[q.Unit] {
		return q, "range too large for bucket size"
	}
	return q, ""
}

// Ranges offered by the dashboard. Each maps to a bucket size; day and week buckets are
// aligned to midnight / Monday in the requested display timezone, matching PostgreSQL's
// date_trunc so the generated series and the aggregated rows line up.
var ranges = map[string]struct {
	unit  string
	count int
}{
	"7d":  {"hour", 168},
	"30d": {"day", 30},
	"12m": {"week", 52},
	"24m": {"week", 104},
}

func applyRange(q *store.ActivityQuery, rng string, now time.Time) string {
	spec, ok := ranges[rng]
	if !ok {
		return "range must be 7d, 30d, 12m, or 24m"
	}
	loc, err := time.LoadLocation(q.TimeZone)
	if err != nil {
		return "invalid tz"
	}
	q.Unit = spec.unit
	local := now.In(loc)
	switch spec.unit {
	case "hour":
		q.To = now.Truncate(time.Hour).Add(time.Hour)
		q.From = q.To.Add(-time.Duration(spec.count) * time.Hour)
	case "day":
		midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
		q.To = midnight.AddDate(0, 0, 1).UTC()
		q.From = midnight.AddDate(0, 0, 1-spec.count).UTC()
	case "week":
		midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
		monday := midnight.AddDate(0, 0, -((int(local.Weekday()) + 6) % 7))
		q.To = monday.AddDate(0, 0, 7).UTC()
		q.From = monday.AddDate(0, 0, 7-7*spec.count).UTC()
	}
	return ""
}

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	q, problem := s.parseActivity(r)
	if problem != "" {
		http.Error(w, problem, http.StatusBadRequest)
		return
	}
	now := s.now().UTC()
	resp := ActivityResponse{Start: q.From, End: q.To, Bucket: q.Unit, TimeZone: q.TimeZone, Definitions: definitions,
		PartialLastBucket: q.To.After(now), Sources: []store.SourceStatus{}, WorkAreas: []store.WorkArea{}}
	resp.PartialLastHour = resp.PartialLastBucket
	if _, ok := ranges[r.URL.Query().Get("range")]; ok {
		resp.Range = r.URL.Query().Get("range")
	}
	if s.Demo {
		// Labeled in-memory sample for previews without a database: last 168 hours only.
		result := activity.Aggregate(activity.Demo(now), now, q.IdentityID)
		resp.Start, resp.End, resp.Bucket, resp.TimeZone = result.Start, result.Start.Add(168*time.Hour), "hour", "UTC"
		resp.Buckets, resp.Demo, resp.Dataset = result.Buckets, true, "demo"
		resp.PartialLastBucket, resp.PartialLastHour = true, true
		for _, i := range result.Identities {
			resp.Identities = append(resp.Identities, store.IdentitySummary{ID: i.ID, Name: i.Name, Kind: "fixture", Groups: []string{}})
		}
		writeJSON(w, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if !s.DB.ValidTimeZone(ctx, q.TimeZone) {
		http.Error(w, "invalid tz", http.StatusBadRequest)
		return
	}
	buckets, err := s.DB.Activity(ctx, q)
	if err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Unable to read activity", err)
		return
	}
	if resp.Identities, err = s.DB.ActiveIdentities(ctx, q.From, q.To); err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Unable to read identities", err)
		return
	}
	if resp.Sources, err = s.DB.SourceStatuses(ctx); err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Unable to read collection status", err)
		return
	}
	if resp.Dataset, err = s.DB.Dataset(ctx); err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Unable to read dataset", err)
		return
	}
	for _, src := range resp.Sources {
		if src.CoverageStart != nil && (resp.CollectedFrom == nil || src.CoverageStart.After(*resp.CollectedFrom)) {
			cs := *src.CoverageStart
			resp.CollectedFrom = &cs
		}
		if src.Watermark != nil && (resp.CollectedThrough == nil || src.Watermark.Before(*resp.CollectedThrough)) {
			wm := *src.Watermark
			resp.CollectedThrough = &wm
		}
	}
	if resp.WorkAreas, err = s.DB.WorkAreas(ctx); err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Unable to read work areas", err)
		return
	}
	areas, err := s.DB.AreaActivity(ctx, q)
	if err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Unable to read work-area activity", err)
		return
	}
	for i := range buckets {
		buckets[i].Areas = areas[buckets[i].At]
	}
	resp.Buckets = buckets
	writeJSON(w, resp)
}

func (s *Server) identities(w http.ResponseWriter, r *http.Request) {
	q, problem := s.parseActivity(r)
	if problem != "" {
		http.Error(w, problem, http.StatusBadRequest)
		return
	}
	if s.Demo {
		writeJSON(w, map[string]any{"identities": []any{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ids, err := s.DB.ActiveIdentities(ctx, q.From, q.To)
	if err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Unable to read identities", err)
		return
	}
	writeJSON(w, map[string]any{"start": q.From, "end": q.To, "identities": ids})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if s.Demo {
		writeJSON(w, map[string]any{"dataset": "demo", "sources": []any{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	sources, err := s.DB.SourceStatuses(ctx)
	if err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Unable to read collection status", err)
		return
	}
	dataset, err := s.DB.Dataset(ctx)
	if err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Unable to read dataset", err)
		return
	}
	writeJSON(w, map[string]any{"dataset": dataset, "sources": sources})
}

func (s *Server) groups(w http.ResponseWriter, r *http.Request) {
	if s.Demo {
		writeJSON(w, map[string]any{"groups": []any{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	groups, err := s.DB.Groups(ctx)
	if err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Unable to read groups", err)
		return
	}
	writeJSON(w, map[string]any{"groups": groups})
}

func (s *Server) repositories(w http.ResponseWriter, r *http.Request) {
	if s.Demo {
		writeJSON(w, map[string]any{"repositories": []any{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	repos, err := s.DB.Repositories(ctx)
	if err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Unable to read repositories", err)
		return
	}
	writeJSON(w, map[string]any{"repositories": repos})
}
