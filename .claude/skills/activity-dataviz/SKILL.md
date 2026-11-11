---
name: activity-dataviz
description: Build reports, charts, and dashboards from the Engineering Activity (Azure DevOps + Git) database (commits, PRs, task events, per-file churn, AI co-author markers, contributor groups such as FPGA engineers). Use whenever someone asks for productivity/throughput/AI-adoption analysis, trends over months, per-person or per-group comparisons, HDL vs software code mix, PR cycle time, or any visualization of the collected ADO/Git activity in this repository.
---

# Activity data → reports and visualizations

This repo collects Azure DevOps + Git activity into PostgreSQL (see `docs/reporting.md` for the
full data contract and `docs/architecture.md` for counting rules). Your job with this skill:
answer the question with **correct, honestly-caveated numbers**, then present them well.

## 1. Pick the database and check coverage first

| Target | What it holds | How to query |
| --- | --- | --- |
| `live` (default) | Real data backfilled from ADO (`compose.live.yaml`, project `activity-live`) | `./scripts/report-query.ps1 -Target live -Query "..."` |
| `dev` | Deterministic fixture data (`fixture:*` people) for building/testing reports offline | `./scripts/report-query.ps1 -Target dev -Query "..."` |
| cluster | Production; use a read-only login granted `dashboard_read` | see `docs/deployment.md` |

`report-query.ps1` runs one SELECT inside a READ ONLY transaction via the Compose postgres
container (`-Format csv|json|table`, `-OutFile path`). Start the stack if needed:
`docker compose --env-file .env.live -f compose.yaml -f compose.live.yaml up -d postgres`.

**Always run this before analysis** and state the covered period in your output:

```sql
SELECT source, scope, coverage_start, watermark, last_success_at, last_outcome FROM rpt_collection_status
```

Only report on `[coverage_start, watermark)`. Work items changed inside coverage carry their full
history, so `rpt_task_events` has rows older than `coverage_start` — those older periods are
**incomplete**; clip to coverage. Missing data is not zero activity.

## 2. Query the stable `rpt_*` views (not base tables)

| View | Grain | Key columns |
| --- | --- | --- |
| `rpt_identities` | person/identity | `identity_id, display_name, kind, is_service, groups text[], emails text[]` |
| `rpt_commits` | unique commit (deduped across branches) | `identity_id, display_name, committed_at, is_merge, additions, deletions, files_changed, binary_files, stats_status, ai_tools text[], subject, repository_name` |
| `rpt_commit_files` | file change in a non-merge commit | `path, category, additions, deletions, is_binary, identity_id, committed_at, ai_tools` |
| `rpt_pull_requests` | PR | `identity_id` (creator), `created_at, completed_at, cycle_hours, status, additions, deletions, files_changed, commit_count, stats_status` |
| `rpt_task_events` | task created/completed event | `event_type, occurred_at, identity_id` (assignee at that revision), `work_item_type, area_path` |
| `rpt_commit_work_areas` | commit × work area | `area, area_label, area_kind` (software/firmware/fpga/support/other), `files, lines, is_primary` (count commits only where primary), `rules_version, tagged_at`, identity, `ai_tools` |
| `rpt_daily_activity` | UTC day × identity | commits, merge_commits, commit_additions/deletions/files, ai_marked_commits, prs_created/completed, pr_additions/deletions/files, tasks_created/completed |
| `rpt_unmapped_git_authors` | Git email without identity | commit counts, `candidate_identity_ids` (name matches — suggestions only) |

Work areas come from editable path rules (`work_area_rules`, see `docs/reporting.md`); use `work_area(path)` on `rpt_commit_files.path` for file-level area. For "FPGA engineers doing software", use areas of kind `software` (firmware areas are kind `firmware`, not software), and exclude `other` (vendored drops) from line counts.

Helper functions: `file_category(path)` → `hdl | fpga_project | software | config_build_ui | docs | generated_vendor | binary_asset | other`; `ai_tools(message)`.

Cohorts live in `contributor_groups` / `contributor_group_members` (e.g. `fpga`). People are added
by humans — never invent memberships. If a requested group is empty, list `rpt_identities` and ask.

## 3. Counting rules you must respect

- **Commits**: exclude `is_merge` from counts and churn (report merges separately if useful). Time axis is `committed_at`; attribution is Git author → Azure identity via email aliases.
- **Lines**: text additions/deletions only; binary files have NULL lines. `stats_status <> 'ok'` means incomplete — say how many.
- **Churn noise**: exclude `category IN ('generated_vendor','binary_asset')` from line-based productivity metrics and say so. Check for single huge commits (vendored IP cores, regenerated files) with a top-N query before trusting totals; prefer medians/percentiles over sums when comparing people.
- **PRs and commits overlap** — never add PR lines to commit lines.
- **Tasks completed** are completion *events* (entry into the completion states; reopen → can complete again), attributed to the assignee at that revision.
- **Service accounts**: exclude `rpt_identities.is_service` and `git:` build bots unless asked.
- **Unmapped Git authors** (`identity_id LIKE 'git:%'`) may be the same person as an ADO identity. Check `rpt_unmapped_git_authors` and mention material gaps; mapping requires a confirmed `identity_aliases` row (`source='manual'`), which is a write — ask the user first.
- **One person, several ADO identities**: `identity_merges` folds secondary identities into a canonical one; the `rpt_*` views already return the canonical `identity_id` (pre-merge id in `source_identity_id`). Duplicate display names in `identities` hint at unmerged people — mention them, don't merge without confirmation (see `docs/reporting.md`). Join groups on the canonical id.

