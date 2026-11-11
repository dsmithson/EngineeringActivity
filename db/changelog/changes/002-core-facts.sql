--liquibase formatted sql

-- Typed fact model for the activity dashboard and long-range reporting.
-- All timestamps are timestamptz stored as UTC instants. Statistics that could not be
-- computed are NULL (never zero) and described by a stats_status column.

--changeset dashboard:002-repositories
CREATE TABLE repositories (
    repository_id uuid PRIMARY KEY,
    organization text NOT NULL,
    project text NOT NULL,
    name text NOT NULL,
    web_url text,
    is_fixture boolean NOT NULL DEFAULT false,
    first_seen_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (organization, project, name)
);

--changeset dashboard:002-identities
-- identity_id is an Azure DevOps identity GUID, or a synthetic id:
--   git:<lowercased email>  Git author with no mapped Azure identity
--   unassigned              task event with no assignee
--   unknown                 assignee value present but unusable
--   fixture:<name>          development seed data only
CREATE TABLE identities (
    identity_id text PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('ado', 'git', 'unassigned', 'unknown', 'fixture')),
    display_name text NOT NULL,
    unique_name text,
    is_service boolean NOT NULL DEFAULT false,
    first_seen_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
INSERT INTO identities (identity_id, kind, display_name) VALUES
    ('unassigned', 'unassigned', 'Unassigned'),
    ('unknown', 'unknown', 'Unknown historical assignee');

