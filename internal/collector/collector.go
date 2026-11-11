// Package collector runs one collection: it reads Azure DevOps and Git, writes facts to
// PostgreSQL, advances per-source watermarks only after facts commit, and records
// durable monitoring summaries.
package collector

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/dsmithson/EngineeringActivity/internal/ado"
	"github.com/dsmithson/EngineeringActivity/internal/facts"
	"github.com/dsmithson/EngineeringActivity/internal/gitmirror"
	"github.com/dsmithson/EngineeringActivity/internal/store"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

const (
	SourceCommits      = "commits"
	SourcePullRequests = "pull_requests"
	SourceTasks        = "tasks"
)

// Modes.
const (
	ModeScheduled = "scheduled" // catch up from each watermark (minus overlap); bootstrap if new
	ModeBackfill  = "backfill"  // collect from an explicit start through now; extends coverage
	ModeReconcile = "reconcile" // scheduled run that also re-reads the reconciliation window
	ModeWindow    = "window"    // re-collect an explicit [from, to); checkpoints untouched
)

type Config struct {
	Organization     string
	Project          string
	Repositories     []string
	WorkItemTypes    []string
	CompletionStates facts.CompletionStates
	Overlap          time.Duration
	Bootstrap        time.Duration
	ReconcileEvery   time.Duration
	ReconcileWindow  time.Duration
	Concurrency      int
	AllowFixtureDB   bool
}

// Mirror is the Git access the collector needs; *gitmirror.Mirror implements it.
type Mirror interface {
	Fetch(ctx context.Context) error
	RefTips(ctx context.Context) (map[string]string, error)
	Log(ctx context.Context, opts gitmirror.LogOptions, fn func(facts.Commit) error) error
	PRStats(ctx context.Context, sourceSHA, targetSHA string) (facts.PRStats, error)
}

type Collector struct {
	Config   Config
	ADO      ado.Source
	Store    *store.Store
	Mirrors  func(facts.Repository) Mirror
	Counters *ado.Counters
	Log      *slog.Logger
	Now      func() time.Time
}

type Request struct {
	Mode    string
	From    time.Time
	To      time.Time
	Sources map[string]bool // empty means all
}

func (r Request) wants(source string) bool { return len(r.Sources) == 0 || r.Sources[source] }

// Window is the interval a source run covers.
type Window struct {
	From, To   time.Time
	Reconciled bool
	Advance    bool
}

// PlanWindow computes a source's window. Scheduled runs start at the watermark minus the
// overlap (or bootstrap before the current hour when there is no watermark) and end at the
// start of the current UTC hour. Reconciliation extends the start back by ReconcileWindow.
func PlanWindow(cfg Config, req Request, cp store.Checkpoint, now time.Time) (Window, error) {
	hour := now.UTC().Truncate(time.Hour)
	switch req.Mode {
	case ModeBackfill:
		if req.From.IsZero() || !req.From.Before(hour) {
			return Window{}, configError{"backfill requires --from before the current hour"}
		}
		return Window{From: req.From.UTC(), To: hour, Advance: true}, nil
	case ModeWindow:
		if req.From.IsZero() || req.To.IsZero() || !req.From.Before(req.To) {
			return Window{}, configError{"window mode requires --from before --to"}
		}
		return Window{From: req.From.UTC(), To: req.To.UTC()}, nil
	case ModeScheduled, ModeReconcile:
		w := Window{To: hour, Advance: true}
		if cp.Watermark == nil {
			// A bootstrap reads at least the reconciliation window, so it counts as one.
			w.From, w.Reconciled = hour.Add(-cfg.Bootstrap), cfg.Bootstrap >= cfg.ReconcileWindow
		} else {
			w.From = cp.Watermark.UTC().Add(-cfg.Overlap)
		}
		due := req.Mode == ModeReconcile || cp.LastReconcileAt == nil || now.Sub(*cp.LastReconcileAt) >= cfg.ReconcileEvery
		if due && cp.Watermark != nil {
			if start := hour.Add(-cfg.ReconcileWindow); start.Before(w.From) {
				w.From = start
			}
			w.Reconciled = true
		}
		if !w.From.Before(w.To) {
			w.From = w.To.Add(-cfg.Overlap)
		}
		return w, nil
	default:
		return Window{}, configError{fmt.Sprintf("unknown mode %q", req.Mode)}
	}
}

