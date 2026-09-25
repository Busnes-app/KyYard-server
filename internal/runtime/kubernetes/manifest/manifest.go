// Package manifest renders the one file an administrator applies to enroll a Kubernetes
// cluster. It imports no Kubernetes library: the server links this package, and the adapter's
// client-go stays in the agent.
package manifest

import (
	"encoding/json"
	"errors"
	"strings"
	"text/template"

	"github.com/Busnes-app/kyyard-server/internal/config"
)

// Input is everything the manifest varies on. Link carries the single-use token; it appears
// once, in the enrollment Secret.
type Input struct {
	Image string
	Link  string
	Name  string
}

// Render returns the multi-document YAML. The image must be digest-pinned: the same rule the
// Docker command follows, because a tag can move after the administrator reviewed it.
func Render(in Input) (string, error) {
	if !config.IsPinnedAgentImage(in.Image) {
		return "", errors.New("the agent image is not pinned to a digest")
	}
	if in.Name == "" || !strings.HasPrefix(in.Link, "https://") {
		return "", errors.New("a manifest needs an endpoint name and an HTTPS enrollment link")
	}
	var b strings.Builder
	err := manifestTemplate.Execute(&b, map[string]string{"Image": in.Image, "Link": in.Link, "Name": in.Name, "File": FileName(in.Name)})
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
# persistentvolumeclaims, deployments, statefulsets and daemonsets in every namespace. It cannot
# read Secrets or ConfigMaps; in its own namespace it may read and write its identity Secret.
# The Secret kyyard-agent-enrollment holds a single-use enrollment link. Once the endpoint is
# approved, delete it: kubectl -n kyyard-agent delete secret kyyard-agent-enrollment
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
`))