-- Git author email -> identity. 'manual' rows are administrator overrides and are never
-- replaced by automatic ADO uniqueName learning.
CREATE TABLE identity_aliases (
    email text PRIMARY KEY CHECK (email = lower(email) AND email <> ''),
    identity_id text NOT NULL REFERENCES identities (identity_id),
    source text NOT NULL CHECK (source IN ('ado_unique_name', 'manual', 'fixture')),
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX identity_aliases_identity_idx ON identity_aliases (identity_id);

--changeset dashboard:002-contributor-groups
-- Editable cohorts (for example 'fpga') used by long-range reports. Populated by people,
-- never by the collector. Optional validity dates support people changing teams.
CREATE TABLE contributor_groups (
    group_name text PRIMARY KEY CHECK (group_name ~ '^[a-z0-9][a-z0-9_-]{0,62}$'),
    description text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE contributor_group_members (
    group_name text NOT NULL REFERENCES contributor_groups (group_name) ON DELETE CASCADE,
    identity_id text NOT NULL REFERENCES identities (identity_id),
    valid_from date,
    valid_to date,
    PRIMARY KEY (group_name, identity_id),
    CHECK (valid_to IS NULL OR valid_from IS NULL OR valid_to >= valid_from)
);

--changeset dashboard:002-commits
CREATE TABLE commits (
    repository_id uuid NOT NULL REFERENCES repositories (repository_id),
    sha text NOT NULL CHECK (sha ~ '^[0-9a-f]{40}$'),
    author_email text NOT NULL CHECK (author_email = lower(author_email)),
    author_name text NOT NULL,
    committer_email text NOT NULL,
    committer_name text NOT NULL,
    authored_at timestamptz NOT NULL,
    committed_at timestamptz NOT NULL,
    parent_count smallint NOT NULL CHECK (parent_count >= 0),
    is_merge boolean GENERATED ALWAYS AS (parent_count > 1) STORED,
    message text NOT NULL,
    -- Sums over text files only; binary files add to files_changed/binary_files.
    additions integer CHECK (additions >= 0),
    deletions integer CHECK (deletions >= 0),
    files_changed integer CHECK (files_changed >= 0),
    binary_files integer CHECK (binary_files >= 0),
    stats_status text NOT NULL CHECK (stats_status IN ('ok', 'partial_binary', 'unavailable', 'merge')),
    first_seen_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (repository_id, sha)
);
CREATE INDEX commits_committed_at_idx ON commits (committed_at);
CREATE INDEX commits_author_committed_idx ON commits (author_email, committed_at);

-- One row per file-change occurrence in a non-merge commit (diff against first parent or
-- the empty tree). additions/deletions are NULL for binary files.
CREATE TABLE commit_files (
    repository_id uuid NOT NULL,
    sha text NOT NULL,
    file_index integer NOT NULL CHECK (file_index >= 0),
    path text NOT NULL,
    old_path text,
    additions integer CHECK (additions >= 0),
    deletions integer CHECK (deletions >= 0),
    PRIMARY KEY (repository_id, sha, file_index),
    FOREIGN KEY (repository_id, sha) REFERENCES commits (repository_id, sha) ON DELETE CASCADE
);

-- Branch tips observed after the last successful commit collection. Incremental runs list
-- commits reachable from current refs but not from these tips, so late pushes of commits
-- with old timestamps are still discovered.
CREATE TABLE git_ref_tips (
    repository_id uuid NOT NULL REFERENCES repositories (repository_id),
    ref_name text NOT NULL,
    sha text NOT NULL CHECK (sha ~ '^[0-9a-f]{40}$'),
    observed_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (repository_id, ref_name)
);

--changeset dashboard:002-pull-requests
CREATE TABLE pull_requests (
    repository_id uuid NOT NULL REFERENCES repositories (repository_id),
    pr_id integer NOT NULL CHECK (pr_id > 0),
    creator_identity_id text NOT NULL REFERENCES identities (identity_id),
    title text NOT NULL,
    status text NOT NULL CHECK (status IN ('active', 'abandoned', 'completed', 'notSet')),
    is_draft boolean NOT NULL DEFAULT false,
    source_ref text,
    target_ref text,
    created_at timestamptz NOT NULL,
    closed_at timestamptz,
    completed_at timestamptz,
    last_merge_source_sha text CHECK (last_merge_source_sha ~ '^[0-9a-f]{40}$'),
    last_merge_target_sha text CHECK (last_merge_target_sha ~ '^[0-9a-f]{40}$'),
    merge_base_sha text CHECK (merge_base_sha ~ '^[0-9a-f]{40}$'),
    -- Completion snapshot: diff merge-base..source at completion. NULL until computed.
    additions integer CHECK (additions >= 0),
    deletions integer CHECK (deletions >= 0),
    files_changed integer CHECK (files_changed >= 0),
    binary_files integer CHECK (binary_files >= 0),
    commit_count integer CHECK (commit_count >= 0),
    stats_status text NOT NULL CHECK (stats_status IN ('ok', 'partial_binary', 'unavailable', 'pending', 'not_completed')),
    first_seen_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (repository_id, pr_id),
    CHECK (completed_at IS NULL OR status = 'completed')
);
CREATE INDEX pull_requests_created_idx ON pull_requests (created_at);
CREATE INDEX pull_requests_completed_idx ON pull_requests (completed_at) WHERE completed_at IS NOT NULL;
CREATE INDEX pull_requests_creator_created_idx ON pull_requests (creator_identity_id, created_at);
CREATE INDEX pull_requests_creator_completed_idx ON pull_requests (creator_identity_id, completed_at) WHERE completed_at IS NOT NULL;

--changeset dashboard:002-work-items
CREATE TABLE work_items (
    organization text NOT NULL,
    project text NOT NULL,
    work_item_id integer NOT NULL CHECK (work_item_id > 0),
    work_item_type text NOT NULL,
    title text NOT NULL,
    current_state text NOT NULL,
    area_path text,
    iteration_path text,
    created_at timestamptz NOT NULL,
    changed_at timestamptz NOT NULL,
    last_revision integer NOT NULL CHECK (last_revision > 0),
    is_fixture boolean NOT NULL DEFAULT false,
    first_seen_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (organization, project, work_item_id)
);

-- Derived from the full revision history of a work item. A task's events are replaced as a
-- whole whenever its history is re-read, which keeps re-collection idempotent.
CREATE TABLE task_events (
    organization text NOT NULL,
    project text NOT NULL,
    work_item_id integer NOT NULL,
    revision integer NOT NULL CHECK (revision > 0),
    event_type text NOT NULL CHECK (event_type IN ('created', 'completed')),
    occurred_at timestamptz NOT NULL,
    identity_id text NOT NULL REFERENCES identities (identity_id),
    from_state text,
    to_state text NOT NULL,
    PRIMARY KEY (organization, project, work_item_id, revision, event_type),
    FOREIGN KEY (organization, project, work_item_id) REFERENCES work_items (organization, project, work_item_id) ON DELETE CASCADE
);
CREATE INDEX task_events_occurred_idx ON task_events (occurred_at);
CREATE INDEX task_events_identity_occurred_idx ON task_events (identity_id, occurred_at);

--changeset dashboard:002-collection-monitoring
-- Per (source, scope) progress. watermark: facts are complete up to (exclusive) this instant.
-- coverage_start: earliest instant a successful run has covered (backfill depth).
CREATE TABLE sync_checkpoints (
    source text NOT NULL CHECK (source IN ('commits', 'pull_requests', 'tasks')),
    scope text NOT NULL,
    watermark timestamptz,
    coverage_start timestamptz,
    last_success_at timestamptz,
    last_attempt_at timestamptz,
    last_reconcile_at timestamptz,
    last_outcome text CHECK (last_outcome IN ('success', 'failed')),
    last_error_class text,
    PRIMARY KEY (source, scope)
);

CREATE TABLE collection_runs (
    run_id uuid PRIMARY KEY,
    mode text NOT NULL CHECK (mode IN ('scheduled', 'backfill', 'reconcile', 'window')),
    started_at timestamptz NOT NULL,
    finished_at timestamptz,
    outcome text NOT NULL CHECK (outcome IN ('running', 'success', 'partial', 'failed')),
    error_class text
);
CREATE INDEX collection_runs_started_idx ON collection_runs (started_at);

CREATE TABLE collection_run_sources (
    run_id uuid NOT NULL REFERENCES collection_runs (run_id) ON DELETE CASCADE,
    source text NOT NULL,
    scope text NOT NULL,
    started_at timestamptz NOT NULL,
    finished_at timestamptz,
    window_start timestamptz,
    window_end timestamptz,
    outcome text NOT NULL CHECK (outcome IN ('running', 'success', 'failed')),
    fetched integer NOT NULL DEFAULT 0,
    inserted integer NOT NULL DEFAULT 0,
    updated integer NOT NULL DEFAULT 0,
    unchanged integer NOT NULL DEFAULT 0,
    unknown_stats integer NOT NULL DEFAULT 0,
    retries integer NOT NULL DEFAULT 0,
    throttles integer NOT NULL DEFAULT 0,
    error_class text,
    PRIMARY KEY (run_id, source, scope)
);

--changeset dashboard:002-read-role splitStatements:false
-- NOLOGIN role carrying read-only privileges for reports and the web process. Grant it to
-- a login role managed outside Git, e.g. GRANT dashboard_read TO reporting_user;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'dashboard_read') THEN
        CREATE ROLE dashboard_read NOLOGIN;
    END IF;
END
$$;

--changeset dashboard:002-read-grants
GRANT USAGE ON SCHEMA public TO dashboard_read;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO dashboard_read;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO dashboard_read;
