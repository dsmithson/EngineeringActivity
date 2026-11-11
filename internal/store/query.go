package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Bucket is one time bucket of activity. Field names are the API contract used by the
// dashboard and CSV exports.
type Bucket struct {
	At            time.Time `json:"at"`
	PRCreated     int       `json:"prCreated"`
	PRCompleted   int       `json:"prCompleted"`
	PRAdded       int64     `json:"prAdded"`
	PRDeleted     int64     `json:"prDeleted"`
	PRFiles       int64     `json:"prFiles"`
	PRCommits     int64     `json:"prCommits"`
	PRUnknown     int       `json:"prUnknown"`
	Commits       int       `json:"commits"`
	Added         int64     `json:"added"`
	Deleted       int64     `json:"deleted"`
	Files         int64     `json:"files"`
	CommitUnknown int       `json:"commitUnknown"`
	Merges        int       `json:"merges"`
	AIMarked      int       `json:"aiMarkedCommits"`
	TaskCreated   int       `json:"taskCreated"`
	TaskCompleted int       `json:"taskCompleted"`
	// Areas is activity by work area (commits only); filled by the web layer.
	Areas map[string]AreaValue `json:"areas,omitempty"`
}

// ActivityQuery selects a half-open interval [From, To) bucketed by Unit in TimeZone.
// Filters are optional; Group selects members of a contributor group.
type ActivityQuery struct {
	From, To     time.Time
	Unit         string // hour | day | week | month
	TimeZone     string
	IdentityID   string
	Group        string
	RepositoryID string
}

var units = map[string]bool{"hour": true, "day": true, "week": true, "month": true}

func (q ActivityQuery) validate() error {
	if !units[q.Unit] {
		return fmt.Errorf("unsupported bucket %q", q.Unit)
	}
	if !q.From.Before(q.To) {
		return fmt.Errorf("from must be before to")
	}
	return nil
}

// filterCTE resolves the user ($4) / group ($5) filter to identity ids and Git emails,
// including identities merged into a selected person.
const filterCTE = `
		selected AS (
			SELECT identity_id FROM identities WHERE $4 <> '' AND identity_id = $4
			UNION SELECT $4 WHERE $4 LIKE 'git:%'
			UNION SELECT m.identity_id FROM contributor_group_members m WHERE $5 <> '' AND m.group_name = $5
		),
		-- A selected person includes every identity merged into them (identity_merges).
		filter_ids AS (
			SELECT identity_id FROM selected
			UNION SELECT im.identity_id FROM identity_merges im JOIN selected s ON s.identity_id = im.canonical_identity_id
		),
		filter_emails AS (
			SELECT a.email FROM identity_aliases a JOIN filter_ids f USING (identity_id)
			UNION SELECT substr(identity_id, 5) FROM filter_ids WHERE identity_id LIKE 'git:%'
		),
		filtering AS (SELECT ($4 <> '' OR $5 <> '') AS active)`

