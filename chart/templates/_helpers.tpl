{{/* Fixed names: cluster-singleton controller (ClusterRole/Binding). */}}
{{- define "skc.name" -}}simplek8s-controller{{- end }}
{{- define "skc.labels" -}}
app.kubernetes.io/name: simplek8s-controller
app.kubernetes.io/instance: {{ .Release.Name }}
app: simplek8s-controller
{{- end }}
