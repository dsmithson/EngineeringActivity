# Architecture and counting rules

## Objective

Show development activity around the clock, filtered by Azure DevOps identity or group. Code scope is one or more repositories in a project; task scope is the whole project, because Azure Boards tasks belong to a project rather than to a Git repository. A narrower area-path or linked-repository filter would need an explicit rule.

## Why a standalone app

A standalone Go web application and collector backed by PostgreSQL, deployed with Helm, rather than an Azure DevOps extension: no publisher setup, and the layout is unconstrained. Link to it from Azure DevOps. Native Azure DevOps Analytics charts use daily snapshots, which cannot show hourly activity. A custom extension could later embed this service inside Azure DevOps without replacing the collector.

## Proposed charts

Every chart shares an identity selector, timezone, and time range. Default timezone: America/Phoenix. Default range: 168 consecutive hourly buckets, ending with the current, explicitly marked partial hour. Show zero-activity hours and midnight boundaries; never compress overnight hours or weekends. Store timestamps in UTC.

| Area | Charts | Attribution |
| --- | --- | --- |
| Pull requests | Created/completed PR counts; lines added/deleted; files changed; commits per PR | PR creator by default; completion actor is a separate dimension |
| Commits across branches | Unique commit count; lines added/deleted; files changed | Git author mapped to Azure identity |
| Tasks | Stacked hourly created and entered-completion counts | Task assignee at the time of the event |

PR size should be shown at a clearly selected event (creation or completion), with the corresponding diff snapshot. Prefer completed PR size for the initial implementation because it represents delivered changes. Never silently attach today's growing PR diff to its creation hour. PR counts can still include both creation and completion events.

Lines means text additions and deletions, not repository size or net lines alone. File totals mean file-change occurrences summed over events; a file touched in three commits counts three times. Binary files contribute to file counts but have unavailable line counts. Missing statistics must remain unknown rather than becoming zero.

## Counting rules

* Deduplicate commits by repository ID and SHA across all branch refs. A commit reachable from several branches counts once. Track branch membership separately.
* Define commit time explicitly: Git committer timestamp for the initial activity view, with author timestamp and first-seen/push time stored separately. Push activity is a different metric. Historical Git timestamps can be rewritten.
* Default commit churn excludes merge commits to avoid repeating branch changes; expose merge count separately. Squash and cherry-picked commits have distinct SHAs and cannot safely be assumed identical work.
* Compute commit text statistics with Git numstat against the parent (empty tree for root commits), and PR statistics from the appropriate base/source snapshot. Do not sum commit diffs to approximate the PR net diff.
* Fetch all relevant branch refs and preserve observed history. Initial backfill cannot guarantee recovery of deleted branches or unreachable, force-pushed commits. Report this coverage limit.
* Count a task as created at System.CreatedDate.
* Attribute creation to System.AssignedTo in the creation revision and completion to System.AssignedTo in the revision entering the completion set. If assignment and state change together, use the resulting assignee in that revision. Later reassignment never changes earlier attribution. Keep genuinely unassigned events under an explicit Unassigned identity; distinguish unavailable historical assignment as Unknown rather than substituting the current assignee or change actor.
* Define the completion set (configurable; default `ready for qa test build`, `ready to test`, `closed`) case-insensitively. The collector validates the configured states against the live workflow and fails fast on a mismatch, so a typo can never silently count nothing.
* Count a completion event only when moving from outside that set into it. Moving between members does not produce another completion. Reopening and subsequently reentering counts as another completion event; label the chart as events, not unique tasks.
* Reconstruct transitions from revisions/updates, using the revision's actual System.ChangedDate. Do not infer history from a task's current state or blindly use update revisedDate, which can contain a sentinel date.
* The stacked task chart represents activity volume. Creation plus completion does not equal net backlog change. Give each series its own total and tooltip.
* Map identities by Azure identity ID and verified email aliases, never display name alone. Preserve unmapped Git authors and service identities. Activity credited to a human account does not establish whether Claude, Codex, or the human performed it.

## Service design

One Go codebase, with separate `serve` and `collect` commands. Embed the browser assets in the server. A scheduled collector reads Azure DevOps REST APIs and a local Git mirror, persists normalized events/statistics in PostgreSQL, and updates collection checkpoints only after successful transactions.

Suggested tables: identities, identity_aliases, repositories, commits, commit_branch_observations, pull_requests, pr_snapshots, task_events, collection_checkpoints, collection_runs. Use source IDs and revision numbers as unique keys so retries and overlapping collection windows are idempotent.

Collection must implement pagination, bounded concurrency, cancellation, retry/backoff including Retry-After, overlapping incremental windows, and periodic reconciliation. Split work-item query time windows if service query limits are reached. Fetch historical revisions for changed tasks, not only latest snapshots. Track source-specific success and failure so partial data is visible.

Suggested read API: `/api/identities`, `/api/activity`, `/api/status`, `/healthz`, and `/readyz`. Activity responses include bucket boundaries, coverage, timezone, metric definitions, and latest successful collection timestamps. Aggregate into a generated hourly series so empty hours are present. Do not add PR and commit totals together: those are overlapping views of activity.

Use read-only Azure DevOps access for collection. Keep credentials in Kubernetes Secrets or an external secret provider, outside Git and browser responses. Select interactive authentication for setup independently from unattended collector authentication. Protect the dashboard with office ingress/SSO appropriate to repository visibility.

## Helm deployment design

The chart should provide a web Deployment/Service, optional Ingress, collector CronJob with `concurrencyPolicy: Forbid`, persistent Git mirror storage, migration Job, resource requests/limits, and readiness/liveness probes. Also use a database advisory lock to protect manual and scheduled collectors from overlapping.

Support an existing PostgreSQL service for production and an optional development PostgreSQL StatefulSet/PVC. Store schema migrations in this repository. Deployment documentation must cover backups/restores, storage class, retention, and upgrades; a PVC alone is not a backup.

Keep cluster-specific settings in your own values file without secrets, outside this repository (see [deployment](deployment.md)).

## Primary references

* [Azure DevOps dashboard design and daily snapshots](https://learn.microsoft.com/en-us/azure/devops/report/dashboards/dashboard-focus?view=azure-devops)
* [Query-based charts](https://learn.microsoft.com/en-us/azure/devops/report/dashboards/charts?view=azure-devops)
* [Custom dashboard widgets](https://learn.microsoft.com/en-us/azure/devops/extend/develop/add-dashboard-widget?view=azure-devops)
* [Dashboard widget REST API](https://learn.microsoft.com/en-us/rest/api/azure/devops/dashboard/widgets?view=azure-devops-rest-7.1)
* [Commit changes REST API](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/commits/get-changes?view=azure-devops-rest-7.1)
* [Work item updates REST API](https://learn.microsoft.com/en-us/rest/api/azure/devops/wit/updates/list?view=azure-devops-rest-7.1)
* [Microsoft Azure DevOps MCP server](https://github.com/microsoft/azure-devops-mcp)
