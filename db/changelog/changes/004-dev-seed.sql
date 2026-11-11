--liquibase formatted sql

-- Development-only fixture data. Every changeset here uses the explicit-only context
-- `@dev-seed`: Liquibase runs it ONLY when the context filter names dev-seed. With no
-- filter, or any other filter (for example `live`), it is skipped. Verified with
-- liquibase:4.33.0 by scripts/test-compose.ps1.
--
-- Fixture ownership (nothing else is ever touched):
--   repository 00000000-0000-4000-8000-00000000f1c5 (is_fixture), organization 'fixture',
--   work items in project 'FIXTURE' (is_fixture), identities kind 'fixture' (fixture:*),
--   identity_aliases source 'fixture', checkpoints scoped to the fixture repo/project,
--   collection run 00000000-0000-4000-8000-0000000f1c50, app_metadata key 'dataset'.
-- Groups 'fpga' and 'software' are created if missing but never deleted.

--changeset dashboard:004-dev-seed-function context:@dev-seed runOnChange:true splitStatements:false
CREATE OR REPLACE FUNCTION dev_seed_refresh() RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    repo constant uuid := '00000000-0000-4000-8000-00000000f1c5';
    run constant uuid := '00000000-0000-4000-8000-0000000f1c50';
    h constant timestamptz := date_trunc('hour', now());