type sourceResult struct {
	store.RunSource
	err error
}

// Run performs one collection. It returns an error if any source failed; sources are
// independent, so a failing source does not prevent others from committing progress.
func (c *Collector) Run(ctx context.Context, req Request) error {
	if c.Now == nil {
		c.Now = time.Now
	}
	runID := uuid.New().String()
	log := c.Log.With("run_id", runID, "mode", req.Mode)
	started := c.Now().UTC()
	release, err := c.Store.Lock(ctx)
	if err != nil {
		log.Error("collection not started", "error_class", Classify(err))
		return err
	}
	defer release()
	if !c.Config.AllowFixtureDB {
		fixture, err := c.Store.HasFixtureData(ctx)
		if err != nil {
			return err
		}
		if fixture {
			err := configError{"database contains development fixture data; refusing to collect live data into it"}
			log.Error("collection not started", "error_class", "config", "reason", err.Error())
			return err
		}
	}
	if err := c.Store.StartRun(ctx, runID, req.Mode, started); err != nil {
		return err
	}
	log.Info("collection started", "repositories", strings.Join(c.Config.Repositories, ","))

	results := []sourceResult{}
	if req.wants(SourceTasks) {
		results = append(results, c.runSource(ctx, log, runID, req, SourceTasks, c.Config.Project, c.collectTasks))
	}
	if req.wants(SourceCommits) || req.wants(SourcePullRequests) {
		for _, name := range c.Config.Repositories {
			results = append(results, c.collectRepository(ctx, log, runID, req, name)...)
		}
	}

	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
		}
	}
	outcome, class := "success", ""
	switch {
	case failed == len(results) && failed > 0:
		outcome, class = "failed", results[0].ErrorClass
	case failed > 0:
		outcome = "partial"
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := c.Store.FinishRun(finishCtx, runID, outcome, class); err != nil {
		log.Error("recording run outcome failed", "error_class", Classify(err))
	}
	log.Info("collection finished", "outcome", outcome, "sources_failed", failed, "duration_ms", c.Now().Sub(started).Milliseconds())
	if failed > 0 {
		return fmt.Errorf("%d of %d collection sources failed", failed, len(results))
	}
	return nil
}

type collectFunc func(ctx context.Context, w Window, r *store.RunSource) error

func (c *Collector) runSource(ctx context.Context, log *slog.Logger, runID string, req Request, source, scope string, fn collectFunc) sourceResult {
	res := sourceResult{RunSource: store.RunSource{Source: source, Scope: scope, StartedAt: c.Now().UTC(), Outcome: "running"}}
	slog := log.With("source", source, "scope", scope)
	var retries0, throttles0 int64
	if c.Counters != nil {
		retries0, throttles0 = c.Counters.Retries.Load(), c.Counters.Throttles.Load()
	}
	cp, err := c.Store.Checkpoint(ctx, source, scope)
	var w Window
	if err == nil {
		w, err = PlanWindow(c.Config, req, cp, c.Now())
	}
	if err == nil {
		res.WindowStart, res.WindowEnd = &w.From, &w.To
		err = fn(ctx, w, &res.RunSource)
	}
	if c.Counters != nil {
		res.Retries = int(c.Counters.Retries.Load() - retries0)
		res.Throttles = int(c.Counters.Throttles.Load() - throttles0)
	}
	// Persist outcome even if the run context was canceled.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err == nil && w.Advance {
		err = c.Store.Advance(pctx, source, scope, w.From, w.To, w.Reconciled)
	}
	res.err = err
	res.Outcome = "success"
	if err != nil {
		res.Outcome, res.ErrorClass = "failed", Classify(err)
		if ferr := c.Store.Fail(pctx, source, scope, res.ErrorClass); ferr != nil {
			slog.Error("recording source failure failed", "error_class", Classify(ferr))
		}
	}
	if rerr := c.Store.RecordRunSource(pctx, runID, res.RunSource); rerr != nil {
		slog.Error("recording source summary failed", "error_class", Classify(rerr))
	}
	attrs := []any{"outcome", res.Outcome, "fetched", res.Fetched, "inserted", res.Counts.Inserted, "updated", res.Counts.Updated,
		"unchanged", res.Counts.Unchanged, "unknown_stats", res.UnknownStats, "retries", res.Retries, "throttles", res.Throttles,
		"duration_ms", c.Now().Sub(res.StartedAt).Milliseconds()}
	if res.WindowStart != nil {
		attrs = append(attrs, "window_start", res.WindowStart.Format(time.RFC3339), "window_end", res.WindowEnd.Format(time.RFC3339), "reconciled", w.Reconciled)
	}
	if err != nil {
		slog.Error("source failed", append(attrs, "error_class", res.ErrorClass)...)
	} else {
		slog.Info("source collected", attrs...)
	}
	return res
}

