// Package manifest renders the one file an administrator applies to enroll a Kubernetes
// cluster. It imports no Kubernetes library: the server links this package, and the adapter's
// client-go stays in the agent.
package manifest

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"text/template"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/config"
)

// MaxNamespaces bounds the namespaces one manifest grants writes in.
const MaxNamespaces = 32

// Input is everything the manifest varies on. Link carries the single-use token; it appears
// once, in the enrollment Secret. Namespaces, sorted and distinct, each get the deploy Role.
type Input struct {
	Image      string
	Link       string
	Name       string
	Namespaces []string
}

// Render returns the multi-document YAML. The image must be digest-pinned: the same rule the
// Docker command follows, because a tag can move after the administrator reviewed it.
func Render(in Input) (string, error) {
	if !config.IsPinnedAgentImage(in.Image) {
		return "", errors.New("the agent image is not pinned to a digest")
	}
	if !strings.HasPrefix(in.Link, "https://") {
		return "", errors.New("a manifest needs an HTTPS enrollment link")
	}
	return execute(in)
}

// RenderRBAC returns the manifest without the enrollment Secret and the agent Deployment: what
// an administrator applies to change the namespaces an enrolled agent may write in.
func RenderRBAC(name string, namespaces []string) (string, error) {
	return execute(Input{Name: name, Namespaces: namespaces})
}

func execute(in Input) (string, error) {
	if in.Name == "" {
		return "", errors.New("a manifest needs an endpoint name")
	}
	if len(in.Namespaces) > MaxNamespaces || !slices.IsSorted(in.Namespaces) || len(slices.Compact(slices.Clone(in.Namespaces))) != len(in.Namespaces) || slices.ContainsFunc(in.Namespaces, func(ns string) bool { return !protocol.ValidDNSLabel(ns) }) {
		return "", errors.New("namespaces are distinct DNS labels, sorted, at most 32")
	}
	var b strings.Builder
	err := manifestTemplate.Execute(&b, struct {
		Input
		File string
	}{in, FileName(in.Name)})
	return b.String(), err
}

// FileName is kyyard-agent-<name>.yaml with the name reduced to lower-case letters, digits and
// single hyphens, so the kubectl command is safe to paste into any shell.
func FileName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	slug := strings.TrimSuffix(b.String(), "-")
	if slug == "" {
		slug = "endpoint"
	}
	return "kyyard-agent-" + slug + ".yaml"
}

// quote makes any string one YAML double-quoted scalar: JSON string escapes are a subset of
// YAML's, so a name holding quotes, colons or a hash cannot become structure.
func quote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

var manifestTemplate = template.Must(template.New("manifest").Funcs(template.FuncMap{"q": quote}).Parse(`# KyYard agent for endpoint {{q .Name}}.
# Apply as cluster-admin: kubectl apply -f {{.File}}
# The agent reads get/list on namespaces, nodes, pods, pod logs, events, services,
# persistentvolumeclaims, deployments, statefulsets, daemonsets and storageclasses in every
# namespace. It cannot read Secrets or ConfigMaps; in its own namespace it may read and write its
# identity Secret.
{{- if .Namespaces}}
# In each namespace listed below (Role kyyard-agent-deploy) it may create, update and delete
# Deployments, Services, ConfigMaps and Secrets; Secrets are read by name, never listed. It may
# create PersistentVolumeClaims but never update or delete one: a claim KyYard created stays
# until you delete it. Create the namespaces first. A namespace dropped from a later manifest
# keeps its Role until you run
# kubectl -n <namespace> delete role,rolebinding kyyard-agent-deploy
{{- end}}
{{- if .Link}}
# The Secret kyyard-agent-enrollment holds a single-use enrollment link. Once the endpoint is
# approved, delete it: kubectl -n kyyard-agent delete secret kyyard-agent-enrollment
{{- end}}
# Uninstall: kubectl delete -f {{.File}}
apiVersion: v1
kind: Namespace
metadata:
  name: kyyard-agent
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: kyyard-agent
  namespace: kyyard-agent
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kyyard-agent-read
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
rules:
  - apiGroups: [""]
    resources: [namespaces, nodes, pods, pods/log, events, services, persistentvolumeclaims]
    verbs: [get, list]
  - apiGroups: [apps]
    resources: [deployments, statefulsets, daemonsets]
    verbs: [get, list]
  # A migration's storage choices pick from these.
  - apiGroups: [storage.k8s.io]
    resources: [storageclasses]
    verbs: [get, list]
  # The agent asks the API server what it may do before every apply.
  - apiGroups: [authorization.k8s.io]
    resources: [selfsubjectaccessreviews]
    verbs: [create]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kyyard-agent-read
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kyyard-agent-read
subjects:
  - kind: ServiceAccount
    name: kyyard-agent
    namespace: kyyard-agent
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kyyard-agent-identity
  namespace: kyyard-agent
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
rules:
  # create cannot be limited by name; the namespace holds nothing but the agent's own Secrets.
  - apiGroups: [""]
    resources: [secrets]
    verbs: [get, create]
  - apiGroups: [""]
    resources: [secrets]
    resourceNames: [kyyard-agent-identity]
    verbs: [update]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: kyyard-agent-identity
  namespace: kyyard-agent
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kyyard-agent-identity
subjects:
  - kind: ServiceAccount
    name: kyyard-agent
    namespace: kyyard-agent
{{- range .Namespaces}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kyyard-agent-deploy
  namespace: {{q .}}
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
rules:
  - apiGroups: [apps]
    resources: [deployments]
    verbs: [get, list, create, update, patch, delete]
  - apiGroups: [""]
    resources: [services, configmaps]
    verbs: [get, list, create, update, patch, delete]
  # No list: the agent reads each Secret it owns by name.
  - apiGroups: [""]
    resources: [secrets]
    verbs: [get, create, update, patch, delete]
  # Claims are created once and kept: no update, patch or delete.
  - apiGroups: [""]
    resources: [persistentvolumeclaims]
    verbs: [get, list, create]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: kyyard-agent-deploy
  namespace: {{q .}}
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kyyard-agent-deploy
subjects:
  - kind: ServiceAccount
    name: kyyard-agent
    namespace: kyyard-agent
{{- end}}
{{- if .Link}}
---
apiVersion: v1
kind: Secret
metadata:
  name: kyyard-agent-enrollment
  namespace: kyyard-agent
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
type: Opaque
stringData:
  link: {{q .Link}}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kyyard-agent
  namespace: kyyard-agent
  labels:
    app.kubernetes.io/name: kyyard-agent
    app.kubernetes.io/managed-by: kyyard
spec:
  # One replica, replaced rather than rolled: two agents would share one identity.
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app.kubernetes.io/name: kyyard-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: kyyard-agent
        app.kubernetes.io/managed-by: kyyard
    spec:
      serviceAccountName: kyyard-agent
      automountServiceAccountToken: true
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: agent
          image: {{q .Image}}
          command: ["/app/kyyard-agent"]
          args: ["--kubernetes", "--link-file", "/etc/kyyard/link", "--identity-secret", "kyyard-agent-identity", "--name", {{q .Name}}, "--docker-socket="]
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
            limits:
              cpu: 500m
              memory: 256Mi
          volumeMounts:
            - name: enrollment
              mountPath: /etc/kyyard
              readOnly: true
      volumes:
        - name: enrollment
          secret:
            secretName: kyyard-agent-enrollment
            # Optional: the Secret is deleted once spent, and a restart must not wait for it.
            optional: true
            defaultMode: 0440
{{- end}}
`))
