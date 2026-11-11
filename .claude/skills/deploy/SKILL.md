---
name: deploy
description: Build and roll out an Engineering Activity release to Kubernetes with Helm or Argo CD, including image tagging, values, secrets, backfills and post-deploy checks. Use when asked to deploy, upgrade, release, or troubleshoot a cluster install of this app.
---

# Deploy Engineering Activity

Full reference: `docs/deployment.md`. This skill is the checklist.

## Before touching a cluster

1. **Confirm the target** with the user: kube context, namespace, and whether the release is
   managed by **Argo CD** (look for an Argo `Application` that points at
   `helm/engineering-activity`) or by hand with Helm. Never guess a context; check
   `kubectl config get-contexts`. If Argo manages it, do not `helm upgrade` by hand: Argo will
   revert it. Change the values in the GitOps repository instead.
2. **Find the values file.** It is never in this repository. Ask where the operator keeps it.
3. **Read before writing:** `kubectl -n <ns> get deploy,cronjob,jobs,pvc` and the current image tags.

## Release

```bash
tag=$(git rev-parse --short=12 HEAD)        # one tag per commit; never latest
for t in web collector migrations; do
  docker build --platform linux/amd64 --target $t -t "$REG/<repo-prefix>-$t:$tag" . && docker push "$REG/<repo-prefix>-$t:$tag"
done
```

- **Argo CD:** set the three `images.*.tag` values in the GitOps values file to `$tag`, commit,
  push (on a branch with a PR if the user asks), then watch the Application sync. The migration Job
  is an Argo `Sync` hook and re-runs on every sync.
- **Helm by hand:** `helm upgrade activity helm/engineering-activity -n <ns> -f <values> --set-string images.web.tag=$tag,images.collector.tag=$tag,images.migrations.tag=$tag --wait`.

Validate first without a cluster: `helm lint` and `helm template` with the same values.

## After the rollout

- Migration Job completed: `kubectl -n <ns> logs job/<release>-migrate-r<rev>` ends with
  "executed successfully".
- Web Pod runs the new tag; `GET /healthz` returns 200 (other routes return 401 without the
  dashboard login, which is fine).
- Next collector run succeeds: `kubectl -n <ns> get jobs`; `GET /api/v1/status` shows fresh watermarks.

## Rules

- Secrets are created with `kubectl`, never rendered from committed values.
- Never set `migrations.contextFilter` to `dev-seed` in a cluster.
- Selectors are immutable: keep `fullnameOverride` and `nameOverride` matching an existing install.
- Changing PVC sizes or storage classes on a running StatefulSet needs a manual migration; stop and ask.
- Backfills: suspend the CronJob (`collector.suspend=true`) while a backfill Job runs; they share a
  ReadWriteOnce volume.