func (c *Collector) collectTasks(ctx context.Context, w Window, r *store.RunSource) error {
	for _, t := range c.Config.WorkItemTypes {
		states, err := c.ADO.WorkItemTypeStates(ctx, t)
		if err != nil {
			return err
		}
		known := facts.CompletionStates{}
		for _, s := range states {
			known[strings.ToLower(s)] = true
		}
		for s := range c.Config.CompletionStates {
			if !known[s] {
				return configError{fmt.Sprintf("completion state %q is not a %s state (available: %s)", s, t, strings.Join(states, ", "))}
			}
		}
	}
	ids, err := c.ADO.ChangedWorkItemIDs(ctx, c.Config.WorkItemTypes, w.From)
	if err != nil {
		return err
	}
	r.Fetched = len(ids)
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(1, c.Config.Concurrency))
	for _, id := range ids {
		g.Go(func() error {
			revisions, err := c.ADO.WorkItemRevisions(gctx, id)
			if err != nil {
				if Classify(err) == "not_found" {
					// Deleted between query and read; its history is no longer available.
					mu.Lock()
					r.UnknownStats++
					mu.Unlock()
					return nil
				}
				return err
			}
			item, events, err := facts.DeriveWorkItem(c.Config.Organization, c.Config.Project, id, revisions, c.Config.CompletionStates)
			if err != nil {
				c.Log.Warn("work item history unusable", "source", SourceTasks, "work_item_id", id, "error_class", "invalid_data")
				mu.Lock()
				r.UnknownStats++
				mu.Unlock()
				return nil
			}
			counts, err := c.Store.ReplaceWorkItem(gctx, item, events)
			if err != nil {
				return err
			}
			mu.Lock()
			r.Counts.Add(counts)
			mu.Unlock()
			return nil
		})
	}
	return g.Wait()
}

func (c *Collector) collectRepository(ctx context.Context, log *slog.Logger, runID string, req Request, name string) []sourceResult {
	repo, err := c.ADO.Repository(ctx, name)
	if err == nil {
		err = c.Store.UpsertRepository(ctx, repo)
	}
	var mirror Mirror
	if err == nil {
		mirror = c.Mirrors(repo)
		err = mirror.Fetch(ctx)
	}
	scope := name
	if repo.ID != "" {
		scope = repo.ID
	}
	results := []sourceResult{}
	for _, source := range []string{SourceCommits, SourcePullRequests} {
		if !req.wants(source) {
			continue
		}
		if err != nil {
			fetchErr := err
			results = append(results, c.runSource(ctx, log, runID, req, source, scope, func(context.Context, Window, *store.RunSource) error { return fetchErr }))
			continue
		}
		fn := func(ctx context.Context, w Window, r *store.RunSource) error {
			return c.collectCommits(ctx, repo, mirror, req, w, r)
		}
		if source == SourcePullRequests {
			fn = func(ctx context.Context, w Window, r *store.RunSource) error {
				return c.collectPullRequests(ctx, repo, mirror, req, w, r)
			}
		}
		results = append(results, c.runSource(ctx, log.With("repository", repo.Name), runID, req, source, scope, fn))
	}
	return results
}

