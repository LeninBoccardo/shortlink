{{/*
  Common naming and labels. Keeping these centralised so renaming the chart
  doesn't mean updating every template by hand.
*/}}

{{- define "shortlink.fullname" -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
  Per-component name -- pass the component string in via `dict`:
    {{ include "shortlink.componentName" (dict "root" . "component" "api") }}
*/}}
{{- define "shortlink.componentName" -}}
{{- printf "%s-%s" (include "shortlink.fullname" .root) .component | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
  Labels every object carries. app.kubernetes.io/* are the recommended set
  Helm tooling and the dashboard understand.
*/}}
{{- define "shortlink.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{/*
  Per-component labels. Adds app.kubernetes.io/component so Pod selectors and
  NetworkPolicy can target exactly one workload.
*/}}
{{- define "shortlink.componentLabels" -}}
{{ include "shortlink.labels" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/*
  PostgreSQL DSN pointed at PgBouncer. apps always go through the pooler.
*/}}
{{- define "shortlink.databaseURL" -}}
postgres://{{ .Values.postgres.user }}:{{ .Values.secrets.postgresPassword }}@{{ include "shortlink.componentName" (dict "root" . "component" "pgbouncer") }}:6432/{{ .Values.postgres.database }}?sslmode=disable
{{- end -}}

{{/*
  DSN that bypasses PgBouncer and connects directly to Postgres. Used by the
  migration Job: goose runs DDL inside transactions, which is unsafe under
  transaction-mode pooling. Bypassing the pooler keeps session state stable
  for the entire migration run.
*/}}
{{- define "shortlink.databaseURLDirect" -}}
postgres://{{ .Values.postgres.user }}:{{ .Values.secrets.postgresPassword }}@{{ .Values.postgres.host }}:{{ .Values.postgres.port }}/{{ .Values.postgres.database }}?sslmode=disable
{{- end -}}

{{/*
  Image reference helper: image.repository + "-" + name + ":" + tag.
*/}}
{{- define "shortlink.image" -}}
{{ .Values.image.repository }}-{{ .name }}:{{ .Values.image.tag }}
{{- end -}}

{{/*
  Pod-level securityContext for the shortlink distroless nonroot binaries
  (api, worker, observer, migrate). UID/GID 65532 is the `nonroot` user
  baked into gcr.io/distroless/static-debian12:nonroot. Aligns with the
  Pod Security "restricted" profile.
*/}}
{{- define "shortlink.podSecurityContext" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
fsGroup: 65532
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{/*
  Container-level securityContext for the shortlink distroless binaries.
  Drops every capability, disables privilege escalation, and locks the
  rootfs read-only -- the binaries only need network + /tmp (none of the
  workloads write to the local filesystem outside emptyDirs).
*/}}
{{- define "shortlink.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
runAsNonRoot: true
capabilities:
  drop:
    - ALL
{{- end -}}

{{/*
  Effective SSRF_ALLOWLIST: the user-provided list from
  .Values.config.ssrfAllowlist plus the in-cluster loadtest Service
  hostname *port-pinned to the sink* (so the worker can deliver attack
  webhooks without the SSRF guard rejecting "<release>-shortlink-loadtest"
  as a private DNS name). Port-pinning matters: a bare hostname entry
  would also legitimise webhook URLs pointing at the unauthenticated
  control plane on :8090, which lets any holder of an API key submit
  `webhook_url=http://<loadtest>:8090/api/attack/start` and weaponise
  the operator panel from outside the cluster. The auto-appended entry
  here uses the sink port (8091) only; if you legitimately need other
  ports allowlisted, add them explicitly in .Values.config.ssrfAllowlist.
  Returns a comma-separated string with no leading/trailing comma.
*/}}
{{- define "shortlink.ssrfAllowlist" -}}
{{- $user := .Values.config.ssrfAllowlist | default "" -}}
{{- $loadtest := include "shortlink.componentName" (dict "root" . "component" "loadtest") -}}
{{- $loadtestSink := printf "%s:8091" $loadtest -}}
{{- if $user -}}
{{- printf "%s,%s" $user $loadtestSink -}}
{{- else -}}
{{- $loadtestSink -}}
{{- end -}}
{{- end -}}