## 4. AI-productivity analysis: be rigorous and fair

`ai_tools` / `ai_marked_commits` detect co-author trailers and tool banners. This is a **lower
bound** — many people (including this repo's owner) strip trailers. Never present "no marker"
as "no AI". Defensible approaches, in order of preference:

1. **Before/after on throughput** per person or group, using an adoption date the user supplies (or the first month with sustained AI markers, labeled as an estimate). Compare monthly medians of non-merge commits, PRs completed, and non-noise lines; show the distribution, not just the mean.
2. **Composition shift**: share of `software` vs `hdl`/`fpga_project` lines for FPGA engineers over time (FPGA engineers shipping software is a headline signal) from `rpt_commit_files`.
3. **Marker share**: AI-marked commits / all commits per month, explicitly as a floor.
4. **Normalize** by active contributors per month (people with ≥1 commit) so team growth isn't read as productivity growth.

Caveats to state in every AI report: correlation not causation; lines of code are a weak proxy
for value; agents acting under a person's account are credited to that person; 24 months of
history depends on branches that still exist (deleted/force-pushed history may be missing).
Comparisons of named individuals are sensitive — default to group/anonymized views unless the
user asks for names.

## 5. Recipes

Monthly per-group throughput (non-merge, noise-excluded lines):
```sql
WITH f AS (
  SELECT date_trunc('month', committed_at) AS month, identity_id, sha,
         sum(additions) FILTER (WHERE category NOT IN ('generated_vendor','binary_asset')) AS lines_added
  FROM rpt_commit_files GROUP BY 1, 2, 3)
SELECT f.month, g.group_name, count(DISTINCT f.sha) AS commits, count(DISTINCT f.identity_id) AS active_people,
       sum(f.lines_added) AS lines_added,
       round(count(DISTINCT f.sha)::numeric / nullif(count(DISTINCT f.identity_id), 0), 1) AS commits_per_person
FROM f JOIN contributor_group_members g USING (identity_id)
GROUP BY 1, 2 ORDER BY 1, 2
```
(Note: `rpt_commit_files` has no rows for commits with zero file changes; use `rpt_commits` for pure counts.)

Code mix for a group over time:
```sql
SELECT date_trunc('quarter', f.committed_at) AS quarter, f.category, sum(coalesce(f.additions,0) + coalesce(f.deletions,0)) AS churn
FROM rpt_commit_files f JOIN contributor_group_members g ON g.identity_id = f.identity_id AND g.group_name = 'fpga'
WHERE f.category NOT IN ('generated_vendor', 'binary_asset') GROUP BY 1, 2 ORDER BY 1, 2
```

AI marker floor per month:
```sql
SELECT date_trunc('month', committed_at) AS month, count(*) FILTER (WHERE cardinality(ai_tools) > 0) AS ai_marked,
       count(*) AS commits FROM rpt_commits WHERE NOT is_merge GROUP BY 1 ORDER BY 1
```

PR cycle time (median hours) by month: `percentile_cont(0.5) WITHIN GROUP (ORDER BY cycle_hours)` over `rpt_pull_requests WHERE completed_at IS NOT NULL`.

Outlier check: `SELECT sha, display_name, additions, subject FROM rpt_commits WHERE NOT is_merge ORDER BY additions DESC NULLS LAST LIMIT 20`.

The web API also serves bucketed series: `GET /api/v1/activity?range=7d|30d|12m|24m&tz=America/Phoenix` (aligned hourly/daily/weekly windows, as on the dashboard) or `?bucket=day|week|month&from=YYYY-MM-DD&to=YYYY-MM-DD`, filtered by `group=fpga`, `user=<id>`, or `repo=<uuid>`; `collectedFrom`/`collectedThrough` give coverage (same fields as the dashboard buckets, including `aiMarkedCommits`).

## 6. Presenting results

- For any chart, load the `dataviz` skill first; for a shareable page, use the Artifact tool (load `artifact-design`). Embed the queried data as JSON in the page; do not make the page call the database.
- Every report states: data source (live vs fixture), coverage window, exclusions (merges, noise categories, service accounts), the AI-marker lower-bound caveat, and unknown-statistics counts.
- Keep timestamps UTC in data; label displays in America/Phoenix unless asked otherwise.
- Save reusable SQL under `reports/sql/<name>.sql` so the next report can rerun it.
