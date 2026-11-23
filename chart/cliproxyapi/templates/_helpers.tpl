{{- define "cliproxyapi.fullname" -}}
{{- .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- end -}}

{{- define "cliproxyapi.labels" -}}
app.kubernetes.io/name: cliproxyapi
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{/* The proxy pod's own label. It is the Deployment's selector, which cannot change once made. */}}
{{- define "cliproxyapi.selector" -}}
app: {{ include "cliproxyapi.fullname" . }}
{{- end -}}

{{/* The gate pod's own label: neither Deployment may select the other's pod. */}}
{{- define "cliproxyapi.gateSelector" -}}
app: {{ include "cliproxyapi.fullname" . }}-gate
{{- end -}}

{{/*
What the two HAProxys have in common: the gate, which clients reach, and the relay beside the
proxy, which the gate reaches. Each connects to its next hop anew for every request and,
while that hop restarts, tries again about once a second. Nothing of the request was sent by
then, so nothing is sent twice.
*/}}
{{- define "cliproxyapi.haproxy.defaults" -}}
defaults
  mode http
  log global
  option httplog
  option http-server-close
  retries {{ .Values.gate.retries }}
  timeout connect 5s
  timeout client 1h
  timeout server 1h
  timeout tunnel 2h
  timeout http-keep-alive 5s
  # A connection that sends nothing (or half its headers) would otherwise keep a hold
  # waiting for `timeout client`.
  timeout http-request 10s
{{- end -}}

{{/* The rules a request passes at the gate, and once more at the relay. */}}
{{- define "cliproxyapi.haproxy.rules" -}}
http-request normalize-uri path-merge-slashes
http-request normalize-uri percent-decode-unreserved
http-request normalize-uri path-strip-dotdot
# The proxy routes on the decoded path, so no %-encoding gets through.
http-request deny if { path -m sub % }
http-request deny unless { path -m reg ^/v1(beta)?/ }
http-request del-header X-Forwarded-For
http-request del-header X-Real-IP
http-request del-header Forwarded
{{- end -}}

{{/* The gate's configuration. Its pod restarts when this changes (templates/gate.yaml). */}}
{{- define "cliproxyapi.gate.cfg" -}}
global
  # normalize-uri and the runtime `set var` are experimental in 3.2.
  expose-experimental-directives
  log stdout format raw local0 info
  stats socket ipv4@127.0.0.1:9999 level admin
  stats timeout 1h

{{ include "cliproxyapi.haproxy.defaults" . }}

frontend api
  # HTTP/1 only: a hold cannot close h2c connections.
  bind :{{ .Values.gate.port }} proto h1
  {{- include "cliproxyapi.haproxy.rules" . | nindent 2 }}
  # Hold: a response that starts now closes its connection, and an idle connection closes
  # after `timeout http-keep-alive`, so every client's next request waits with the new
  # connections.
  http-after-response set-header Connection close if { var(proc.holding) -m int 1 }
  default_backend proxy

frontend keeper
  # For the keeper alone, on loopback: the same way to the proxy while the frontend above is
  # held.
  bind 127.0.0.1:8316 proto h1
  default_backend proxy

backend proxy
  # The relay in the proxy's pod, by its Service. The name is looked up when the gate starts.
  server relay {{ include "cliproxyapi.fullname" . }}-relay:{{ .Values.relay.port }}
{{- end -}}

{{/* The relay's configuration. The proxy's pod restarts when this changes. */}}
{{- define "cliproxyapi.relay.cfg" -}}
global
  # normalize-uri is experimental in 3.2.
  expose-experimental-directives
  log stdout format raw local0 info

{{ include "cliproxyapi.haproxy.defaults" . }}

frontend gate
  bind :{{ .Values.relay.port }} proto h1
  # The gate logs every request. Here only the ones that went wrong.
  option dontlog-normal
  {{- include "cliproxyapi.haproxy.rules" . | nindent 2 }}
  default_backend proxy

backend proxy
  server local 127.0.0.1:{{ .Values.proxy.port }}
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
