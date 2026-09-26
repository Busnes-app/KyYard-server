package api

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/manifest"
)

// clusterReads names each resource the manifest's ClusterRole reads the way the enrollment
// disclosure and the threat model do. A resource added to the ClusterRole fails this test until
// both documents name it.
var clusterReads = map[string][2]string{ // resource: {disclosure, threat model}
	"namespaces": {"namespaces", "namespaces"}, "nodes": {"nodes", "nodes"}, "pods": {"pods", "pods"},
	"pods/log": {"pod logs", "pod logs"}, "events": {"events", "events"}, "services": {"services", "services"},
	"persistentvolumeclaims": {"persistent volume claims", "persistentvolumeclaims"},
	"deployments":            {"deployments", "deployments"}, "statefulsets": {"statefulsets", "statefulsets"}, "daemonsets": {"daemonsets", "daemonsets"},
	"storageclasses": {"storage classes", "storageclasses"},
}

// The enrollment disclosure and the threat model state the blast radius the manifest grants:
// every read of the ClusterRole, cluster-wide, and nothing it does not grant.
func TestClusterDisclosureMatchesTheManifestAndThreatModel(t *testing.T) {
	rendered, err := manifest.RenderRBAC("prod", []string{"shop"})
	if err != nil {
		t.Fatal(err)
	}
	var role string
	for _, doc := range strings.Split(rendered, "\n---\n") {
		if strings.Contains(doc, "\nkind: ClusterRole\n") {
			role = doc
		}
	}
	reads := regexp.MustCompile(`resources: \[([^\]]*)\]\n\s*verbs: \[get, list\]`).FindAllStringSubmatch(role, -1)
	if len(reads) == 0 {
		t.Fatal("no get/list rule in the ClusterRole")
	}
	raw, err := os.ReadFile("../../docs/threat-model.md")
	if err != nil {
		t.Fatal(err)
	}
	var row string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "| Compromised Kubernetes agent") {
			row = line
		}
	}
	first, _, _ := strings.Cut(clusterDisclosure, ". ")
	seen := 0
	for _, rule := range reads {
		for _, resource := range strings.Split(rule[1], ", ") {
			words, ok := clusterReads[resource]
			if !ok {
				t.Errorf("the ClusterRole reads %s, which the disclosure and the threat model do not name", resource)
				continue
			}
			seen++
			if !strings.Contains(first, words[0]) || !strings.Contains(row, words[1]) {
				t.Errorf("%s: disclosure %t, threat model %t", resource, strings.Contains(first, words[0]), strings.Contains(row, words[1]))
			}
		}
	}
	if seen != len(clusterReads) {
		t.Errorf("the ClusterRole reads %d resources, the wording names %d", seen, len(clusterReads))
	}
	for _, want := range []string{"in every namespace", "cannot read Secrets or ConfigMaps", "cluster-admin"} {
		if !strings.Contains(clusterDisclosure, want) {
			t.Errorf("the disclosure lost %q", want)
		}
	}
	for _, want := range []string{"no Secrets, no ConfigMaps, no `watch`, no wildcard", "readable cluster-wide by design", "can create any pod"} {
		if !strings.Contains(row, want) {
			t.Errorf("the threat model lost %q", want)
		}
	}
	if !strings.Contains(namespaceDisclosure, "run any pod in those namespaces") {
		t.Error("the namespace disclosure lost its blast radius")
	}
}
