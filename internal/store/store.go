// Package store persists typed facts in PostgreSQL. The schema is owned by Liquibase
// (db/changelog); this package never creates or alters tables.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dsmithson/EngineeringActivity/internal/facts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ Pool *pgxpool.Pool }

func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("invalid DATABASE_URL")
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "engineering-activity"
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err = p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return &Store{p}, nil
}

func (s *Store) Close() { s.Pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.Pool.Ping(ctx) }

// Counts reports how an upsert batch changed the database.
type Counts struct{ Inserted, Updated, Unchanged int }

func (c *Counts) Add(o Counts) {
	c.Inserted += o.Inserted
	c.Updated += o.Updated
	c.Unchanged += o.Unchanged
}

// collectorLockKey guards against concurrent collectors writing the same facts.
const collectorLockKey = 721804913

// Lock takes the collector advisory lock on a dedicated connection. The returned release
// function must be called to free it.
func (s *Store) Lock(ctx context.Context) (func(), error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", collectorLockKey).Scan(&ok); err != nil {
		conn.Release()
		return nil, err
	}
	if !ok {
		conn.Release()
		return nil, ErrLocked
	}
	return func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", collectorLockKey)
		conn.Release()
	}, nil
}

var ErrLocked = errors.New("another collector holds the collection lock")

// HasFixtureData reports whether development seed data is present. The live collector
// refuses to write into a seeded development database unless explicitly overridden.
func (s *Store) HasFixtureData(ctx context.Context) (bool, error) {
	var found bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM repositories WHERE is_fixture)
		OR EXISTS (SELECT 1 FROM work_items WHERE is_fixture)
		OR EXISTS (SELECT 1 FROM app_metadata WHERE key = 'dataset' AND value = 'fixture')`).Scan(&found)
	return found, err
}

func (s *Store) UpsertRepository(ctx context.Context, r facts.Repository) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO repositories (repository_id, organization, project, name, web_url)
		VALUES ($1, $2, $3, $4, nullif($5, ''))
		ON CONFLICT (repository_id) DO UPDATE SET organization = excluded.organization, project = excluded.project,
			name = excluded.name, web_url = excluded.web_url, updated_at = CURRENT_TIMESTAMP
		WHERE (repositories.organization, repositories.project, repositories.name, repositories.web_url)
			IS DISTINCT FROM (excluded.organization, excluded.project, excluded.name, excluded.web_url)`,
		r.ID, r.Organization, r.Project, r.Name, r.WebURL)
	return err
}

// upsertIdentities records Azure identities and learns their email aliases. Manual aliases
// always win over learned ones.
func upsertIdentities(ctx context.Context, tx pgx.Tx, ids []facts.Identity) error {
	seen := map[string]bool{}
	batch := &pgx.Batch{}
	for _, i := range ids {
		if i.Kind != facts.KindADO || seen[i.ID] {
			continue
		}
		seen[i.ID] = true
		batch.Queue(`INSERT INTO identities (identity_id, kind, display_name, unique_name, is_service)
			VALUES ($1, 'ado', $2, nullif($3, ''), $4)
			ON CONFLICT (identity_id) DO UPDATE SET display_name = excluded.display_name, unique_name = excluded.unique_name,
				is_service = excluded.is_service, updated_at = CURRENT_TIMESTAMP
			WHERE (identities.display_name, identities.unique_name, identities.is_service)
				IS DISTINCT FROM (excluded.display_name, excluded.unique_name, excluded.is_service)`,
			i.ID, i.DisplayName, i.UniqueName, i.IsService)
		if email := i.AliasEmail(); email != "" {
			batch.Queue(`INSERT INTO identity_aliases (email, identity_id, source) VALUES ($1, $2, 'ado_unique_name')
				ON CONFLICT (email) DO UPDATE SET identity_id = excluded.identity_id, updated_at = CURRENT_TIMESTAMP
				WHERE identity_aliases.source = 'ado_unique_name' AND identity_aliases.identity_id <> excluded.identity_id`,
				email, i.ID)
		}
	}
	if batch.Len() == 0 {
		return nil
	}
	return tx.SendBatch(ctx, batch).Close()
}

