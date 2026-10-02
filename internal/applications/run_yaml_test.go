package applications

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

const nativeDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
spec:
  replicas: 2
  selector:
    matchLabels: {app: web}
  template:
    metadata:
      labels: {app: web}
    spec:
      containers:
      - name: web
        image: nginx:stable
        ports:
        - containerPort: 80
        env:
        - name: PASSWORD
          valueFrom:
            secretKeyRef: {name: passwords, key: web}
        volumeMounts:
        - name: data
          mountPath: /data
      volumes:
      - name: data
        persistentVolumeClaim: {claimName: existing-data}
`

func TestRunYAMLPreservesNativeDeployment(t *testing.T) {
	preview, err := ParseRunYAML(nativeDeployment, "kubernetes")
	if err != nil {
		t.Fatal(err)
	}
	d, err := protocol.RunDeployment(preview.Workloads[0].RunManifest, preview.Workloads[0].Target)
	if err != nil {
		t.Fatal(err)
	}
	if *d.Spec.Replicas != 2 || d.Spec.Selector.MatchLabels["app"] != "web" || d.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort != 80 || d.Spec.Template.Spec.Containers[0].Env[0].ValueFrom.SecretKeyRef.Name != "passwords" || d.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "existing-data" || *d.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatalf("manifest fields lost: %+v", d)
	}
}
func TestRunYAMLRefusesAmbiguityAndPrivilege(t *testing.T) {
	for name, source := range map[string]string{
		"unknown":   strings.Replace(nativeDeployment, "replicas: 2", "replicas: 2\n  unknown: secret-canary", 1),
		"selector":  strings.Replace(nativeDeployment, "matchLabels: {app: web}", "matchLabels: {app: other}", 1),
		"account":   strings.Replace(nativeDeployment, "containers:", "serviceAccountName: powerful\n      containers:", 1),
		"automount": strings.Replace(nativeDeployment, "containers:", "automountServiceAccountToken: true\n      containers:", 1),
		"alias":     strings.Replace(nativeDeployment, "name: web", "name: &alias web", 1),
		"duplicate": strings.Replace(nativeDeployment, "replicas: 2", "replicas: 2\n  replicas: 3", 1),
		"foreign":   strings.Replace(nativeDeployment, "name: web", "name: web\n  labels: {kyyard.busnes.app/application: other}", 1),
		"kind":      strings.Replace(nativeDeployment, "kind: Deployment", "kind: Service", 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseRunYAML(source, "kubernetes")
			if err == nil {
				t.Fatal("accepted unsafe/ambiguous YAML")
			}
			if strings.Contains(err.Error(), "secret-canary") {
				t.Fatal("diagnostic contains source")
			}
		})
	}
}
func TestRunYAMLComposeUsesExistingFrames(t *testing.T) {
	source := `services:
  web:
    image: nginx:stable
    environment: {PASSWORD: "secret-canary"}
    ports: [{target: 80, published: 8080, protocol: tcp}]
    volumes: [existing:/data:ro]
volumes:
  existing: {external: true}
`
	preview, err := ParseRunYAML(source, "docker")
	if err != nil {
		t.Fatal(err)
	}
	c := preview.Containers[0]
	if c.Env[0].Value != "secret-canary" || c.Ports[0].Host != 8080 || c.Mounts[0].Kind != "volume" || !c.Mounts[0].ReadOnly {
		t.Fatal("compose conversion lost fields")
	}
	_, err = ParseRunYAML(strings.Replace(source, "external: true", "external: false", 1), "docker")
	if err == nil {
		t.Fatal("unexpected volume creation")
	}
	body, _ := json.Marshal(preview)
	if len(body) == 0 {
		t.Fatal("empty preview")
	}
}

func TestRunComposeConveniencesKeepArgvAndBindings(t *testing.T) {
	preview, err := ParseRunYAML(`services:
  web:
    image: nginx:stable
    container_name: my-web
    command: [nginx, "-g", "daemon off;"]
    entrypoint: ["/entrypoint"]
    ports: ["127.0.0.1:8080:80/tcp"]
`, "docker")
	if err != nil {
		t.Fatal(err)
	}
	c := preview.Containers[0]
	if c.Name != "my-web" || len(c.Command) != 3 || c.Command[2] != "daemon off;" || c.Entrypoint[0] != "/entrypoint" || c.Ports[0].HostIP != "127.0.0.1" || c.Ports[0].Host != 8080 || c.Ports[0].Container != 80 {
		t.Fatal("run conveniences changed semantics")
	}
}
