# Reporting data contract

The collector stores typed facts in PostgreSQL; reports and the web API read them through the
`rpt_*` views and helper functions defined in `db/changelog/changes/003-reporting-views.sql`.
Those views are the stable contract: columns may be added, but not renamed or removed without a
note here. Claude instances can use the `activity-dataviz` skill (`.claude/skills/`), which
encodes the rules below.

## Coverage

`rpt_collection_status` lists, per source (`commits`, `pull_requests`, `tasks`) and scope
(repository id or project):

- `coverage_start` — earliest instant a successful run (normally a backfill) has covered.
- `watermark` — facts are complete up to (exclusive) this instant.

Report only on `[coverage_start, watermark)`. Work items changed inside the covered period carry
their full history, so task events older than `coverage_start` exist but are incomplete.
Branches deleted or force-pushed before the first collection may be missing from history.

## Views

| View | Grain | Notes |
| --- | --- | --- |
| `rpt_identities` | identity | ADO identities (GUID ids), `unassigned`, `unknown`; `groups`, `emails`, `is_service` |
| `rpt_commits` | unique commit per repository | Deduplicated across branches. Attributed to Git author mapped by email alias; unmapped authors are `git:<email>` with a "(Git)" display name. `ai_tools` from the message. |
| `rpt_commit_files` | file-change occurrence | Non-merge commits only, diff against first parent (empty tree for roots), rename-aware. `category` from `file_category(path)`. |
| `rpt_pull_requests` | pull request | Creator attribution; completion snapshot statistics (merge base → source at completion); `cycle_hours`. |
| `rpt_task_events` | created/completed event | Completion = entering the configured completion states from outside them; assignee at that revision. |
| `rpt_daily_activity` | UTC day × identity | Convenience rollup of the above; commit metrics exclude merges. |
| `rpt_unmapped_git_authors` | Git email | Emails without an alias plus candidate identities by display-name match (suggestions only). |
| `rpt_collection_status` | source × scope | Coverage and freshness. |

## Semantics

- Timestamps are `timestamptz` UTC instants. Commit time is the committer timestamp (author time is also stored).
- Statistics that could not be computed are NULL with a `stats_status` of `partial_binary`, `unavailable`, `pending`, or `merge` — never zero.
- Binary files count toward `files_changed`/`binary_files` but have no line counts.
- Merge commits carry no diff and are excluded from commit counts and churn.
- PR and commit metrics are overlapping views of the same work; never add them together.
- `ai_tools(message)` detects AI co-author trailers and tool banners (Claude, Copilot, Codex/OpenAI, Cursor, Gemini, Aider, Devin). It is a **lower bound**: many people strip trailers.
- `file_category(path)` buckets: `hdl`, `fpga_project`, `software`, `config_build_ui`, `docs`, `generated_vendor`, `binary_asset`, `other`. Exclude `generated_vendor` and `binary_asset` from line-based productivity measures.
- Service/build identities are kept but flagged `is_service`.

## Work areas

Each changed file is classified by `work_area(path)`: the first matching row of
`work_area_rules` (lowest `priority`, PostgreSQL regex on the lowercased path), otherwise
`other`. The starter areas (`work_areas`) are Frontend & UI, Backend & services, Tools &
scripts, Shared libraries (kind `software`); Firmware (`firmware`); FPGA & hardware (`fpga`);
Build & CI, Docs & plans (`support`); Vendored, generated & other (`other`: node_modules,
third_party/, vendor/, lock/minified/designer files, unmatched paths). They are a generic
example: edit the rows and rules to match your repositories' layout (below).

`commit_work_areas` stores one row per commit and area with `files`, `lines`, `is_primary`,
`source` (`rule`, later `llm`/`manual`), `rules_version`, and `tagged_at`. The primary area is the
one with the most changed files (ignoring `other` unless nothing else was touched), so
`count(*) WHERE is_primary` adds up to non-merge commits. `rpt_commit_work_areas` joins identity
and AI markers.

Changing rules is a data edit; the statement trigger bumps `app_metadata.work_area_rules_version`
and `SELECT refresh_work_area_tags()` (also run by every collector run and every migration)
re-tags stale commits:

```sql
INSERT INTO work_area_rules (priority, area, pattern, note) VALUES (45, 'tools', '(^|/)devtools/', 'internal dev tooling');
SELECT refresh_work_area_tags();
SELECT path, work_area(path) FROM (VALUES ('firmware/motor/main.c'), ('web/client/node_modules/x.js')) v(path);
```

## Administration (writes — run deliberately)

Contributor groups are maintained by people, never by the collector:

```sql
INSERT INTO contributor_groups (group_name, description) VALUES ('fpga', 'FPGA engineers');
INSERT INTO contributor_group_members (group_name, identity_id, valid_from)
SELECT 'fpga', identity_id, DATE '2024-10-01' FROM rpt_identities WHERE display_name = 'Jane Example';
```

Map a Git email to an Azure identity (re-credits all history immediately because attribution is
resolved at query time; `manual` aliases are never overwritten by the collector):

```sql
INSERT INTO identity_aliases (email, identity_id, source) VALUES ('jane@personal.example', '<ado-guid>', 'manual')
ON CONFLICT (email) DO UPDATE SET identity_id = excluded.identity_id, source = 'manual', updated_at = now();
```

One person may have several Azure identities (re-created accounts, guest accounts, UPN-reuse
"OIDCONFLICT" duplicates). Merge secondary identities into one canonical identity; all `rpt_*`
views, the dashboard filter, and the contributor list then treat them as one person. Merges are
one level deep (enforced by a trigger) and resolved at query time, so deleting a merge row
undoes it. Each view keeps the pre-merge id in `source_identity_id`.

```sql
INSERT INTO identity_merges (identity_id, canonical_identity_id, note)
VALUES ('<secondary-ado-guid>', '<canonical-ado-guid>', 'why, confirmed by whom, when');
```

Find candidates with `SELECT display_name, array_agg(identity_id) FROM identities WHERE kind = 'ado'
GROUP BY 1 HAVING count(*) > 1` and by comparing `unique_name` values; confirm with the person
before merging. Put group memberships on the canonical identity.

Run these with `docker compose ... exec postgres psql -U activity -d activity` (local) or a
privileged session in the cluster. `scripts/report-query.ps1` is read-only by design.

## Read-only access

Liquibase creates the `NOLOGIN` role `dashboard_read` with SELECT on all tables and views. Grant
it to a login role created outside Git, e.g. `CREATE ROLE reporting LOGIN PASSWORD '…' IN ROLE dashboard_read;`.
