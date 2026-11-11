{{- define "activity.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 40 | trimSuffix "-" -}}
{{- else if contains "activity" .Release.Name -}}
{{- .Release.Name | trunc 40 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-activity" .Release.Name | trunc 40 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/* Selector name. Existing installs that predate the chart rename keep their old value via
nameOverride, because Deployment and StatefulSet selectors cannot change in place. */}}
{{- define "activity.name" -}}
{{- .Values.nameOverride | default .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "activity.labels" -}}
app.kubernetes.io/name: {{ include "activity.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "activity.selector" -}}
app.kubernetes.io/name: {{ include "activity.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* image renders registry/repository:tag for one of web, collector, migrations. */}}
{{- define "activity.image" -}}
{{- $img := index .root.Values.images .name -}}
{{- $registry := .root.Values.images.registry -}}
{{- $repo := required (printf "images.%s.repository must be set" .name) $img.repository -}}
{{- $tag := required (printf "images.%s.tag must be set (no floating tags)" .name) $img.tag -}}
{{- if $registry -}}{{ printf "%s/%s:%s" $registry $repo $tag }}{{- else -}}{{ printf "%s:%s" $repo $tag }}{{- end -}}
{{- end -}}

{{- define "activity.secretName" -}}
{{- if .Values.secrets.create -}}{{ include "activity.fullname" . }}-secrets{{- else -}}{{ required "secrets.existingSecret must be set when secrets.create is false" .Values.secrets.existingSecret }}{{- end -}}
{{- end -}}

{{- define "activity.dbHost" -}}
{{- if .Values.postgresql.enabled -}}{{ include "activity.fullname" . }}-postgres{{- else -}}{{ required "database.host must be set when postgresql.enabled is false" .Values.database.host }}{{- end -}}
{{- end -}}

{{/* Environment shared by every container that talks to PostgreSQL. The password must be
URL-safe (for example hex) because it is interpolated into DATABASE_URL. */}}
{{- define "activity.dbEnv" -}}
- name: DATABASE_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "activity.secretName" . }}
      key: {{ .Values.secrets.keys.databasePassword }}
- name: DATABASE_URL
  value: {{ printf "postgres://%s:$(DATABASE_PASSWORD)@%s:%v/%s?sslmode=%s" .Values.database.user (include "activity.dbHost" .) .Values.database.port .Values.database.name .Values.database.sslmode | quote }}
{{- end -}}

{{- define "activity.liquibaseEnv" -}}
- name: LIQUIBASE_COMMAND_URL
  value: {{ printf "jdbc:postgresql://%s:%v/%s?sslmode=%s" (include "activity.dbHost" .) .Values.database.port .Values.database.name .Values.database.sslmode | quote }}
- name: LIQUIBASE_COMMAND_USERNAME
  value: {{ .Values.database.user | quote }}
- name: LIQUIBASE_COMMAND_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "activity.secretName" . }}
      key: {{ .Values.secrets.keys.databasePassword }}
- name: LIQUIBASE_COMMAND_CHANGELOG_FILE
  value: db.changelog-master.xml
- name: LIQUIBASE_SEARCH_PATH
  value: /liquibase/changelog
- name: LIQUIBASE_COMMAND_CONTEXT_FILTER
  value: {{ required "migrations.contextFilter must be set" .Values.migrations.contextFilter | quote }}
- name: LIQUIBASE_ANALYTICS_ENABLED
  value: "false"
{{- end -}}

{{/* initContainer that blocks until every changeset in the packaged changelog is applied.
Gates workloads on the schema version shipped with this chart revision. */}}
{{- define "activity.waitForSchema" -}}
- name: wait-for-schema
  image: {{ include "activity.image" (dict "root" . "name" "migrations") }}
  imagePullPolicy: {{ .Values.images.pullPolicy }}
  command: ["/bin/bash", "-c"]
  args:
    - |
      for i in $(seq 1 {{ .Values.migrations.waitAttempts }}); do
        if liquibase status 2>/dev/null | grep -q "is up to date"; then echo "schema up to date"; exit 0; fi
        echo "waiting for database migrations ($i)"; sleep 10
      done
      echo "schema not up to date; check the migration Job logs" >&2; exit 1
  env:
    {{- include "activity.liquibaseEnv" . | nindent 4 }}
  resources:
    {{- toYaml .Values.migrations.resources | nindent 4 }}
  securityContext:
    {{- toYaml .Values.migrations.securityContext | nindent 4 }}
{{- end -}}

{{- define "activity.imagePullSecrets" -}}
{{- with .Values.images.pullSecrets }}
imagePullSecrets:
{{- range . }}
  - name: {{ . }}
{{- end }}
{{- end }}
{{- end -}}

{{/* Collector pod spec shared by the CronJob and the backfill Job. Args are passed in. */}}
{{- define "activity.collectorPod" -}}
{{- $root := .root -}}
{{- $v := $root.Values -}}
restartPolicy: Never
{{- include "activity.imagePullSecrets" $root | nindent 0 }}
securityContext:
  runAsNonRoot: true
  runAsUser: 10001
  runAsGroup: 10001
  fsGroup: 10001
  fsGroupChangePolicy: OnRootMismatch
  seccompProfile:
    type: RuntimeDefault
{{- with $v.collector.nodeSelector }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $v.collector.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
initContainers:
  {{- include "activity.waitForSchema" $root | nindent 2 }}
containers:
  - name: collector
    image: {{ include "activity.image" (dict "root" $root "name" "collector") }}
    imagePullPolicy: {{ $v.images.pullPolicy }}
    args:
      {{- toYaml .args | nindent 6 }}
    env:
      {{- include "activity.dbEnv" $root | nindent 6 }}
      - name: AZURE_DEVOPS_PAT
        valueFrom:
          secretKeyRef:
            name: {{ include "activity.secretName" $root }}
            key: {{ $v.secrets.keys.azureDevOpsPat }}
      - name: AZURE_DEVOPS_ORG
        value: {{ required "azureDevOps.organization must be set" $v.azureDevOps.organization | quote }}
      - name: AZURE_DEVOPS_PROJECT
        value: {{ required "azureDevOps.project must be set" $v.azureDevOps.project | quote }}
      - name: AZURE_DEVOPS_REPOSITORIES
        value: {{ required "azureDevOps.repositories must list at least one repository" $v.azureDevOps.repositories | join "," | quote }}
      - name: WORK_ITEM_TYPES
        value: {{ join "," $v.azureDevOps.workItemTypes | quote }}
      - name: COMPLETION_STATES
        value: {{ join "," $v.azureDevOps.completionStates | quote }}
      - name: SYNC_OVERLAP
        value: {{ $v.collector.overlap | quote }}
      - name: SYNC_BOOTSTRAP
        value: {{ $v.collector.bootstrap | quote }}
      - name: RECONCILE_EVERY
        value: {{ $v.collector.reconcileEvery | quote }}
      - name: RECONCILE_WINDOW
        value: {{ $v.collector.reconcileWindow | quote }}
      - name: COLLECTOR_CONCURRENCY
        value: {{ $v.collector.concurrency | quote }}
      - name: GIT_MIRROR_DIR
        value: /data/mirrors
      - name: HOME
        value: /tmp
    resources:
      {{- toYaml .resources | nindent 6 }}
    securityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities:
        drop: ["ALL"]
    volumeMounts:
      - name: mirror
        mountPath: /data
      - name: tmp
        mountPath: /tmp
volumes:
  - name: mirror
    persistentVolumeClaim:
      claimName: {{ $v.collector.mirror.existingClaim | default (printf "%s-git-mirror" (include "activity.fullname" $root)) }}
  - name: tmp
    emptyDir:
      sizeLimit: 2Gi
{{- end -}}