func inTx(ctx context.Context, s *Store, fn func(pgx.Tx) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UpsertCommits stores commits and their file changes in one transaction. A commit's file
// rows are rewritten only when the commit row itself changed.
func (s *Store) UpsertCommits(ctx context.Context, repositoryID string, commits []facts.Commit) (Counts, error) {
	var counts Counts
	err := inTx(ctx, s, func(tx pgx.Tx) error {
		counts = Counts{}
		for _, c := range commits {
			var inserted bool
			err := tx.QueryRow(ctx, `INSERT INTO commits (repository_id, sha, author_email, author_name, committer_email, committer_name,
					authored_at, committed_at, parent_count, message, additions, deletions, files_changed, binary_files, stats_status)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
				ON CONFLICT (repository_id, sha) DO UPDATE SET author_email = excluded.author_email, author_name = excluded.author_name,
					committer_email = excluded.committer_email, committer_name = excluded.committer_name,
					authored_at = excluded.authored_at, committed_at = excluded.committed_at, parent_count = excluded.parent_count,
					message = excluded.message, additions = excluded.additions, deletions = excluded.deletions,
					files_changed = excluded.files_changed, binary_files = excluded.binary_files,
					stats_status = excluded.stats_status, work_areas_version = NULL, updated_at = CURRENT_TIMESTAMP
				WHERE (commits.author_email, commits.author_name, commits.committer_email, commits.committer_name, commits.authored_at,
					commits.committed_at, commits.parent_count, commits.message, commits.additions, commits.deletions,
					commits.files_changed, commits.binary_files, commits.stats_status)
					IS DISTINCT FROM (excluded.author_email, excluded.author_name, excluded.committer_email, excluded.committer_name,
					excluded.authored_at, excluded.committed_at, excluded.parent_count, excluded.message, excluded.additions,
					excluded.deletions, excluded.files_changed, excluded.binary_files, excluded.stats_status)
				RETURNING (xmax = 0)`,
				repositoryID, c.SHA, c.AuthorEmail, c.AuthorName, c.CommitterEmail, c.CommitterName, c.AuthoredAt, c.CommittedAt,
				c.ParentCount, c.Message, c.Additions, c.Deletions, c.FilesChanged, c.BinaryFiles, c.StatsStatus).Scan(&inserted)
			if errors.Is(err, pgx.ErrNoRows) {
				counts.Unchanged++
				continue
			}
			if err != nil {
				return fmt.Errorf("commit %.12s: %w", c.SHA, err)
			}
			if inserted {
				counts.Inserted++
			} else {
				counts.Updated++
				if _, err := tx.Exec(ctx, `DELETE FROM commit_files WHERE repository_id = $1 AND sha = $2`, repositoryID, c.SHA); err != nil {
					return err
				}
			}
			if len(c.Files) > 0 {
				rows := make([][]any, len(c.Files))
				for i, f := range c.Files {
					var old any
					if f.OldPath != "" {
						old = f.OldPath
					}
					rows[i] = []any{repositoryID, c.SHA, i, f.Path, old, f.Additions, f.Deletions}
				}
				if _, err := tx.CopyFrom(ctx, pgx.Identifier{"commit_files"}, []string{"repository_id", "sha", "file_index", "path", "old_path", "additions", "deletions"}, pgx.CopyFromRows(rows)); err != nil {
					return fmt.Errorf("commit %.12s files: %w", c.SHA, err)
				}
			}
		}
		return nil
	})
	return counts, err
}

// RefTips returns branch tips recorded after the previous successful commit collection.
func (s *Store) RefTips(ctx context.Context, repositoryID string) (map[string]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT ref_name, sha FROM git_ref_tips WHERE repository_id = $1`, repositoryID)
	if err != nil {
		return nil, err
	}
	tips := map[string]string{}
	for rows.Next() {
		var ref, sha string
		if err := rows.Scan(&ref, &sha); err != nil {
			return nil, err
		}
		tips[ref] = sha
	}
	return tips, rows.Err()
}

// ReplaceRefTips records the branch tips whose history has been fully collected. Deleted
// upstream branches stay in the mirror, so tips are replaced as a set.
func (s *Store) ReplaceRefTips(ctx context.Context, repositoryID string, tips map[string]string) error {
	return inTx(ctx, s, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM git_ref_tips WHERE repository_id = $1`, repositoryID); err != nil {
			return err
		}
		rows := [][]any{}
		for ref, sha := range tips {
			rows = append(rows, []any{repositoryID, ref, sha})
		}
		_, err := tx.CopyFrom(ctx, pgx.Identifier{"git_ref_tips"}, []string{"repository_id", "ref_name", "sha"}, pgx.CopyFromRows(rows))
		return err
	})
}