BEGIN
    -- 1. Remove previous fixture rows only.
    DELETE FROM collection_runs WHERE run_id = run;
    DELETE FROM sync_checkpoints WHERE scope IN (repo::text, 'FIXTURE');
    DELETE FROM git_ref_tips WHERE repository_id = repo;
    DELETE FROM commits WHERE repository_id = repo;
    DELETE FROM pull_requests WHERE repository_id = repo;
    DELETE FROM repositories WHERE repository_id = repo;
    DELETE FROM work_items WHERE organization = 'fixture' AND project = 'FIXTURE';
    DELETE FROM contributor_group_members WHERE identity_id LIKE 'fixture:%';
    DELETE FROM identity_aliases WHERE source = 'fixture';
    DELETE FROM identities WHERE kind = 'fixture';

    -- 2. People: two FPGA engineers, two software engineers, and one unmapped Git author
    --    (eve) who appears as git:eve@fixture.invalid.
    INSERT INTO identities (identity_id, kind, display_name, unique_name) VALUES
        ('fixture:ada', 'fixture', 'Ada Fixture (FPGA)', 'ada@fixture.invalid'),
        ('fixture:ben', 'fixture', 'Ben Fixture (FPGA)', 'ben@fixture.invalid'),
        ('fixture:cleo', 'fixture', 'Cleo Fixture (Software)', 'cleo@fixture.invalid'),
        ('fixture:dev', 'fixture', 'Dev Fixture (Software)', 'dev@fixture.invalid');
    INSERT INTO identity_aliases (email, identity_id, source)
        SELECT unique_name, identity_id, 'fixture' FROM identities WHERE kind = 'fixture';
    INSERT INTO contributor_groups (group_name, description) VALUES
        ('fpga', 'FPGA engineers'), ('software', 'Software engineers')
        ON CONFLICT (group_name) DO NOTHING;
    INSERT INTO contributor_group_members (group_name, identity_id) VALUES
        ('fpga', 'fixture:ada'), ('fpga', 'fixture:ben'),
        ('software', 'fixture:cleo'), ('software', 'fixture:dev');

    INSERT INTO repositories (repository_id, organization, project, name, web_url, is_fixture)
        VALUES (repo, 'fixture', 'FIXTURE', 'FixtureRepo', NULL, true);

    -- 3. Commits. Recent: hourly over the last 168 hours, with empty hours (i % 7 = 0),
    --    nights and weekends. History: daily over ~24 months, FPGA output and AI-marker
    --    share rising toward the present.
    CREATE TEMP TABLE fx_commit ON COMMIT DROP AS
    WITH people AS (
        SELECT * FROM (VALUES (0, 'ada', 'Ada Fixture', true), (1, 'ben', 'Ben Fixture', true),
            (2, 'cleo', 'Cleo Fixture', false), (3, 'dev', 'Dev Fixture', false), (4, 'eve', 'Eve Contractor', false))
            AS p(n, handle, name, fpga)
    ),
    recent AS (
        SELECT 'r-' || i || '-' || j AS key, i * 5 + j AS seed, p.handle, p.name, p.fpga,
            h - make_interval(hours => 167 - i) + make_interval(mins => (i * 17 + j * 11) % 60) AS at,
            (i % 11 = 0 AND j = 0) AS merge,
            (i % 17 = 5) AS unavailable,
            CASE (i * 3 + j) % 6 WHEN 0 THEN 'claude' WHEN 1 THEN 'copilot' WHEN 2 THEN 'codex' END AS ai
        FROM generate_series(0, 167) AS i, generate_series(0, 4) AS j, people p
        WHERE j < (i * 13) % 5 AND i % 7 <> 0 AND p.n = (i + j) % 5
    ),
    history AS (
        SELECT 'h-' || d || '-' || p.n || '-' || j AS key, d * 31 + p.n * 7 + j AS seed, p.handle, p.name, p.fpga,
            date_trunc('day', h) - make_interval(days => d) + make_interval(hours => 8 + (j * 3 + p.n * 2 + d) % 11, mins => (d * 7 + j) % 60) AS at,
            (d % 13 = 0 AND j = 0 AND p.n = 0) AS merge,
            (d % 97 = 0 AND j = 0) AS unavailable,
            CASE
                WHEN p.fpga AND (d * 7 + j * 3) % 100 < (90 * (730 - d)) / 730 THEN (ARRAY['claude', 'copilot', 'codex'])[1 + (d + j) % 3]
                WHEN NOT p.fpga AND (d * 7 + j * 3) % 100 < 15 + (35 * (730 - d)) / 730 THEN (ARRAY['claude', 'copilot', 'codex'])[1 + (d + j) % 3]
            END AS ai
        FROM generate_series(8, 730) AS d, people p, generate_series(0, 5) AS j
        WHERE p.n < 4
          AND extract(isodow FROM date_trunc('day', h) - make_interval(days => d)) < 6
          AND j < CASE WHEN p.fpga THEN 1 + (730 - d) / 240 + (d % 3 = 0)::int ELSE 1 + d % 2 END
    )
    SELECT key, seed, handle, name, fpga, at, merge, unavailable AND NOT merge AS unavailable, ai,
        md5('fx-c-' || key) || substr(md5('fx-c2-' || key), 1, 8) AS sha,
        1 + seed % 3 AS nfiles,
        (seed % 9 = 0) AS has_binary
    FROM (SELECT * FROM recent UNION ALL SELECT * FROM history) AS all_commits
    WHERE at <= now();

    CREATE TEMP TABLE fx_file ON COMMIT DROP AS
    SELECT c.sha, k AS file_index,
        CASE
            WHEN k = 0 AND c.has_binary THEN 'assets/board_logo.png'
            WHEN k = 2 AND c.seed % 4 = 0 THEN 'docs/notes.md'
            WHEN k = 2 AND c.seed % 4 = 1 THEN 'third_party/vendor_lib/vendor.c'
            WHEN c.fpga THEN (ARRAY['rtl/video_pipe.vhd', 'rtl/scaler_top.sv', 'constraints/pins.xdc', 'sim/tb_scaler.sv'])[1 + (c.seed + k) % 4]
            -- Paths shaped so the starter work-area rules (005) classify fixtures into each area.
            ELSE (ARRAY['server/orders/OrderService.cs', 'firmware/motor/pid_loop.c', 'web/src/app.ts', 'ui/MainView.xaml.cs',
                        'common/messaging/Bus.cs', 'builds/build-image.ps1', 'tools/release/bump_version.py'])[1 + (c.seed + k) % 7]
        END AS path,
        CASE WHEN k = 0 AND c.has_binary THEN NULL ELSE 1 + (c.seed * 7 + k * 13) % 120 END AS additions,
        CASE WHEN k = 0 AND c.has_binary THEN NULL ELSE (c.seed * 3 + k * 5) % 40 END AS deletions
    FROM fx_commit c, generate_series(0, 2) AS k
    WHERE k < c.nfiles AND NOT c.merge AND NOT c.unavailable;

    INSERT INTO commits (repository_id, sha, author_email, author_name, committer_email, committer_name, authored_at, committed_at,
        parent_count, message, additions, deletions, files_changed, binary_files, stats_status)
    SELECT repo, c.sha, c.handle || '@fixture.invalid', c.name, c.handle || '@fixture.invalid', c.name, c.at, c.at,
        CASE WHEN c.merge THEN 2 ELSE 1 END,
        CASE WHEN c.merge THEN 'Merge branch ''feature/fixture-' || c.seed || ''' into main'
             ELSE 'fixture: ' || (ARRAY['tune scaler timing', 'fix register map', 'add driver retry', 'refactor UI binding', 'update build script'])[1 + c.seed % 5]
                || CASE c.ai
                    WHEN 'claude' THEN E'\n\nCo-Authored-By: Claude <noreply@anthropic.com>'
                    WHEN 'copilot' THEN E'\n\nCo-authored-by: GitHub Copilot <copilot@github.com>'
                    WHEN 'codex' THEN E'\n\nCo-authored-by: Codex <codex@openai.com>'
                    ELSE '' END
        END,
        CASE WHEN c.merge OR c.unavailable THEN NULL ELSE coalesce(f.additions, 0) END,
        CASE WHEN c.merge OR c.unavailable THEN NULL ELSE coalesce(f.deletions, 0) END,
        CASE WHEN c.merge OR c.unavailable THEN NULL ELSE coalesce(f.files, 0) END,
        CASE WHEN c.merge OR c.unavailable THEN NULL ELSE coalesce(f.binary, 0) END,
        CASE WHEN c.merge THEN 'merge' WHEN c.unavailable THEN 'unavailable' WHEN c.has_binary THEN 'partial_binary' ELSE 'ok' END
    FROM fx_commit c
    LEFT JOIN (
        SELECT sha, sum(additions) AS additions, sum(deletions) AS deletions, count(*) AS files,
            count(*) FILTER (WHERE additions IS NULL) AS binary
        FROM fx_file GROUP BY sha
    ) f USING (sha);

    INSERT INTO commit_files (repository_id, sha, file_index, path, additions, deletions)
    SELECT repo, sha, file_index, path, additions, deletions FROM fx_file;

    -- 4. Pull requests. Recent: every third hour; completed five hours later when that has
    --    happened (one with unavailable statistics). History: roughly weekly per person.
    INSERT INTO pull_requests (repository_id, pr_id, creator_identity_id, title, status, source_ref, target_ref,
        created_at, closed_at, completed_at, additions, deletions, files_changed, binary_files, commit_count, stats_status)
    SELECT repo, pr_id, 'fixture:' || handle, 'Fixture PR ' || pr_id,
        CASE WHEN done THEN 'completed' ELSE 'active' END,
        'refs/heads/feature/fixture-' || pr_id, 'refs/heads/main', created,
        CASE WHEN done THEN created + lag END, CASE WHEN done THEN created + lag END,
        CASE WHEN done AND NOT unavailable THEN 10 + (pr_id * 37) % 400 END,
        CASE WHEN done AND NOT unavailable THEN (pr_id * 11) % 90 END,
        CASE WHEN done AND NOT unavailable THEN 1 + pr_id % 12 END,
        CASE WHEN done AND NOT unavailable THEN (pr_id % 10 = 0)::int END,
        CASE WHEN done AND NOT unavailable THEN 1 + pr_id % 6 END,
        CASE WHEN NOT done THEN 'not_completed' WHEN unavailable THEN 'unavailable'
             WHEN pr_id % 10 = 0 THEN 'partial_binary' ELSE 'ok' END
    FROM (
        SELECT 100000 + i AS pr_id, (ARRAY['ada', 'ben', 'cleo', 'dev'])[1 + (i / 3) % 4] AS handle,
            h - make_interval(hours => 167 - i) + make_interval(mins => 20) AS created, interval '5 hours' AS lag,
            h - make_interval(hours => 167 - i) + make_interval(mins => 20) + interval '5 hours' <= now() AS done,
            (i = 27) AS unavailable
        FROM generate_series(0, 167) AS i WHERE i % 3 = 0 AND i % 7 <> 0
        UNION ALL
        SELECT 200000 + d * 4 + n, (ARRAY['ada', 'ben', 'cleo', 'dev'])[1 + n],
            date_trunc('day', h) - make_interval(days => d) + make_interval(hours => 10 + n),
            make_interval(hours => 6 + d % 30), true, false
        FROM generate_series(8, 730) AS d, generate_series(0, 3) AS n
        WHERE d % 5 = n
    ) AS prs
    WHERE created <= now();

    -- 5. Tasks. Every fourth recent hour creates a task (rotating assignees, including
    --    unassigned); every eighth completes three hours later. Task 399999 is reassigned
    --    and reopened: created by Ada, completed by Ben on entering QA, moved to Closed
    --    (no new completion), reopened by Ada, completed again by Ada.
    CREATE TEMP TABLE fx_task ON COMMIT DROP AS
    SELECT 300000 + i AS id, (ARRAY['fixture:ada', 'fixture:ben', 'fixture:cleo', 'fixture:dev', 'unassigned'])[1 + (i / 4) % 5] AS who,
        h - make_interval(hours => 167 - i) + make_interval(mins => 5) AS created,
        CASE WHEN i % 8 = 1 AND h - make_interval(hours => 167 - i) + make_interval(mins => 5) + interval '3 hours' <= now()
             THEN h - make_interval(hours => 167 - i) + make_interval(mins => 5) + interval '3 hours' END AS completed
    FROM generate_series(0, 167) AS i WHERE i % 4 = 1
    UNION ALL
    SELECT 400000 + d, (ARRAY['fixture:ada', 'fixture:ben', 'fixture:cleo', 'fixture:dev'])[1 + (d / 2) % 4],
        date_trunc('day', h) - make_interval(days => d) + make_interval(hours => 9),
        date_trunc('day', h) - make_interval(days => d - 2) + make_interval(hours => 15)
    FROM generate_series(8, 730) AS d WHERE d % 2 = 0;

    INSERT INTO work_items (organization, project, work_item_id, work_item_type, title, current_state, created_at, changed_at, last_revision, is_fixture)
    SELECT 'fixture', 'FIXTURE', id, 'Task', 'Fixture task ' || id,
        CASE WHEN completed IS NULL THEN 'Active' ELSE 'Ready for QA Test Build' END,
        created, coalesce(completed, created), CASE WHEN completed IS NULL THEN 1 ELSE 2 END, true
    FROM fx_task WHERE created <= now();
    INSERT INTO task_events (organization, project, work_item_id, revision, event_type, occurred_at, identity_id, from_state, to_state)
    SELECT 'fixture', 'FIXTURE', id, 1, 'created', created, who, NULL, 'New' FROM fx_task WHERE created <= now()
    UNION ALL
    SELECT 'fixture', 'FIXTURE', id, 2, 'completed', completed, who, 'Active', 'Ready for QA Test Build' FROM fx_task WHERE completed IS NOT NULL;

    INSERT INTO work_items (organization, project, work_item_id, work_item_type, title, current_state, created_at, changed_at, last_revision, is_fixture)
    VALUES ('fixture', 'FIXTURE', 399999, 'Task', 'Fixture task reassigned and reopened', 'Closed',
        h - interval '150 hours', h - interval '20 hours', 5, true);
    INSERT INTO task_events (organization, project, work_item_id, revision, event_type, occurred_at, identity_id, from_state, to_state) VALUES
        ('fixture', 'FIXTURE', 399999, 1, 'created', h - interval '150 hours', 'fixture:ada', NULL, 'New'),
        ('fixture', 'FIXTURE', 399999, 2, 'completed', h - interval '120 hours', 'fixture:ben', 'Active', 'Ready for QA Test Build'),
        ('fixture', 'FIXTURE', 399999, 5, 'completed', h - interval '20 hours', 'fixture:ada', 'Active', 'Closed');

    -- 6. Collection status: collected through the start of the current hour.
    INSERT INTO sync_checkpoints (source, scope, watermark, coverage_start, last_success_at, last_attempt_at, last_reconcile_at, last_outcome)
    VALUES ('commits', repo::text, h, h - interval '730 days', h - interval '55 minutes', h - interval '55 minutes', h - interval '55 minutes', 'success'),
           ('pull_requests', repo::text, h, h - interval '730 days', h - interval '55 minutes', h - interval '55 minutes', h - interval '55 minutes', 'success'),
           ('tasks', 'FIXTURE', h, h - interval '730 days', h - interval '55 minutes', h - interval '55 minutes', h - interval '55 minutes', 'success');
    INSERT INTO collection_runs (run_id, mode, started_at, finished_at, outcome)
    VALUES (run, 'scheduled', h - interval '56 minutes', h - interval '55 minutes', 'success');
    INSERT INTO collection_run_sources (run_id, source, scope, started_at, finished_at, window_start, window_end, outcome, fetched, inserted, updated, unchanged)
    VALUES (run, 'commits', repo::text, h - interval '56 minutes', h - interval '55 minutes', h - interval '3 hours', h, 'success', 12, 3, 0, 9),
           (run, 'pull_requests', repo::text, h - interval '56 minutes', h - interval '55 minutes', h - interval '3 hours', h, 'success', 4, 1, 1, 2),
           (run, 'tasks', 'FIXTURE', h - interval '56 minutes', h - interval '55 minutes', h - interval '3 hours', h, 'success', 6, 1, 2, 3);

    INSERT INTO app_metadata (key, value, updated_at) VALUES ('dataset', 'fixture', now())
        ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at;

    -- Tag regenerated fixture commits with work areas once 005 has created the tagger (on a
    -- brand-new database 005's initial tagging changeset covers the first seed).
    IF to_regproc('refresh_work_area_tags') IS NOT NULL THEN
        PERFORM refresh_work_area_tags();
    END IF;
END
$$;

--changeset dashboard:004-dev-seed-refresh context:@dev-seed runAlways:true
-- Re-anchors fixtures to the current hour on every `liquibase update` with dev-seed, so the
-- rolling seven-day dashboard never goes empty. Only fixture-owned rows are rewritten.
SELECT dev_seed_refresh();
