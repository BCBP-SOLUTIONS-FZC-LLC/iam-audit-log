{{/*
Expand the name of the chart.
*/}}
{{- define "iam-audit-log.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to
this (by the DNS naming spec).
*/}}
{{- define "iam-audit-log.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "iam-audit-log.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels applied to every resource.
*/}}
{{- define "iam-audit-log.labels" -}}
helm.sh/chart: {{ include "iam-audit-log.chart" . }}
{{ include "iam-audit-log.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels — used by Deployment, Service, HPA, PDB, ServiceMonitor,
and every CronJob (so NetworkPolicy egress rules cover CronJob pods too,
since they select on these same labels).
*/}}
{{- define "iam-audit-log.selectorLabels" -}}
app.kubernetes.io/name: {{ include "iam-audit-log.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use.
*/}}
{{- define "iam-audit-log.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "iam-audit-log.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Image tag: prefer .Values.image.tag; fall back to chart appVersion. One
image carries both binaries (server + reconciler) — the Deployment and all
six CronJobs share this same tag.
*/}}
{{- define "iam-audit-log.imageTag" -}}
{{- .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{/*
Name of the Secret holding application secrets: either a pre-existing,
externally-managed Secret (.Values.existingSecret), or the one this chart
renders itself from .Values.secretValues (see templates/secret.yaml).
Shared by the server Deployment and every reconciler CronJob.
*/}}
{{/*
Per-composition-root Secret names (rule 5 / LLD §3.3.2: the server never
receives the audit_reconciler DSN and the reconciler never receives the
audit_app DSN — separate Secret objects, not just separate env lists).
*/}}
{{- define "iam-audit-log.serverSecretName" -}}
{{- .Values.secrets.server.existingSecret | default (printf "%s-server" (include "iam-audit-log.fullname" .)) }}
{{- end }}

{{- define "iam-audit-log.reconcilerSecretName" -}}
{{- .Values.secrets.reconciler.existingSecret | default (printf "%s-reconciler" (include "iam-audit-log.fullname" .)) }}
{{- end }}
