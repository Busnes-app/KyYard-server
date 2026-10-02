package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestManifestRunFrameAndSummaryMustAgree(t *testing.T) {
	raw := json.RawMessage(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"shop"},"spec":{"replicas":1,"selector":{"matchLabels":{"app":"web"}},"template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"web","image":"nginx:stable"}]}}}}`)
	target := WorkloadRef{Namespace: "shop", Kind: WorkloadDeployment, Name: "web"}
	n := int32(1)
	now := time.Now()
	frame := WorkloadApply{Request: "11111111-2222-4333-8444-555555555555", Endpoint: "ep_1", IssuedAt: now, Deadline: now.Add(time.Minute), Target: target, Create: true, Spec: WorkloadConfiguration{Target: target, Replicas: &n, Strategy: "RollingUpdate", Containers: []WorkloadContainer{{Name: "web", Image: "nginx:stable"}}, RunManifest: raw}}
	if err := frame.Validate(now); err != nil {
		t.Fatal(err)
	}
	frame.Spec.Containers[0].Image = "malicious:latest"
	if frame.Validate(now) == nil {
		t.Fatal("summary and manifest image differ")
	}
	frame.Spec.Containers[0].Image = "nginx:stable"
	frame.Create = false
	frame.ResourceVersion = "1"
	frame.Spec.ResourceVersion = "1"
	if frame.Validate(now) == nil {
		t.Fatal("manifest allowed on edit")
	}
	_, err := RunDeployment(json.RawMessage(strings.Replace(string(raw), `"replicas":1`, `"replicas":1,"typo":true`, 1)), target)
	if err == nil {
		t.Fatal("unknown native fields dropped")
	}
}
