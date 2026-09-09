{{/*
Expand the name of the chart.
*/}}
{{- define "alertory.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Full resource name.
*/}}
{{- define "alertory.fullname" -}}
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

{{- define "alertory.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "alertory.labels" -}}
helm.sh/chart: {{ include "alertory.chart" . }}
{{ include "alertory.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "alertory.selectorLabels" -}}
app.kubernetes.io/name: {{ include "alertory.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "alertory.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "alertory.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the Secret actually used at runtime - either the one this chart
creates, or the existingSecret the user pointed us at.
*/}}
{{- define "alertory.secretName" -}}
{{- default (include "alertory.fullname" .) .Values.secret.existingSecret }}
{{- end }}

{{/*
Name of the bundled Postgres StatefulSet/Service (see postgresql.yaml).
*/}}
{{- define "alertory.postgresqlFullname" -}}
{{- printf "%s-postgresql" (include "alertory.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
DATABASE_URL for the bundled Postgres, built from postgresql.auth so
alertory can reach it without the user having to duplicate connection
details by hand.
*/}}
{{- define "alertory.postgresqlDatabaseURL" -}}
{{- $database := .Values.postgresqlDatabaseOverride | default .Values.postgresql.auth.database }}
{{- printf "postgres://%s:%s@%s:5432/%s?sslmode=disable" .Values.postgresql.auth.username .Values.postgresql.auth.password (include "alertory.postgresqlFullname" .) $database }}
{{- end }}