// Activity aggregates facts into a gap-free bucket series. Commit counts and churn exclude
// merge commits; unknown counts are events whose statistics are incomplete.
func (s *Store) Activity(ctx context.Context, q ActivityQuery) ([]Bucket, error) {
	if err := q.validate(); err != nil {
		return nil, err
	}
	// Filters resolve identities to their Git emails so commit filtering can use indexes.

	sql := `WITH ` + filterCTE + `,
		series AS (
			SELECT generate_series(date_trunc($3, $1::timestamptz, $6), $2::timestamptz - interval '1 microsecond', ('1 ' || $3)::interval, $6) AS at
		),
		c AS (
			SELECT date_trunc($3, c.committed_at, $6) AS at,
				count(*) FILTER (WHERE NOT c.is_merge) AS commits,
				count(*) FILTER (WHERE c.is_merge) AS merges,
				coalesce(sum(c.additions) FILTER (WHERE NOT c.is_merge), 0) AS added,
				coalesce(sum(c.deletions) FILTER (WHERE NOT c.is_merge), 0) AS deleted,
				coalesce(sum(c.files_changed) FILTER (WHERE NOT c.is_merge), 0) AS files,
				count(*) FILTER (WHERE NOT c.is_merge AND c.stats_status <> 'ok') AS unknown,
				count(*) FILTER (WHERE NOT c.is_merge AND cardinality(ai_tools(c.message)) > 0) AS ai
			FROM commits c, filtering
			WHERE c.committed_at >= $1 AND c.committed_at < $2
				AND ($7 = '' OR c.repository_id::text = $7)
				AND (NOT filtering.active OR c.author_email IN (SELECT email FROM filter_emails))
			GROUP BY 1
		),
		pc AS (
			SELECT date_trunc($3, p.created_at, $6) AS at, count(*) AS created
			FROM pull_requests p, filtering
			WHERE p.created_at >= $1 AND p.created_at < $2
				AND ($7 = '' OR p.repository_id::text = $7)
				AND (NOT filtering.active OR p.creator_identity_id IN (SELECT identity_id FROM filter_ids))
			GROUP BY 1
		),
		pd AS (
			SELECT date_trunc($3, p.completed_at, $6) AS at, count(*) AS completed,
				coalesce(sum(p.additions), 0) AS added, coalesce(sum(p.deletions), 0) AS deleted,
				coalesce(sum(p.files_changed), 0) AS files, coalesce(sum(p.commit_count), 0) AS commits,
				count(*) FILTER (WHERE p.stats_status <> 'ok') AS unknown
			FROM pull_requests p, filtering
			WHERE p.completed_at >= $1 AND p.completed_at < $2
				AND ($7 = '' OR p.repository_id::text = $7)
				AND (NOT filtering.active OR p.creator_identity_id IN (SELECT identity_id FROM filter_ids))
			GROUP BY 1
		),
		t AS (
			SELECT date_trunc($3, e.occurred_at, $6) AS at,
				count(*) FILTER (WHERE e.event_type = 'created') AS created,
				count(*) FILTER (WHERE e.event_type = 'completed') AS completed
			FROM task_events e, filtering
			WHERE e.occurred_at >= $1 AND e.occurred_at < $2 AND $7 = ''
				AND (NOT filtering.active OR e.identity_id IN (SELECT identity_id FROM filter_ids))
			GROUP BY 1
		)
		SELECT s.at,
			coalesce(pc.created, 0), coalesce(pd.completed, 0), coalesce(pd.added, 0), coalesce(pd.deleted, 0),
			coalesce(pd.files, 0), coalesce(pd.commits, 0), coalesce(pd.unknown, 0),
			coalesce(c.commits, 0), coalesce(c.added, 0), coalesce(c.deleted, 0), coalesce(c.files, 0),
			coalesce(c.unknown, 0), coalesce(c.merges, 0), coalesce(c.ai, 0),
			coalesce(t.created, 0), coalesce(t.completed, 0)
		FROM series s
		LEFT JOIN c USING (at) LEFT JOIN pc USING (at) LEFT JOIN pd USING (at) LEFT JOIN t USING (at)
		ORDER BY s.at`
	rows, err := s.Pool.Query(ctx, sql, q.From, q.To, q.Unit, q.IdentityID, q.Group, q.TimeZone, q.RepositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Bucket{}
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.At, &b.PRCreated, &b.PRCompleted, &b.PRAdded, &b.PRDeleted, &b.PRFiles, &b.PRCommits, &b.PRUnknown,
			&b.Commits, &b.Added, &b.Deleted, &b.Files, &b.CommitUnknown, &b.Merges, &b.AIMarked, &b.TaskCreated, &b.TaskCompleted); err != nil {
			return nil, err
		}
		b.At = b.At.UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}

type IdentitySummary struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	IsService bool     `json:"isService"`
	Groups    []string `json:"groups"`
}

// ActiveIdentities lists identities with any activity in [from, to), for the contributor
// selector. Unmapped Git authors appear as git:<email> with a "(Git)" suffix.
func (s *Store) ActiveIdentities(ctx context.Context, from, to time.Time) ([]IdentitySummary, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH raw AS (
			SELECT coalesce(a.identity_id, 'git:' || c.author_email) AS identity_id, min(c.author_name) AS git_name
			FROM commits c LEFT JOIN identity_aliases a ON a.email = c.author_email
			WHERE c.committed_at >= $1 AND c.committed_at < $2 GROUP BY 1
			UNION ALL SELECT creator_identity_id, NULL FROM pull_requests
				WHERE (created_at >= $1 AND created_at < $2) OR (completed_at >= $1 AND completed_at < $2)
			UNION ALL SELECT identity_id, NULL FROM task_events WHERE occurred_at >= $1 AND occurred_at < $2
		),
		active AS (
			SELECT coalesce(im.canonical_identity_id, r.identity_id) AS identity_id, r.git_name
			FROM raw r LEFT JOIN identity_merges im ON im.identity_id = r.identity_id
		)
		SELECT a.identity_id, coalesce(i.display_name, min(a.git_name) || ' (Git)', a.identity_id),
			coalesce(i.kind, 'git'), coalesce(i.is_service, false),
			coalesce((SELECT array_agg(m.group_name ORDER BY m.group_name) FROM contributor_group_members m WHERE m.identity_id = a.identity_id), '{}')
		FROM active a LEFT JOIN identities i USING (identity_id)
		GROUP BY a.identity_id, i.display_name, i.kind, i.is_service
		ORDER BY 2`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IdentitySummary{}
	for rows.Next() {
		var v IdentitySummary
		if err := rows.Scan(&v.ID, &v.Name, &v.Kind, &v.IsService, &v.Groups); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

type Group struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Members     int    `json:"members"`
}

func (s *Store) Groups(ctx context.Context) ([]Group, error) {
	rows, err := s.Pool.Query(ctx, `SELECT g.group_name, g.description, count(m.identity_id)
		FROM contributor_groups g LEFT JOIN contributor_group_members m USING (group_name)
		GROUP BY g.group_name, g.description ORDER BY g.group_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.Name, &g.Description, &g.Members); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

