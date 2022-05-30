{{/*
If runner.name is not provided, use chart name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
*/}}
{{- define "runner.name" -}}
{{- default .Chart.Name .Values.runner.name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Set the namespace to default if not provided
*/}}

{{- define "runner.namespace" -}}
{{- if eq .Values.runner.namespace "" }}
default
{{- else }}
{{- .Values.runner.namespace | trunc 63 | trimSuffix "-" }}
{{- end}}
{{- end}}


