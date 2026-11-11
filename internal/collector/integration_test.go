package collector

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dsmithson/EngineeringActivity/internal/ado"
	"github.com/dsmithson/EngineeringActivity/internal/facts"
	"github.com/dsmithson/EngineeringActivity/internal/gitmirror"
	"github.com/dsmithson/EngineeringActivity/internal/store"
)

// These tests need a PostgreSQL database initialized by the real Liquibase changelog:
//
//	TEST_DATABASE_URL=postgres://activity:test@127.0.0.1:55432/activity?sslmode=disable
//
// The database is truncated; never point this at a database you care about.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	ctx := context.Background()
	s, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	_, err = s.Pool.Exec(ctx, `TRUNCATE commits, commit_files, git_ref_tips, pull_requests, task_events, work_items,
		sync_checkpoints, collection_run_sources, collection_runs, contributor_group_members, contributor_groups, identity_merges,
		identity_aliases, repositories CASCADE;
		DELETE FROM work_area_rules WHERE priority = 5;
		DELETE FROM identities WHERE identity_id NOT IN ('unassigned', 'unknown');
		DELETE FROM app_metadata WHERE key = 'dataset'`)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type fakeADO struct {
	repo      facts.Repository
	states    []string
	revisions map[int][]facts.Revision
	prs       []facts.PullRequest
	failTasks error
	prQueries []ado.PRQuery
}

func (f *fakeADO) Repository(context.Context, string) (facts.Repository, error) { return f.repo, nil }
func (f *fakeADO) WorkItemTypeStates(context.Context, string) ([]string, error) {
	return f.states, f.failTasks
}
func (f *fakeADO) ChangedWorkItemIDs(context.Context, []string, time.Time) ([]int, error) {
	ids := []int{}
	for id := range f.revisions {
		ids = append(ids, id)
	}
	return ids, nil
}
func (f *fakeADO) WorkItemRevisions(_ context.Context, id int) ([]facts.Revision, error) {
	return f.revisions[id], nil
}
func (f *fakeADO) PullRequests(_ context.Context, _ string, q ado.PRQuery, page func([]facts.PullRequest) error) error {
	f.prQueries = append(f.prQueries, q)
	return page(f.prs)
}

type gitRepo struct {
	t   *testing.T
	dir string
}

func (r *gitRepo) git(env []string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir, "-c", "core.autocrlf=false"}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %s", args, b)
	}
	return strings.TrimSpace(string(b))
}

func (r *gitRepo) commit(email string, at time.Time, file, content, message string) string {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, file), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
	date := at.UTC().Format(time.RFC3339)
	env := []string{"GIT_AUTHOR_NAME=" + strings.Split(email, "@")[0], "GIT_AUTHOR_EMAIL=" + email, "GIT_COMMITTER_NAME=ci", "GIT_COMMITTER_EMAIL=ci@example.test",
		"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
	r.git(env, "add", "-A")
	r.git(env, "commit", "--quiet", "-m", message)
	return r.git(nil, "rev-parse", "HEAD")
}

func rev(n int, state string, assignee map[string]any, at time.Time) facts.Revision {
	f := map[string]any{"System.State": state, "System.CreatedDate": "2026-01-01T00:00:00Z", "System.ChangedDate": at.UTC().Format(time.RFC3339Nano),
		"System.WorkItemType": "Task", "System.Title": "Task title"}
	if n == 1 {
		f["System.CreatedDate"] = at.UTC().Format(time.RFC3339Nano)
	}
	if assignee != nil {
		f["System.AssignedTo"] = assignee
	}
	return facts.Revision{Rev: n, Fields: f}
}