type RepositorySummary struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Project string `json:"project"`
	WebURL  string `json:"webUrl,omitempty"`
}

func (s *Store) Repositories(ctx context.Context) ([]RepositorySummary, error) {
	rows, err := s.Pool.Query(ctx, `SELECT repository_id::text, name, project, coalesce(web_url, '') FROM repositories ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RepositorySummary{}
	for rows.Next() {
		var r RepositorySummary
		if err := rows.Scan(&r.ID, &r.Name, &r.Project, &r.WebURL); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SourceStatus is the freshness of one collected source.
type SourceStatus struct {
	Source        string     `json:"source"`
	Scope         string     `json:"scope"`
	ScopeName     string     `json:"scopeName"`
	Watermark     *time.Time `json:"watermark"`
	CoverageStart *time.Time `json:"coverageStart"`
	LastSuccess   *time.Time `json:"lastSuccess"`
	LastAttempt   *time.Time `json:"lastAttempt"`
	Outcome       string     `json:"outcome"`
	ErrorClass    string     `json:"errorClass,omitempty"`
}

func (s *Store) SourceStatuses(ctx context.Context) ([]SourceStatus, error) {
	rows, err := s.Pool.Query(ctx, `SELECT k.source, k.scope, coalesce(r.name, k.scope), k.watermark, k.coverage_start,
			k.last_success_at, k.last_attempt_at, coalesce(k.last_outcome, ''), coalesce(k.last_error_class, '')
		FROM sync_checkpoints k LEFT JOIN repositories r ON r.repository_id::text = k.scope
		ORDER BY k.source, k.scope`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SourceStatus{}
	for rows.Next() {
		var v SourceStatus
		if err := rows.Scan(&v.Source, &v.Scope, &v.ScopeName, &v.Watermark, &v.CoverageStart, &v.LastSuccess, &v.LastAttempt, &v.Outcome, &v.ErrorClass); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Dataset reports "fixture" when development seed data is present, otherwise "live".
func (s *Store) Dataset(ctx context.Context) (string, error) {
	fixture, err := s.HasFixtureData(ctx)
	if err != nil {
		return "", err
	}
	if fixture {
		return "fixture", nil
	}
	return "live", nil
}

// ValidTimeZone checks a zone name against PostgreSQL's catalog.
func (s *Store) ValidTimeZone(ctx context.Context, tz string) bool {
	if tz == "" || len(tz) > 64 || strings.ContainsAny(tz, "'\\") {
		return false
	}
	var ok bool
	return s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_timezone_names WHERE name = $1)`, tz).Scan(&ok) == nil && ok
}

// MetricsSnapshot holds durable collector monitoring data for the /metrics exporter.
type MetricsSnapshot struct {
	Sources []SourceMetrics
	Runs    map[string]int64 // outcome -> total runs
	Last    *LastRun
}

type SourceMetrics struct {
	SourceStatus
	LastRun                     *RunTotals
	Totals                      map[string]int64 // outcome -> total source runs
	Retries, Throttles, Fetched int64
}

type RunTotals struct {
	Fetched, Inserted, Updated, Unchanged, Unknown, Retries, Throttles int64
	DurationSeconds                                                    float64
	Outcome                                                            string
}

type LastRun struct {
	Mode            string
	Outcome         string
	StartedAt       time.Time
	DurationSeconds float64
}

