--liquibase formatted sql

-- Reporting contract: functions and rpt_* views used by the web API, reports, and the
-- activity-dataviz Claude skill. These are replaceable objects, so they use runOnChange:
-- editing a definition here re-applies it. Keep column names stable; add columns rather
-- than renaming them, and document changes in docs/reporting.md.

--changeset dashboard:003-file-category runOnChange:true splitStatements:false
-- Coarse classification of a changed path, for separating HDL from software from noise.
CREATE OR REPLACE FUNCTION file_category(path text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT CASE
        WHEN p ~ '(^|/)(node_modules|vendor|third_?party|thirdparty|packages|external|extern|deps)/'
          OR p ~ '(package-lock\.json|yarn\.lock|pnpm-lock\.yaml|go\.sum|\.min\.(js|css)|\.designer\.cs|\.g\.cs|\.generated\.\w+)$'
            THEN 'generated_vendor'
        WHEN p ~ '\.(v|sv|svh|vh|vhd|vhdl|vho|vhi)$' THEN 'hdl'
        WHEN p ~ '\.(xdc|ucf|sdc|qsf|qpf|qip|xci|xcix|bd|xpr|tcl|lpf|pdc|ip|ipx|xise|xpfm)$' THEN 'fpga_project'
        WHEN p ~ '\.(c|h|cc|cpp|cxx|hh|hpp|hxx|inl|cs|go|py|js|mjs|cjs|ts|tsx|jsx|java|kt|rs|swift|m|mm|ps1|psm1|sh|bash|lua|rb|php|qml|vb|fs|sql|razor|cshtml|vue|svelte|scala|dart|asm|s)$'
            THEN 'software'
        WHEN p ~ '\.(test|spec)\.' OR p ~ '(^|/)(tests?|specs?)/' THEN 'software'
        WHEN p ~ '\.(md|markdown|rst|txt|adoc|docx?|pdf|pptx?|xlsx?|vsdx?)$' THEN 'docs'
        WHEN p ~ '\.(ya?ml|json|xml|toml|ini|cfg|conf|config|props|targets|csproj|vcxproj|sln|filters|cmake|mk|gradle|editorconfig|gitignore|gitattributes|dockerignore|resx|xaml|html|htm|css|scss|less)$'
          OR p ~ '(^|/)(makefile|dockerfile|cmakelists\.txt|jenkinsfile)$'
            THEN 'config_build_ui'
        WHEN p ~ '\.(png|jpe?g|gif|bmp|ico|svg|ttf|otf|woff2?|bin|hex|bit|mcs|elf|dll|exe|so|a|lib|zip|7z|gz|tar|mp4|wav|dat|rbf|sof|pof|jic)$'
            THEN 'binary_asset'
        ELSE 'other'
    END
    FROM (SELECT lower(path) AS p) AS normalized
$$;

--changeset dashboard:003-ai-tools runOnChange:true splitStatements:false
-- AI-assistance markers found in a commit message (trailers or tool banners). This is a
-- LOWER BOUND: many people disable or strip these trailers. Never treat an empty array as
-- evidence that no AI tool was used.
CREATE OR REPLACE FUNCTION ai_tools(message text) RETURNS text[]
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT coalesce(array_agg(t.tool ORDER BY t.tool), '{}'::text[])
    FROM (SELECT lower(coalesce(message, '')) AS m) AS n,
    LATERAL (VALUES
        ('claude',  n.m ~ 'co-authored-by:[^\n]*(claude|anthropic)' OR n.m ~ 'generated (with|by) \[?claude' OR n.m ~ 'noreply@anthropic\.com'),
        ('copilot', n.m ~ 'co-authored-by:[^\n]*copilot'),
        ('codex',   n.m ~ 'co-authored-by:[^\n]*(codex|openai|chatgpt)' OR n.m ~ 'generated (with|by) (openai )?codex'),
        ('cursor',  n.m ~ 'co-authored-by:[^\n]*cursor' OR n.m ~ 'generated (with|by) cursor'),
        ('gemini',  n.m ~ 'co-authored-by:[^\n]*gemini'),
        ('aider',   n.m ~ '^aider: ' OR n.m ~ 'co-authored-by:[^\n]*aider'),
        ('devin',   n.m ~ 'co-authored-by:[^\n]*devin')
    ) AS t(tool, hit)
    WHERE t.hit
$$;

--changeset dashboard:003-commit-subject runOnChange:true splitStatements:false
CREATE OR REPLACE FUNCTION commit_subject(message text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT left(split_part(coalesce(message, ''), E'\n', 1), 200)
$$;

--changeset dashboard:003-rpt-identities runOnChange:true splitStatements:false
CREATE OR REPLACE VIEW rpt_identities AS
SELECT i.identity_id,
       i.kind,
       i.display_name,
       i.unique_name,
       i.is_service,
       coalesce((SELECT array_agg(m.group_name ORDER BY m.group_name)
                 FROM contributor_group_members m WHERE m.identity_id = i.identity_id), '{}'::text[]) AS groups,
       coalesce((SELECT array_agg(a.email ORDER BY a.email)
                 FROM identity_aliases a WHERE a.identity_id = i.identity_id), '{}'::text[]) AS emails,
       coalesce(im.canonical_identity_id, i.identity_id) AS canonical_identity_id,
       coalesce((SELECT array_agg(m2.identity_id ORDER BY m2.identity_id)
                 FROM identity_merges m2 WHERE m2.canonical_identity_id = i.identity_id), '{}'::text[]) AS merged_identity_ids
FROM identities i
LEFT JOIN identity_merges im ON im.identity_id = i.identity_id;

--changeset dashboard:003-rpt-commits runOnChange:true splitStatements:false
-- One row per unique commit (deduplicated across branches). Attribution: Git author, mapped
-- to an Azure identity through identity_aliases at query time, so adding an alias re-credits
-- history immediately. identity_merges then folds secondary identities into their canonical
-- person (identity_id is canonical; source_identity_id is the alias target before merging).
-- Time axis convention: committed_at.
CREATE OR REPLACE VIEW rpt_commits AS
SELECT c.repository_id,
       r.name AS repository_name,
       c.sha,
       coalesce(im.canonical_identity_id, a.identity_id, 'git:' || c.author_email) AS identity_id,
       coalesce(i.display_name, c.author_name || ' (Git)') AS display_name,
       c.author_email,
       c.authored_at,
       c.committed_at,
       c.is_merge,
       c.additions,
       c.deletions,
       c.files_changed,
       c.binary_files,
       c.stats_status,
       commit_subject(c.message) AS subject,
       ai_tools(c.message) AS ai_tools,
       r.is_fixture,
       coalesce(a.identity_id, 'git:' || c.author_email) AS source_identity_id
FROM commits c
JOIN repositories r ON r.repository_id = c.repository_id
LEFT JOIN identity_aliases a ON a.email = c.author_email
LEFT JOIN identity_merges im ON im.identity_id = a.identity_id
LEFT JOIN identities i ON i.identity_id = coalesce(im.canonical_identity_id, a.identity_id);

--changeset dashboard:003-rpt-commit-files runOnChange:true splitStatements:false
CREATE OR REPLACE VIEW rpt_commit_files AS
SELECT f.repository_id,
       c.repository_name,
       f.sha,
       c.identity_id,
       c.display_name,
       c.committed_at,
       f.path,
       f.old_path,
       file_category(f.path) AS category,
       f.additions,
       f.deletions,
       (f.additions IS NULL) AS is_binary,
       c.ai_tools,
       c.is_fixture
FROM commit_files f
JOIN rpt_commits c ON c.repository_id = f.repository_id AND c.sha = f.sha;

--changeset dashboard:003-rpt-pull-requests runOnChange:true splitStatements:false
CREATE OR REPLACE VIEW rpt_pull_requests AS
SELECT p.repository_id,
       r.name AS repository_name,
       p.pr_id,
       coalesce(im.canonical_identity_id, p.creator_identity_id) AS identity_id,
       i.display_name,
       p.title,
       p.status,
       p.is_draft,
       p.source_ref,
       p.target_ref,
       p.created_at,
       p.closed_at,
       p.completed_at,
       CASE WHEN p.completed_at IS NOT NULL THEN extract(epoch FROM p.completed_at - p.created_at) / 3600.0 END AS cycle_hours,
       p.additions,
       p.deletions,
       p.files_changed,
       p.binary_files,
       p.commit_count,
       p.stats_status,
       r.is_fixture,
       p.creator_identity_id AS source_identity_id
FROM pull_requests p
JOIN repositories r ON r.repository_id = p.repository_id
LEFT JOIN identity_merges im ON im.identity_id = p.creator_identity_id
JOIN identities i ON i.identity_id = coalesce(im.canonical_identity_id, p.creator_identity_id);

--changeset dashboard:003-rpt-task-events runOnChange:true splitStatements:false
CREATE OR REPLACE VIEW rpt_task_events AS
SELECT e.organization,
       e.project,
       e.work_item_id,
       w.work_item_type,
       w.title,
       w.area_path,
       e.revision,
       e.event_type,
       e.occurred_at,
       coalesce(im.canonical_identity_id, e.identity_id) AS identity_id,
       i.display_name,
       e.from_state,
       e.to_state,
       w.is_fixture,
       e.identity_id AS source_identity_id
FROM task_events e
JOIN work_items w USING (organization, project, work_item_id)
LEFT JOIN identity_merges im ON im.identity_id = e.identity_id
JOIN identities i ON i.identity_id = coalesce(im.canonical_identity_id, e.identity_id);

--changeset dashboard:003-rpt-daily-activity runOnChange:true splitStatements:false
-- Long-range rollup: one row per UTC day and identity with any activity. Days with no row
-- for an identity had no recorded activity *if* the sources were collected for that day;
-- check rpt_collection_status coverage before calling a day quiet.
CREATE OR REPLACE VIEW rpt_daily_activity AS
WITH events AS (
    SELECT (committed_at AT TIME ZONE 'UTC')::date AS day, identity_id, display_name, is_fixture,
           CASE WHEN is_merge THEN 0 ELSE 1 END AS commits,
           CASE WHEN is_merge THEN 1 ELSE 0 END AS merge_commits,
           CASE WHEN is_merge THEN 0 ELSE coalesce(additions, 0) END AS commit_additions,
           CASE WHEN is_merge THEN 0 ELSE coalesce(deletions, 0) END AS commit_deletions,
           CASE WHEN is_merge THEN 0 ELSE coalesce(files_changed, 0) END AS commit_files,
           CASE WHEN NOT is_merge AND cardinality(ai_tools) > 0 THEN 1 ELSE 0 END AS ai_marked_commits,
           0 AS prs_created, 0 AS prs_completed, 0 AS pr_additions, 0 AS pr_deletions, 0 AS pr_files,
           0 AS tasks_created, 0 AS tasks_completed
    FROM rpt_commits
    UNION ALL
    SELECT (created_at AT TIME ZONE 'UTC')::date, identity_id, display_name, is_fixture,
           0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0
    FROM rpt_pull_requests
    UNION ALL
    SELECT (completed_at AT TIME ZONE 'UTC')::date, identity_id, display_name, is_fixture,
           0, 0, 0, 0, 0, 0, 0, 1, coalesce(additions, 0), coalesce(deletions, 0), coalesce(files_changed, 0), 0, 0
    FROM rpt_pull_requests WHERE completed_at IS NOT NULL
    UNION ALL
    SELECT (occurred_at AT TIME ZONE 'UTC')::date, identity_id, display_name, is_fixture,
           0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
           CASE WHEN event_type = 'created' THEN 1 ELSE 0 END,
           CASE WHEN event_type = 'completed' THEN 1 ELSE 0 END
    FROM rpt_task_events
)
SELECT day, identity_id, min(display_name) AS display_name, bool_or(is_fixture) AS is_fixture,
       sum(commits)::int AS commits, sum(merge_commits)::int AS merge_commits,
       sum(commit_additions)::bigint AS commit_additions, sum(commit_deletions)::bigint AS commit_deletions,
       sum(commit_files)::int AS commit_files, sum(ai_marked_commits)::int AS ai_marked_commits,
       sum(prs_created)::int AS prs_created, sum(prs_completed)::int AS prs_completed,
       sum(pr_additions)::bigint AS pr_additions, sum(pr_deletions)::bigint AS pr_deletions, sum(pr_files)::int AS pr_files,
       sum(tasks_created)::int AS tasks_created, sum(tasks_completed)::int AS tasks_completed
FROM events
GROUP BY day, identity_id;

--changeset dashboard:003-rpt-unmapped-git-authors runOnChange:true splitStatements:false
-- Git author emails without an identity alias, with candidate Azure identities whose display
-- name matches the Git author name. Candidates are suggestions only: confirm before adding a
-- 'manual' identity_aliases row.
CREATE OR REPLACE VIEW rpt_unmapped_git_authors AS
SELECT c.author_email,
       min(c.author_name) AS author_name,
       count(*) AS commits,
       min(c.committed_at) AS first_commit_at,
       max(c.committed_at) AS last_commit_at,
       coalesce((SELECT array_agg(DISTINCT i.identity_id)
                 FROM identities i
                 WHERE i.kind = 'ado' AND lower(i.display_name) = lower(min(c.author_name))
                   AND NOT EXISTS (SELECT 1 FROM identity_merges m WHERE m.identity_id = i.identity_id)), '{}'::text[]) AS candidate_identity_ids
FROM commits c
LEFT JOIN identity_aliases a ON a.email = c.author_email
WHERE a.email IS NULL
GROUP BY c.author_email;

--changeset dashboard:003-rpt-collection-status runOnChange:true splitStatements:false
CREATE OR REPLACE VIEW rpt_collection_status AS
SELECT source, scope, coverage_start, watermark, last_success_at, last_attempt_at,
       last_reconcile_at, last_outcome, last_error_class
FROM sync_checkpoints;

--changeset dashboard:003-reporting-grants runOnChange:true
GRANT SELECT ON ALL TABLES IN SCHEMA public TO dashboard_read;