// UpsertPullRequests stores PR metadata. Snapshot statistics are preserved: they are set by
// UpdatePRStats and reset to pending only if the recorded completion snapshot changes.
func (s *Store) UpsertPullRequests(ctx context.Context, prs []facts.PullRequest) (Counts, error) {
	var counts Counts
	err := inTx(ctx, s, func(tx pgx.Tx) error {
		counts = Counts{}
		creators := make([]facts.Identity, 0, len(prs))
		for _, p := range prs {
			creators = append(creators, p.Creator)
		}
		if err := upsertIdentities(ctx, tx, creators); err != nil {
			return err
		}
		for _, p := range prs {
			status := facts.StatsNotCompleted
			if p.CompletedAt != nil {
				status = facts.StatsPending
			}
			var inserted bool
			err := tx.QueryRow(ctx, `INSERT INTO pull_requests (repository_id, pr_id, creator_identity_id, title, status, is_draft,
					source_ref, target_ref, created_at, closed_at, completed_at, last_merge_source_sha, last_merge_target_sha, stats_status)
				VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), nullif($8, ''), $9, $10, $11, nullif($12, ''), nullif($13, ''), $14)
				ON CONFLICT (repository_id, pr_id) DO UPDATE SET creator_identity_id = excluded.creator_identity_id,
					title = excluded.title, status = excluded.status, is_draft = excluded.is_draft, source_ref = excluded.source_ref,
					target_ref = excluded.target_ref, created_at = excluded.created_at, closed_at = excluded.closed_at,
					completed_at = excluded.completed_at, last_merge_source_sha = excluded.last_merge_source_sha,
					last_merge_target_sha = excluded.last_merge_target_sha,
					stats_status = CASE
						WHEN excluded.completed_at IS NULL THEN 'not_completed'
						WHEN pull_requests.completed_at IS NOT NULL
							AND pull_requests.last_merge_source_sha IS NOT DISTINCT FROM excluded.last_merge_source_sha
							AND pull_requests.last_merge_target_sha IS NOT DISTINCT FROM excluded.last_merge_target_sha
							AND pull_requests.stats_status <> 'not_completed'
							THEN pull_requests.stats_status
						ELSE 'pending' END,
					merge_base_sha = CASE WHEN excluded.completed_at IS NULL THEN NULL ELSE pull_requests.merge_base_sha END,
					updated_at = CURRENT_TIMESTAMP
				WHERE (pull_requests.creator_identity_id, pull_requests.title, pull_requests.status, pull_requests.is_draft,
					pull_requests.source_ref, pull_requests.target_ref, pull_requests.created_at, pull_requests.closed_at,
					pull_requests.completed_at, pull_requests.last_merge_source_sha, pull_requests.last_merge_target_sha)
					IS DISTINCT FROM (excluded.creator_identity_id, excluded.title, excluded.status, excluded.is_draft,
					excluded.source_ref, excluded.target_ref, excluded.created_at, excluded.closed_at, excluded.completed_at,
					excluded.last_merge_source_sha, excluded.last_merge_target_sha)
				RETURNING (xmax = 0)`,
				p.RepositoryID, p.ID, p.Creator.ID, p.Title, p.Status, p.IsDraft, p.SourceRef, p.TargetRef, p.CreatedAt,
				p.ClosedAt, p.CompletedAt, p.LastMergeSourceSHA, p.LastMergeTargetSHA, status).Scan(&inserted)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				counts.Unchanged++
			case err != nil:
				return fmt.Errorf("pull request %d: %w", p.ID, err)
			case inserted:
				counts.Inserted++
			default:
				counts.Updated++
			}
		}
		// Clear stale statistics for PRs whose snapshot changed or that were reactivated.
		_, err := tx.Exec(ctx, `UPDATE pull_requests SET additions = NULL, deletions = NULL, files_changed = NULL,
				binary_files = NULL, commit_count = NULL
			WHERE stats_status IN ('pending', 'not_completed') AND (additions IS NOT NULL OR commit_count IS NOT NULL)`)
		return err
	})
	return counts, err
}

