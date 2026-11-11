--liquibase formatted sql

-- Work-area tagging. Path rules (editable data) classify each changed file; per-commit tags
-- roll those up by area with the rules version and tag time, so tags can be re-encoded when
-- rules change (or later replaced by LLM/manual tags, which carry their own source).

--changeset dashboard:005-work-areas
--validCheckSum: 9:e35419aae07260165a0ad357e8a2b6f0
-- The starter areas and path rules below are a generic example; replace them with your own
-- (they are ordinary data, see docs/reporting.md). An earlier private revision seeded
-- organization-specific rules; the accepted checksum above keeps databases created from it
-- valid without re-running this step.
CREATE TABLE work_areas (
    area text PRIMARY KEY CHECK (area ~ '^[a-z0-9_]{1,32}$'),
    label text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('software', 'firmware', 'fpga', 'support', 'other')),
    sort_order integer NOT NULL UNIQUE,
    -- Dashboard series color: categorical slots validated for the dark chart surface.
    color text NOT NULL CHECK (color ~ '^#[0-9a-f]{6}$')
);
INSERT INTO work_areas (area, label, kind, sort_order, color) VALUES
    ('frontend', 'Frontend & UI',               'software', 1, '#3987e5'),
    ('backend',  'Backend & services',          'software', 2, '#d95926'),
    ('tools',    'Tools & scripts',             'software', 3, '#199e70'),
    ('shared',   'Shared libraries',            'software', 4, '#c98500'),
    ('firmware', 'Firmware',                    'firmware', 5, '#d55181'),
    ('fpga',     'FPGA & hardware',             'fpga',     6, '#008300'),
    ('build',    'Build & CI',                  'support',  7, '#9085e9'),
    ('docs',     'Docs & plans',                'support',  8, '#e66767'),
    ('other',    'Vendored, generated & other', 'other',    9, '#6b7a90');

-- First matching rule (lowest priority, then rule_id) wins; patterns are PostgreSQL regular
-- expressions matched against the lowercased path. Unmatched paths are 'other'.
CREATE TABLE work_area_rules (
    rule_id bigserial PRIMARY KEY,
    priority integer NOT NULL,
    area text NOT NULL REFERENCES work_areas (area) ON UPDATE CASCADE,
    pattern text NOT NULL CHECK (('x' ~ pattern) IS NOT NULL),
    note text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (priority, pattern)
);
INSERT INTO work_area_rules (priority, area, pattern, note) VALUES
    (1,  'other',    '(^|/)(node_modules|third_?party|vendor|external|packages|bin|obj|dist)/', 'vendored, packaged, or build-output trees'),
    (3,  'other',    '(\.min\.(js|css)|package-lock\.json|yarn\.lock|pnpm-lock\.yaml|go\.sum|\.designer\.cs|\.g\.cs|\.generated\.[a-z]+)$', 'generated files'),
    (10, 'docs',     '\.(md|markdown|rst|txt|adoc|docx?|pdf|pptx?|xlsx?|vsdx?)$', 'documentation and plans'),
    (11, 'docs',     '(^|/)\.claude/', 'AI agent configuration'),
    (20, 'fpga',     '\.(v|sv|svh|vh|vhd|vhdl|xdc|ucf|sdc|qsf|qip|xci|bd|xpr|tcl|bit|mcs|xsa)$', 'HDL, constraints, FPGA projects, bitstreams'),
    (21, 'fpga',     '(^|/)(rtl|hdl|fpga|hw|hardware)/', 'hardware design trees'),
    (30, 'firmware', '(^|/)(firmware|fw|embedded|mcu|rtos)/', 'embedded firmware'),
    (35, 'build',    '(^|/)(builds?|pipelines?|\.github|\.azuredevops|ci)/', 'build and pipeline directories'),
    (36, 'build',    '(^|/)(azure-pipelines[^/]*|[^/]*-pipelines?\.ya?ml|dockerfile[^/]*|docker-compose[^/]*|compose[^/]*\.ya?ml|makefile|[^/]*\.(sln|slnx|proj|props|targets)|build[^/]*\.(bat|ps1|sh|cmd)|\.gitignore|\.gitattributes|\.editorconfig)$', 'build and CI files'),
    (40, 'tools',    '(^|/)(tools|scripts|cli)/', 'developer tools and scripts'),
    (60, 'frontend', '(^|/)(web|ui|frontend|client|clients|app|www|static)/', 'web and desktop clients'),
    (61, 'frontend', '\.(tsx|jsx|vue|svelte|css|scss|less|xaml|html?)$', 'UI source files'),
    (70, 'backend',  '(^|/)(server|servers|api|backend|services?|cmd|internal)/', 'servers and services'),
    (80, 'shared',   '(^|/)(common|shared|libs?|pkg|core|src)/', 'shared libraries');

ALTER TABLE commits ADD COLUMN work_areas_version integer;
ALTER TABLE commits ADD COLUMN work_areas_tagged_at timestamptz;

CREATE TABLE commit_work_areas (
    repository_id uuid NOT NULL,
    sha text NOT NULL,
    area text NOT NULL REFERENCES work_areas (area) ON UPDATE CASCADE,
    files integer NOT NULL CHECK (files >= 0),
    lines bigint NOT NULL CHECK (lines >= 0),
    is_primary boolean NOT NULL DEFAULT false,
    source text NOT NULL DEFAULT 'rule' CHECK (source IN ('rule', 'llm', 'manual')),
    rules_version integer,
    tagged_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (repository_id, sha, area),
    FOREIGN KEY (repository_id, sha) REFERENCES commits (repository_id, sha) ON DELETE CASCADE
);
CREATE INDEX commit_work_areas_area_idx ON commit_work_areas (area);