func (s *Store) Metrics(ctx context.Context) (MetricsSnapshot, error) {
	snap := MetricsSnapshot{Runs: map[string]int64{}}
	statuses, err := s.SourceStatuses(ctx)
	if err != nil {
		return snap, err
	}
	bySource := map[string]*SourceMetrics{}
	for _, st := range statuses {
		snap.Sources = append(snap.Sources, SourceMetrics{SourceStatus: st, Totals: map[string]int64{}})
	}
	for i := range snap.Sources {
		bySource[snap.Sources[i].Source+"\x00"+snap.Sources[i].Scope] = &snap.Sources[i]
	}
	rows, err := s.Pool.Query(ctx, `SELECT source, scope, outcome, count(*), sum(retries), sum(throttles), sum(fetched)
		FROM collection_run_sources WHERE outcome <> 'running' GROUP BY source, scope, outcome`)
	if err != nil {
		return snap, err
	}
	for rows.Next() {
		var source, scope, outcome string
		var n, retries, throttles, fetched int64
		if err := rows.Scan(&source, &scope, &outcome, &n, &retries, &throttles, &fetched); err != nil {
			rows.Close()
			return snap, err
		}
		if m := bySource[source+"\x00"+scope]; m != nil {
			m.Totals[outcome] += n
			m.Retries += retries
			m.Throttles += throttles
			m.Fetched += fetched
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return snap, err
	}
	rows, err = s.Pool.Query(ctx, `SELECT DISTINCT ON (source, scope) source, scope, fetched, inserted, updated, unchanged, unknown_stats,
			retries, throttles, extract(epoch FROM finished_at - started_at), outcome
		FROM collection_run_sources WHERE finished_at IS NOT NULL ORDER BY source, scope, finished_at DESC`)
	if err != nil {
		return snap, err
	}
	for rows.Next() {
		var source, scope string
		var t RunTotals
		if err := rows.Scan(&source, &scope, &t.Fetched, &t.Inserted, &t.Updated, &t.Unchanged, &t.Unknown, &t.Retries, &t.Throttles, &t.DurationSeconds, &t.Outcome); err != nil {
			rows.Close()
			return snap, err
		}
		if m := bySource[source+"\x00"+scope]; m != nil {
			m.LastRun = &t
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return snap, err
	}
	rows, err = s.Pool.Query(ctx, `SELECT outcome, count(*) FROM collection_runs WHERE outcome <> 'running' GROUP BY outcome`)
	if err != nil {
		return snap, err
	}
	for rows.Next() {
		var outcome string
		var n int64
		if err := rows.Scan(&outcome, &n); err != nil {
			rows.Close()
			return snap, err
		}
		snap.Runs[outcome] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return snap, err
	}
	var last LastRun
	err = s.Pool.QueryRow(ctx, `SELECT mode, outcome, started_at, coalesce(extract(epoch FROM finished_at - started_at), 0)
		FROM collection_runs WHERE outcome <> 'running' ORDER BY started_at DESC LIMIT 1`).Scan(&last.Mode, &last.Outcome, &last.StartedAt, &last.DurationSeconds)
	if err == nil {
		snap.Last = &last
	}
	return snap, nil
}

// WorkArea describes one work area series.
type WorkArea struct {
	Area  string `json:"area"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
	Color string `json:"color"`
}

func (s *Store) WorkAreas(ctx context.Context) ([]WorkArea, error) {
	rows, err := s.Pool.Query(ctx, `SELECT area, label, kind, color FROM work_areas ORDER BY sort_order`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkArea{}
	for rows.Next() {
		var a WorkArea
		if err := rows.Scan(&a.Area, &a.Label, &a.Kind, &a.Color); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AreaValue is one bucket's activity in one work area: changed text lines (additive across
// areas) and commits whose primary area this is (also additive).
type AreaValue struct {
	Lines   int64 `json:"lines"`
	Commits int   `json:"commits"`
	Files   int64 `json:"files"`
}

// AreaActivity returns, for each bucket start of q's series, activity by work area, using
// the same filters as Activity. Buckets without activity are omitted.
func (s *Store) AreaActivity(ctx context.Context, q ActivityQuery) (map[time.Time]map[string]AreaValue, error) {
	if err := q.validate(); err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `WITH `+filterCTE+`
		SELECT date_trunc($3, c.committed_at, $6) AS at, t.area,
			sum(t.lines), count(*) FILTER (WHERE t.is_primary), sum(t.files)
		FROM commits c
		JOIN commit_work_areas t ON t.repository_id = c.repository_id AND t.sha = c.sha, filtering
		WHERE c.committed_at >= $1 AND c.committed_at < $2 AND NOT c.is_merge
			AND ($7 = '' OR c.repository_id::text = $7)
			AND (NOT filtering.active OR c.author_email IN (SELECT email FROM filter_emails))
		GROUP BY 1, 2`, q.From, q.To, q.Unit, q.IdentityID, q.Group, q.TimeZone, q.RepositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[time.Time]map[string]AreaValue{}
	for rows.Next() {
		var at time.Time
		var area string
		var v AreaValue
		if err := rows.Scan(&at, &area, &v.Lines, &v.Commits, &v.Files); err != nil {
			return nil, err
		}
		at = at.UTC()
		if out[at] == nil {
			out[at] = map[string]AreaValue{}
		}
		out[at][area] = v
	}
	return out, rows.Err()
}