const commitBatch = 500

// collectCommits lists commits reachable from any branch. Incremental runs exclude the
// ancestry of the branch tips recorded after the last successful run, which also finds
// late-pushed commits with old timestamps; first runs and backfills filter by committer time.
func (c *Collector) collectCommits(ctx context.Context, repo facts.Repository, mirror Mirror, req Request, w Window, r *store.RunSource) error {
	tips, err := mirror.RefTips(ctx)
	if err != nil {
		return err
	}
	opts := gitmirror.LogOptions{Since: w.From}
	if req.Mode == ModeWindow {
		opts.Until = w.To
	}
	if req.Mode == ModeScheduled || req.Mode == ModeReconcile {
		previous, err := c.Store.RefTips(ctx, repo.ID)
		if err != nil {
			return err
		}
		if len(previous) > 0 && !w.Reconciled {
			opts = gitmirror.LogOptions{}
			for _, sha := range previous {
				opts.Exclude = append(opts.Exclude, sha)
			}
		}
	}
	batch := make([]facts.Commit, 0, commitBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		counts, err := c.Store.UpsertCommits(ctx, repo.ID, batch)
		if err != nil {
			return err
		}
		r.Counts.Add(counts)
		batch = batch[:0]
		return nil
	}
	err = mirror.Log(ctx, opts, func(commit facts.Commit) error {
		r.Fetched++
		if commit.StatsStatus != facts.StatsOK && commit.StatsStatus != facts.StatsMerge {
			r.UnknownStats++
		}
		batch = append(batch, commit)
		if len(batch) >= commitBatch {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		return err
	}
	if req.Mode == ModeWindow {
		_, err = c.Store.RefreshWorkAreas(ctx)
		return err
	}
	if err := c.Store.ReplaceRefTips(ctx, repo.ID, tips); err != nil {
		return err
	}
	_, err = c.Store.RefreshWorkAreas(ctx)
	return err
}

func (c *Collector) collectPullRequests(ctx context.Context, repo facts.Repository, mirror Mirror, req Request, w Window, r *store.RunSource) error {
	save := func(prs []facts.PullRequest) error {
		if len(prs) == 0 {
			return nil
		}
		r.Fetched += len(prs)
		counts, err := c.Store.UpsertPullRequests(ctx, prs)
		r.Counts.Add(counts)
		return err
	}
	if req.Mode == ModeBackfill {
		// The PR list is small enough to read completely; this also finds PRs created
		// before the backfill start that completed inside it.
		if err := c.ADO.PullRequests(ctx, repo.ID, ado.PRQuery{}, save); err != nil {
			return err
		}
	} else {
		maxTime := c.Now().UTC().Add(time.Hour)
		if req.Mode == ModeWindow {
			maxTime = w.To
		}
		for _, kind := range []string{"created", "closed"} {
			if err := c.ADO.PullRequests(ctx, repo.ID, ado.PRQuery{TimeRangeType: kind, Min: w.From, Max: maxTime}, save); err != nil {
				return err
			}
		}
	}
	pending, err := c.Store.PRsNeedingStats(ctx, repo.ID, w.Reconciled || req.Mode == ModeBackfill)
	if err != nil {
		return err
	}
	for _, p := range pending {
		stats, err := mirror.PRStats(ctx, p.SourceSHA, p.TargetSHA)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.Log.Warn("PR statistics unavailable", "source", SourcePullRequests, "pr_id", p.ID, "error_class", Classify(err))
			r.UnknownStats++
			if err := c.Store.SetPRStats(ctx, repo.ID, p.ID, nil); err != nil {
				return err
			}
			continue
		}
		if err := c.Store.SetPRStats(ctx, repo.ID, p.ID, &stats); err != nil {
			return err
		}
	}
	return nil
}
