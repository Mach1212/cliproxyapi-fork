{{- define "cliproxyapi.fullname" -}}
{{- .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- end -}}

{{- define "cliproxyapi.labels" -}}
app.kubernetes.io/name: cliproxyapi
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{/* The pod's own label. It is the Deployment's selector, which cannot change once made. */}}
{{- define "cliproxyapi.selector" -}}
app: {{ include "cliproxyapi.fullname" . }}
{{- end -}}

{{/*
The proxy's image. What already runs stays when it is a build of the fork, so that an
upgrade of the chart does not undo what the keeper did (values: image.force).
*/}}
{{- define "cliproxyapi.image" -}}
{{- $wanted := printf "%s:%s" .Values.image.repository .Values.image.tag -}}
{{- if .Values.image.digest -}}
{{- $wanted = printf "%s@%s" $wanted .Values.image.digest -}}
{{- end -}}
{{- $running := "" -}}
{{- if not .Values.image.force -}}
{{- $live := lookup "apps/v1" "Deployment" .Release.Namespace (include "cliproxyapi.fullname" .) -}}
{{- range (dig "spec" "template" "spec" "containers" list $live) -}}
{{- if and (eq .name "cliproxyapi") (hasPrefix (printf "%s:" $.Values.image.repository) .image) -}}
{{- $running = .image -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $running | default $wanted -}}
{{- end -}}

{{/* A helper container's security: nobody, read-only, no privileges. */}}
{{- define "cliproxyapi.restricted" -}}
runAsUser: 99
runAsGroup: 99
runAsNonRoot: true
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities: {drop: ["ALL"]}
{{- end -}}
