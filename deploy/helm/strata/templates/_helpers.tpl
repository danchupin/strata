{{/*
Expand the name of the chart.
*/}}
{{- define "strata.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars (DNS-1123 label limit). If release name contains
chart name, suppress duplication.
*/}}
{{- define "strata.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Chart name + version for app.kubernetes.io/version label.
*/}}
{{- define "strata.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Standard labels applied to every resource.
*/}}
{{- define "strata.labels" -}}
helm.sh/chart: {{ include "strata.chart" . }}
{{ include "strata.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: gateway
{{- end -}}

{{/*
Selector labels — must be stable across upgrades (the Deployment's
selector is immutable, so any drift here breaks `helm upgrade`).
*/}}
{{- define "strata.selectorLabels" -}}
app.kubernetes.io/name: {{ include "strata.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
ServiceAccount name.
*/}}
{{- define "strata.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "strata.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Secret name — when secret.create=false, point at the operator-managed
Secret named in secret.name.
*/}}
{{- define "strata.secretName" -}}
{{- if .Values.secret.create -}}
{{- printf "%s-secrets" (include "strata.fullname" .) -}}
{{- else -}}
{{- required "secret.name required when secret.create=false" .Values.secret.name -}}
{{- end -}}
{{- end -}}

{{/*
ConfigMap name.
*/}}
{{- define "strata.configMapName" -}}
{{- printf "%s-config" (include "strata.fullname" .) -}}
{{- end -}}

{{/*
STRATA_WORKERS joined into a single comma-separated string.
*/}}
{{- define "strata.workersCsv" -}}
{{- join "," .Values.workers -}}
{{- end -}}