func count(t *testing.T, s *store.Store, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := s.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func TestCollectorEndToEnd(t *testing.T) {
	db := testStore(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	ctx := context.Background()
	now := time.Now().UTC()
	hour := now.Truncate(time.Hour)

	src := &gitRepo{t: t, dir: t.TempDir()}
	src.git(nil, "init", "--quiet", "--initial-branch=main")
	base := src.commit("alice@example.test", hour.Add(-30*time.Hour), "a.go", "package a\n", "base")
	src.git(nil, "checkout", "--quiet", "-b", "feature")
	feature := src.commit("Alice@Example.test", hour.Add(-20*time.Hour), "b.vhd", "x\ny\n", "feature work\n\nCo-Authored-By: Claude <noreply@anthropic.com>")
	src.git(nil, "checkout", "--quiet", "main")
	mainTip := src.commit("bob@example.test", hour.Add(-10*time.Hour), "a.go", "package a\n// b\n", "main work")
	_ = base

	alice := map[string]any{"id": "AAAAAAAA-0000-0000-0000-000000000001", "displayName": "Alice", "uniqueName": "alice@example.test"}
	bob := map[string]any{"id": "bbbbbbbb-0000-0000-0000-000000000002", "displayName": "Bob", "uniqueName": "bob@example.test"}
	created := hour.Add(-40 * time.Hour)
	completed := hour.Add(-5 * time.Hour)
	fake := &fakeADO{
		repo:   facts.Repository{ID: "df164785-6f11-4e78-bcb1-ab75ba9677f0", Organization: "org", Project: "proj", Name: "Widgets"},
		states: []string{"New", "Active", "Ready for QA Test Build", "Ready to Test", "Closed"},
		revisions: map[int][]facts.Revision{
			42: {
				rev(1, "New", alice, hour.Add(-50*time.Hour)),
				rev(2, "Ready for QA Test Build", bob, hour.Add(-48*time.Hour)),
				rev(3, "Closed", bob, hour.Add(-47*time.Hour)),
				rev(4, "Active", alice, hour.Add(-30*time.Hour)),
				rev(5, "Closed", alice, hour.Add(-3*time.Hour)),
			},
			43: {rev(1, "New", nil, hour.Add(-2*time.Hour))},
		},
		prs: []facts.PullRequest{{RepositoryID: "df164785-6f11-4e78-bcb1-ab75ba9677f0", ID: 7, Creator: facts.IdentityFromRef(alice), Title: "Feature",
			Status: "completed", CreatedAt: created, ClosedAt: &completed, CompletedAt: &completed, LastMergeSourceSHA: feature, LastMergeTargetSHA: mainTip}},
	}
	var logs bytes.Buffer
	mirrorDir := t.TempDir()
	c := &Collector{
		Config: Config{Organization: "org", Project: "proj", Repositories: []string{"Widgets"}, WorkItemTypes: []string{"Task"},
			CompletionStates: facts.ParseCompletionStates("ready for qa test build,ready to test,closed"),
			Overlap:          2 * time.Hour, Bootstrap: 7 * 24 * time.Hour, ReconcileEvery: 24 * time.Hour, ReconcileWindow: 7 * 24 * time.Hour, Concurrency: 2},
		ADO: fake, Store: db, Counters: &ado.Counters{},
		Log: slog.New(slog.NewJSONHandler(&logs, nil)),
		Mirrors: func(r facts.Repository) Mirror {
			return &gitmirror.Mirror{Dir: filepath.Join(mirrorDir, r.Name+".git"), URL: src.dir}
		},
	}

	// First scheduled run bootstraps seven days.
	if err := c.Run(ctx, Request{Mode: ModeScheduled}); err != nil {
		t.Fatalf("first run: %v\n%s", err, logs.String())
	}
	if n := count(t, db, `SELECT count(*) FROM commits`); n != 3 {
		t.Fatalf("commits = %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM rpt_commits WHERE identity_id = 'aaaaaaaa-0000-0000-0000-000000000001'`); n != 2 {
		t.Fatalf("alias mapping (case-insensitive email) credited %d commits to Alice", n)
	}
	if n := count(t, db, `SELECT count(*) FROM rpt_commits WHERE 'claude' = ANY(ai_tools)`); n != 1 {
		t.Fatalf("ai marker commits = %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM rpt_commit_files WHERE category = 'hdl'`); n != 1 {
		t.Fatalf("hdl file changes = %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM pull_requests WHERE stats_status = 'ok' AND commit_count = 1 AND files_changed = 1 AND additions = 2`); n != 1 {
		t.Fatalf("PR snapshot statistics missing")
	}
	// Work areas: every non-merge commit with files is tagged; the VHDL file is FPGA work.
	if n := count(t, db, `SELECT count(*) FROM commits WHERE work_areas_version IS NULL`); n != 0 {
		t.Fatalf("%d commits not tagged with work areas", n)
	}
	if n := count(t, db, `SELECT count(*) FROM commit_work_areas WHERE sha = $1 AND area = 'fpga' AND is_primary AND files = 1 AND lines = 2`, feature); n != 1 {
		t.Fatal("feature commit should be tagged primary FPGA work")
	}
	// A rule change bumps the version and re-tags at the next refresh.
	if _, err := db.Pool.Exec(ctx, `INSERT INTO work_area_rules (priority, area, pattern) VALUES (5, 'shared', '^a\.go$')`); err != nil {
		t.Fatal(err)
	}
	if n, err := db.RefreshWorkAreas(ctx); err != nil || n != 3 {
		t.Fatalf("re-tag after rule change: %d %v", n, err)
	}
	if n := count(t, db, `SELECT count(*) FROM commit_work_areas WHERE sha = $1 AND area = 'shared' AND is_primary AND rules_version = (SELECT value::int FROM app_metadata WHERE key = 'work_area_rules_version')`, mainTip); n != 1 {
		t.Fatal("rule change not applied")
	}
	if _, err := db.Pool.Exec(ctx, `DELETE FROM work_area_rules WHERE pattern = '^a\.go$'`); err != nil {
		t.Fatal(err)
	}

	// Task 42: created by Alice, completed by Bob (entering QA), not again on QA->Closed,
	// reopened, completed again by Alice. Task 43: created unassigned.
	if n := count(t, db, `SELECT count(*) FROM task_events WHERE work_item_id = 42 AND event_type = 'completed'`); n != 2 {
		t.Fatalf("completions = %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM task_events WHERE work_item_id = 42 AND revision = 2 AND identity_id = 'bbbbbbbb-0000-0000-0000-000000000002'`); n != 1 {
		t.Fatal("historical assignee not preserved")
	}
	if n := count(t, db, `SELECT count(*) FROM task_events WHERE work_item_id = 43 AND identity_id = 'unassigned'`); n != 1 {
		t.Fatal("unassigned creation missing")
	}
	if n := count(t, db, `SELECT count(*) FROM sync_checkpoints WHERE last_outcome = 'success' AND watermark = $1`, hour); n != 3 {
		t.Fatalf("checkpoints at current hour = %d", n)
	}

	// A run killed mid-flight leaves a 'running' row; the next run closes it as interrupted.
	if _, err := db.Pool.Exec(ctx, `INSERT INTO collection_runs (run_id, mode, started_at, outcome) VALUES (gen_random_uuid(), 'backfill', now() - interval '1 hour', 'running')`); err != nil {
		t.Fatal(err)
	}

	// Repeat run: nothing changes; incremental commit discovery finds nothing new.
	logs.Reset()
	if err := c.Run(ctx, Request{Mode: ModeScheduled}); err != nil {
		t.Fatalf("repeat run: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM commits`); n != 3 {
		t.Fatalf("repeat duplicated commits: %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM collection_runs WHERE outcome = 'failed' AND error_class = 'interrupted'`); n != 1 {
		t.Fatalf("interrupted run not closed: %d", n)
	}
	if n := count(t, db, `SELECT coalesce(sum(inserted + updated), 0) FROM collection_run_sources s JOIN collection_runs r USING (run_id)
		WHERE r.started_at = (SELECT max(started_at) FROM collection_runs)`); n != 0 {
		t.Fatalf("repeat run changed %d rows", n)
	}

	// A late push of a commit with an old timestamp is found through branch-tip exclusion.
	src.git(nil, "checkout", "--quiet", "feature")
	late := src.commit("alice@example.test", hour.Add(-100*24*time.Hour), "c.go", "package c\n", "backdated")
	if err := c.Run(ctx, Request{Mode: ModeScheduled}); err != nil {
		t.Fatalf("late push run: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM commits WHERE sha = $1`, late); n != 1 {
		t.Fatal("late-pushed backdated commit missed")
	}

	// Source isolation: a failing task source keeps its watermark; Git sources still succeed.
	var before time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT last_success_at FROM sync_checkpoints WHERE source = 'tasks'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	fake.failTasks = errors.New("boom")
	if err := c.Run(ctx, Request{Mode: ModeScheduled}); err == nil {
		t.Fatal("expected failure")
	}
	var outcome string
	var after time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT last_outcome, last_success_at FROM sync_checkpoints WHERE source = 'tasks'`).Scan(&outcome, &after); err != nil {
		t.Fatal(err)
	}
	if outcome != "failed" || !after.Equal(before) {
		t.Fatalf("failed source must not advance: %s %v %v", outcome, before, after)
	}
	if n := count(t, db, `SELECT count(*) FROM collection_runs WHERE outcome = 'partial'`); n != 1 {
		t.Fatal("partial run not recorded")
	}
	fake.failTasks = nil

	// Read side: gap-free hourly series and contributor filtering.
	q := store.ActivityQuery{From: hour.Add(-167 * time.Hour), To: hour.Add(time.Hour), Unit: "hour", TimeZone: "UTC"}
	buckets, err := db.Activity(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 168 {
		t.Fatalf("buckets = %d", len(buckets))
	}
	total := func(bs []store.Bucket, f func(store.Bucket) int) int {
		n := 0
		for _, b := range bs {
			n += f(b)
		}
		return n
	}
	if n := total(buckets, func(b store.Bucket) int { return b.Commits }); n != 3 {
		t.Fatalf("7-day commits = %d (late commit is older than window)", n)
	}
	if n := total(buckets, func(b store.Bucket) int { return b.TaskCompleted }); n != 2 {
		t.Fatalf("task completions = %d", n)
	}
	q.IdentityID = "bbbbbbbb-0000-0000-0000-000000000002"
	bobOnly, err := db.Activity(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if total(bobOnly, func(b store.Bucket) int { return b.Commits }) != 1 || total(bobOnly, func(b store.Bucket) int { return b.TaskCompleted }) != 1 ||
		total(bobOnly, func(b store.Bucket) int { return b.PRCompleted }) != 0 {
		t.Fatal("contributor filter")
	}
	areas, err := db.AreaActivity(ctx, store.ActivityQuery{From: hour.Add(-167 * time.Hour), To: hour.Add(time.Hour), Unit: "hour", TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	primary := 0
	for _, byArea := range areas {
		for _, v := range byArea {
			primary += v.Commits
		}
	}
	if primary != 3 {
		t.Fatalf("primary-area commits must sum to non-merge commits: %d", primary)
	}
	days, err := db.Activity(ctx, store.ActivityQuery{From: hour.AddDate(0, 0, -7), To: hour, Unit: "day", TimeZone: "America/Phoenix"})
	if err != nil || len(days) < 7 || len(days) > 9 {
		t.Fatalf("day buckets %d %v", len(days), err)
	}
	ids, err := db.ActiveIdentities(ctx, q.From, q.To)
	if err != nil || len(ids) < 3 {
		t.Fatalf("identities %+v %v", ids, err)
	}

	// Identity merges: fold Bob into Alice for reporting; history re-credits at query time.
	const aliceID, bobID = "aaaaaaaa-0000-0000-0000-000000000001", "bbbbbbbb-0000-0000-0000-000000000002"
	if _, err := db.Pool.Exec(ctx, `INSERT INTO identity_merges (identity_id, canonical_identity_id, note) VALUES ($1, $2, 'test')`, bobID, aliceID); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM rpt_commits WHERE identity_id = $1`, aliceID); n != 4 {
		t.Fatalf("merged commits credited to canonical = %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM rpt_task_events WHERE identity_id = $1 AND source_identity_id = $2`, aliceID, bobID); n != 1 {
		t.Fatalf("merged task events = %d", n)
	}
	q.IdentityID = aliceID
	merged, err := db.Activity(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if total(merged, func(b store.Bucket) int { return b.Commits }) != 3 || total(merged, func(b store.Bucket) int { return b.TaskCompleted }) != 2 {
		t.Fatal("filtering the canonical person must include merged identities")
	}
	active, err := db.ActiveIdentities(ctx, q.From, q.To)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range active {
		if i.ID == bobID {
			t.Fatal("merged identity listed separately")
		}
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO identities (identity_id, kind, display_name) VALUES ('zzz', 'ado', 'Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO identity_merges (identity_id, canonical_identity_id) VALUES ('zzz', $1)`, bobID); err == nil {
		t.Fatal("merge into an already-merged identity must be rejected")
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO identity_merges (identity_id, canonical_identity_id) VALUES ($1, 'zzz')`, aliceID); err == nil {
		t.Fatal("merging away a canonical identity must be rejected")
	}
	if _, err := db.Pool.Exec(ctx, `DELETE FROM identity_merges`); err != nil {
		t.Fatal(err)
	}

	snap, err := db.Metrics(ctx)
	if err != nil || len(snap.Sources) != 3 || snap.Runs["success"] < 3 || snap.Runs["partial"] != 1 {
		t.Fatalf("metrics %+v %v", snap, err)
	}

	// JSON logs are parseable and never contain repository content or credentials.
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if !strings.HasPrefix(line, "{") || strings.Contains(line, "boom") {
			t.Fatalf("log line: %s", line)
		}
	}

	// Development fixture data blocks live collection unless explicitly allowed.
	if _, err := db.Pool.Exec(ctx, `INSERT INTO app_metadata (key, value) VALUES ('dataset', 'fixture')`); err != nil {
		t.Fatal(err)
	}
	if err := c.Run(ctx, Request{Mode: ModeScheduled}); err == nil || Classify(err) != "config" {
		t.Fatalf("fixture database accepted: %v", err)
	}
}

func TestPlanWindow(t *testing.T) {
	cfg := Config{Overlap: 2 * time.Hour, Bootstrap: 7 * 24 * time.Hour, ReconcileEvery: 24 * time.Hour, ReconcileWindow: 7 * 24 * time.Hour}
	now := time.Date(2026, 10, 15, 13, 5, 0, 0, time.UTC)
	hour := time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)
	w, _ := PlanWindow(cfg, Request{Mode: ModeScheduled}, store.Checkpoint{}, now)
	if !w.From.Equal(hour.Add(-7*24*time.Hour)) || !w.To.Equal(hour) || !w.Reconciled {
		t.Fatalf("bootstrap %+v", w)
	}
	wm := hour.Add(-time.Hour)
	recent := now.Add(-time.Hour)
	w, _ = PlanWindow(cfg, Request{Mode: ModeScheduled}, store.Checkpoint{Watermark: &wm, LastReconcileAt: &recent}, now)
	if !w.From.Equal(wm.Add(-2*time.Hour)) || !w.To.Equal(hour) || w.Reconciled {
		t.Fatalf("incremental %+v", w)
	}
	old := now.Add(-25 * time.Hour)
	w, _ = PlanWindow(cfg, Request{Mode: ModeScheduled}, store.Checkpoint{Watermark: &wm, LastReconcileAt: &old}, now)
	if !w.Reconciled || !w.From.Equal(hour.Add(-7*24*time.Hour)) {
		t.Fatalf("reconcile %+v", w)
	}
	stale := hour.Add(-30 * 24 * time.Hour)
	w, _ = PlanWindow(cfg, Request{Mode: ModeScheduled}, store.Checkpoint{Watermark: &stale, LastReconcileAt: &recent}, now)
	if !w.From.Equal(stale.Add(-2 * time.Hour)) {
		t.Fatalf("catch-up must start at the old watermark: %+v", w)
	}
	if _, err := PlanWindow(cfg, Request{Mode: ModeBackfill}, store.Checkpoint{}, now); err == nil {
		t.Fatal("backfill without start accepted")
	}
	w, _ = PlanWindow(cfg, Request{Mode: ModeWindow, From: hour.Add(-5 * time.Hour), To: hour.Add(-2 * time.Hour)}, store.Checkpoint{}, now)
	if w.Advance {
		t.Fatal("window mode must not move checkpoints")
	}
}