INSERT INTO app_metadata (key, value) VALUES ('work_area_rules_version', '1')
ON CONFLICT (key) DO NOTHING;

--changeset dashboard:005-work-area-functions runOnChange:true splitStatements:false
-- Any change to the rules bumps the version, which marks every commit's rule tags stale.
CREATE OR REPLACE FUNCTION work_area_rules_bump() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO app_metadata (key, value, updated_at) VALUES ('work_area_rules_version', '2', now())
    ON CONFLICT (key) DO UPDATE SET value = (app_metadata.value::int + 1)::text, updated_at = now();
    RETURN NULL;
END
$$;

--changeset dashboard:005-work-area-rules-trigger
CREATE TRIGGER work_area_rules_changed AFTER INSERT OR UPDATE OR DELETE ON work_area_rules
    FOR EACH STATEMENT EXECUTE FUNCTION work_area_rules_bump();

--changeset dashboard:005-work-area-path-function runOnChange:true splitStatements:false
CREATE OR REPLACE FUNCTION work_area(path text) RETURNS text LANGUAGE sql STABLE PARALLEL SAFE AS $$
    SELECT coalesce((SELECT r.area FROM work_area_rules r WHERE lower(path) ~ r.pattern ORDER BY r.priority, r.rule_id LIMIT 1), 'other')
$$;

--changeset dashboard:005-refresh-work-area-tags runOnChange:true splitStatements:false
-- Re-tags commits whose rule tags are missing or older than the current rules version.
-- Commits with manual or LLM tags keep them. The primary area is the one with the most
-- changed files, ignoring 'other' unless a commit touched nothing else, so per-area commit
-- counts add up to total commits. Returns the number of commits examined.
CREATE OR REPLACE FUNCTION refresh_work_area_tags() RETURNS integer LANGUAGE plpgsql AS $$
DECLARE
    v integer := coalesce((SELECT value::int FROM app_metadata WHERE key = 'work_area_rules_version'), 1);
    n integer;
BEGIN
    CREATE TEMP TABLE IF NOT EXISTS wa_stale (repository_id uuid, sha text, PRIMARY KEY (repository_id, sha)) ON COMMIT DROP;
    TRUNCATE wa_stale;
    INSERT INTO wa_stale SELECT c.repository_id, c.sha FROM commits c
    WHERE c.work_areas_version IS DISTINCT FROM v
      AND NOT EXISTS (SELECT 1 FROM commit_work_areas t WHERE t.repository_id = c.repository_id AND t.sha = c.sha AND t.source <> 'rule');
    GET DIAGNOSTICS n = ROW_COUNT;
    IF n = 0 THEN
        RETURN 0;
    END IF;
    DELETE FROM commit_work_areas t USING wa_stale s WHERE t.repository_id = s.repository_id AND t.sha = s.sha AND t.source = 'rule';
    INSERT INTO commit_work_areas (repository_id, sha, area, files, lines, source, rules_version, tagged_at)
    SELECT f.repository_id, f.sha, work_area(f.path), count(*), sum(coalesce(f.additions, 0) + coalesce(f.deletions, 0)), 'rule', v, now()
    FROM commit_files f JOIN wa_stale s USING (repository_id, sha)
    GROUP BY f.repository_id, f.sha, work_area(f.path);
    UPDATE commit_work_areas t SET is_primary = true
    FROM (
        SELECT DISTINCT ON (t2.repository_id, t2.sha) t2.repository_id, t2.sha, t2.area
        FROM commit_work_areas t2 JOIN wa_stale s USING (repository_id, sha) JOIN work_areas a ON a.area = t2.area
        ORDER BY t2.repository_id, t2.sha, (t2.area = 'other'), t2.files DESC, t2.lines DESC, a.sort_order
    ) p
    WHERE t.repository_id = p.repository_id AND t.sha = p.sha AND t.area = p.area;
    UPDATE commits c SET work_areas_version = v, work_areas_tagged_at = now()
    FROM wa_stale s WHERE c.repository_id = s.repository_id AND c.sha = s.sha;
    RETURN n;
END
$$;

--changeset dashboard:005-rpt-commit-work-areas runOnChange:true splitStatements:false
-- One row per commit and work area it touched (non-merge commits with file changes).
-- Sum files/lines across areas freely; count commits only WHERE is_primary to avoid
-- counting a multi-area commit more than once.
CREATE OR REPLACE VIEW rpt_commit_work_areas AS
SELECT t.repository_id,
       c.repository_name,
       t.sha,
       c.identity_id,
       c.display_name,
       c.committed_at,
       t.area,
       a.label AS area_label,
       a.kind AS area_kind,
       t.files,
       t.lines,
       t.is_primary,
       t.source,
       t.rules_version,
       t.tagged_at,
       c.ai_tools,
       c.is_fixture
FROM commit_work_areas t
JOIN rpt_commits c ON c.repository_id = t.repository_id AND c.sha = t.sha
JOIN work_areas a ON a.area = t.area;

--changeset dashboard:005-work-area-grants runOnChange:true
GRANT SELECT ON ALL TABLES IN SCHEMA public TO dashboard_read;

--changeset dashboard:005-initial-work-area-tags
-- Tag existing commits once. Afterwards the collector re-tags after every run, the dev-seed
-- refresh tags fixtures, and a migration that edits rules should end with this same SELECT.
-- (Never runAlways: Kubernetes init containers wait for `liquibase status` to report up to
-- date, which a runAlways changeset never does.)
SELECT refresh_work_area_tags();