// PendingPRSnapshot identifies a completed PR whose statistics still need computing.
type PendingPRSnapshot struct {
	RepositoryID, SourceSHA, TargetSHA string
	ID                                 int
}

// PRsNeedingStats lists completed PRs with pending statistics; retryUnavailable also
// returns ones that previously failed (used during reconciliation).
func (s *Store) PRsNeedingStats(ctx context.Context, repositoryID string, retryUnavailable bool) ([]PendingPRSnapshot, error) {
	rows, err := s.Pool.Query(ctx, `SELECT pr_id, coalesce(last_merge_source_sha, ''), coalesce(last_merge_target_sha, '')
		FROM pull_requests WHERE repository_id = $1 AND completed_at IS NOT NULL
			AND (stats_status = 'pending' OR ($2 AND stats_status = 'unavailable'))
		ORDER BY completed_at`, repositoryID, retryUnavailable)
	if err != nil {
		return nil, err
	}
	out := []PendingPRSnapshot{}
	for rows.Next() {
		p := PendingPRSnapshot{RepositoryID: repositoryID}
		if err := rows.Scan(&p.ID, &p.SourceSHA, &p.TargetSHA); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPRStats records a completed PR's snapshot statistics, or marks them unavailable
// when stats is nil.
func (s *Store) SetPRStats(ctx context.Context, repositoryID string, prID int, stats *facts.PRStats) error {
	if stats == nil {
		_, err := s.Pool.Exec(ctx, `UPDATE pull_requests SET stats_status = 'unavailable', updated_at = CURRENT_TIMESTAMP
			WHERE repository_id = $1 AND pr_id = $2 AND completed_at IS NOT NULL`, repositoryID, prID)
		return err
	}
	_, err := s.Pool.Exec(ctx, `UPDATE pull_requests SET merge_base_sha = $3, additions = $4, deletions = $5, files_changed = $6,
			binary_files = $7, commit_count = $8, stats_status = $9, updated_at = CURRENT_TIMESTAMP
		WHERE repository_id = $1 AND pr_id = $2 AND completed_at IS NOT NULL`,
		repositoryID, prID, stats.MergeBaseSHA, stats.Diff.Additions, stats.Diff.Deletions, stats.Diff.Files,
		stats.Diff.Binary, stats.Commits, stats.Diff.Status())
	return err
}

// ReplaceWorkItem stores a work item and replaces its derived events atomically.
func (s *Store) ReplaceWorkItem(ctx context.Context, w facts.WorkItem, events []facts.TaskEvent) (Counts, error) {
	var counts Counts
	err := inTx(ctx, s, func(tx pgx.Tx) error {
		counts = Counts{}
		ids := make([]facts.Identity, 0, len(events))
		for _, e := range events {
			ids = append(ids, e.Identity)
		}
		if err := upsertIdentities(ctx, tx, ids); err != nil {
			return err
		}
		var previous *int
		err := tx.QueryRow(ctx, `SELECT last_revision FROM work_items WHERE organization = $1 AND project = $2 AND work_item_id = $3`,
			w.Organization, w.Project, w.ID).Scan(&previous)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		switch {
		case previous == nil:
			counts.Inserted = 1
		case *previous == w.LastRevision:
			counts.Unchanged = 1
		default:
			counts.Updated = 1
		}
		if _, err := tx.Exec(ctx, `INSERT INTO work_items (organization, project, work_item_id, work_item_type, title, current_state,
				area_path, iteration_path, created_at, changed_at, last_revision)
			VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), nullif($8, ''), $9, $10, $11)
			ON CONFLICT (organization, project, work_item_id) DO UPDATE SET work_item_type = excluded.work_item_type,
				title = excluded.title, current_state = excluded.current_state, area_path = excluded.area_path,
				iteration_path = excluded.iteration_path, created_at = excluded.created_at, changed_at = excluded.changed_at,
				last_revision = excluded.last_revision, updated_at = CURRENT_TIMESTAMP`,
			w.Organization, w.Project, w.ID, w.Type, w.Title, w.CurrentState, w.AreaPath, w.IterationPath, w.CreatedAt, w.ChangedAt, w.LastRevision); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM task_events WHERE organization = $1 AND project = $2 AND work_item_id = $3`,
			w.Organization, w.Project, w.ID); err != nil {
			return err
		}
		rows := make([][]any, len(events))
		for i, e := range events {
			var from any
			if e.Revision > 1 || e.FromState != "" {
				from = e.FromState
			}
			rows[i] = []any{w.Organization, w.Project, w.ID, e.Revision, e.Type, e.OccurredAt, e.Identity.ID, from, e.ToState}
		}
		_, err = tx.CopyFrom(ctx, pgx.Identifier{"task_events"}, []string{"organization", "project", "work_item_id", "revision", "event_type", "occurred_at", "identity_id", "from_state", "to_state"}, pgx.CopyFromRows(rows))
		return err
	})
	return counts, err
}

// Checkpoint is the durable progress of one source.
type Checkpoint struct {
	Source, Scope   string
	Watermark       *time.Time
	CoverageStart   *time.Time
	LastReconcileAt *time.Time
}

func (s *Store) Checkpoint(ctx context.Context, source, scope string) (Checkpoint, error) {
	c := Checkpoint{Source: source, Scope: scope}
	err := s.Pool.QueryRow(ctx, `SELECT watermark, coverage_start, last_reconcile_at FROM sync_checkpoints WHERE source = $1 AND scope = $2`,
		source, scope).Scan(&c.Watermark, &c.CoverageStart, &c.LastReconcileAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, nil
	}
	return c, err
}

// Advance records a successful collection of [from, to). The watermark only moves
// forward and coverage only extends backward, so a backfill never rewinds hourly progress.
func (s *Store) Advance(ctx context.Context, source, scope string, from, to time.Time, reconciled bool) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO sync_checkpoints (source, scope, watermark, coverage_start, last_success_at, last_attempt_at,
			last_reconcile_at, last_outcome, last_error_class)
		VALUES ($1, $2, $4, $3, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CASE WHEN $5 THEN CURRENT_TIMESTAMP END, 'success', NULL)
		ON CONFLICT (source, scope) DO UPDATE SET
			watermark = CASE WHEN sync_checkpoints.watermark IS NULL OR excluded.watermark > sync_checkpoints.watermark
				THEN excluded.watermark ELSE sync_checkpoints.watermark END,
			coverage_start = CASE WHEN sync_checkpoints.coverage_start IS NULL OR excluded.coverage_start < sync_checkpoints.coverage_start
				THEN excluded.coverage_start ELSE sync_checkpoints.coverage_start END,
			last_success_at = CURRENT_TIMESTAMP, last_attempt_at = CURRENT_TIMESTAMP,
			last_reconcile_at = coalesce(excluded.last_reconcile_at, sync_checkpoints.last_reconcile_at),
			last_outcome = 'success', last_error_class = NULL`,
		source, scope, from, to, reconciled)
	return err
}

// Fail records an unsuccessful attempt without moving the watermark.
func (s *Store) Fail(ctx context.Context, source, scope, errorClass string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO sync_checkpoints (source, scope, last_attempt_at, last_outcome, last_error_class)
		VALUES ($1, $2, CURRENT_TIMESTAMP, 'failed', $3)
		ON CONFLICT (source, scope) DO UPDATE SET last_attempt_at = CURRENT_TIMESTAMP, last_outcome = 'failed',
			last_error_class = excluded.last_error_class`, source, scope, errorClass)
	return err
}

// Run monitoring.

type RunSource struct {
	Source, Scope          string
	StartedAt              time.Time
	WindowStart, WindowEnd *time.Time
	Outcome                string
	Fetched                int
	Counts                 Counts
	UnknownStats           int
	Retries, Throttles     int
	ErrorClass             string
}

// StartRun records a new run. It must be called while holding the collector lock, so any
// run still marked running was interrupted (killed, OOM, node loss) and is closed as failed.
// RefreshWorkAreas tags commits whose work-area tags are missing or stale (see
// refresh_work_area_tags in db/changelog/changes/005-work-areas.sql).
func (s *Store) RefreshWorkAreas(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT refresh_work_area_tags()`).Scan(&n)
	return n, err
}

func (s *Store) StartRun(ctx context.Context, runID, mode string, started time.Time) error {
	return inTx(ctx, s, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE collection_run_sources SET outcome = 'failed', error_class = 'interrupted', finished_at = CURRENT_TIMESTAMP
			WHERE outcome = 'running'`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE collection_runs SET outcome = 'failed', error_class = 'interrupted', finished_at = CURRENT_TIMESTAMP
			WHERE outcome = 'running'`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO collection_runs (run_id, mode, started_at, outcome) VALUES ($1, $2, $3, 'running')`, runID, mode, started)
		return err
	})
}

func (s *Store) RecordRunSource(ctx context.Context, runID string, r RunSource) error {
	var errClass any
	if r.ErrorClass != "" {
		errClass = r.ErrorClass
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO collection_run_sources (run_id, source, scope, started_at, finished_at, window_start, window_end,
			outcome, fetched, inserted, updated, unchanged, unknown_stats, retries, throttles, error_class)
		VALUES ($1, $2, $3, $4, CURRENT_TIMESTAMP, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (run_id, source, scope) DO UPDATE SET finished_at = CURRENT_TIMESTAMP, outcome = excluded.outcome,
			fetched = excluded.fetched, inserted = excluded.inserted, updated = excluded.updated, unchanged = excluded.unchanged,
			unknown_stats = excluded.unknown_stats, retries = excluded.retries, throttles = excluded.throttles,
			error_class = excluded.error_class`,
		runID, r.Source, r.Scope, r.StartedAt, r.WindowStart, r.WindowEnd, r.Outcome, r.Fetched, r.Counts.Inserted,
		r.Counts.Updated, r.Counts.Unchanged, r.UnknownStats, r.Retries, r.Throttles, errClass)
	return err
}

func (s *Store) FinishRun(ctx context.Context, runID, outcome, errorClass string) error {
	var errClass any
	if errorClass != "" {
		errClass = errorClass
	}
	_, err := s.Pool.Exec(ctx, `UPDATE collection_runs SET finished_at = CURRENT_TIMESTAMP, outcome = $2, error_class = $3 WHERE run_id = $1`,
		runID, outcome, errClass)
	return err
}
