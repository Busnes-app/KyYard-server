package api

import (
	"os"
	"regexp"
	"slices"
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

// namespaceGrant is what the per-namespace kyyard-agent-deploy Role grants one resource, and how
// the namespace disclosure must phrase it. write is true for a resource the disclosure covers
// through "create, update and delete <phrase>"; false for a create-only resource the disclosure
// must instead say it may create but never update or delete.
type namespaceGrant struct {
	phrase string
	verbs  []string
	write  bool
}

// namespaceGrants names each resource the kyyard-agent-deploy Role grants. A resource added, or
// a verb list changed, on that Role fails this test until namespaceDisclosure is updated to match.
var namespaceGrants = map[string]namespaceGrant{
	"deployments":            {"Deployments", []string{"get", "list", "create", "update", "patch", "delete"}, true},
	"services":               {"Services", []string{"get", "list", "create", "update", "patch", "delete"}, true},
	"configmaps":             {"ConfigMaps", []string{"get", "list", "create", "update", "patch", "delete"}, true},
	"secrets":                {"Secrets", []string{"get", "create", "update", "patch", "delete"}, true},
	"persistentvolumeclaims": {"PersistentVolumeClaims", []string{"get", "list", "create"}, false},
}

var ruleRE = regexp.MustCompile(`resources: \[([^\]]*)\]\n\s*verbs: \[([^\]]*)\]`)

// The enrollment disclosure and the threat model state the blast radius the manifest grants:
// every read of the ClusterRole, cluster-wide, every grant of the per-namespace Role, and
// nothing either does not grant.
func TestClusterDisclosureMatchesTheManifestAndThreatModel(t *testing.T) {
	rendered, err := manifest.RenderRBAC("prod", []string{"shop"})
	if err != nil {
		t.Fatal(err)
	}
	var clusterRole, namespaceRole string
	for _, doc := range strings.Split(rendered, "\n---\n") {
		switch {
		case strings.Contains(doc, "\nkind: ClusterRole\n"):
			clusterRole = doc
		case strings.Contains(doc, "\nkind: Role\n") && strings.Contains(doc, "name: kyyard-agent-deploy\n"):
			namespaceRole = doc
		}
	}
	if namespaceRole == "" {
		t.Fatal("no kyyard-agent-deploy Role rendered")
	}
	clusterRules := ruleRE.FindAllStringSubmatch(clusterRole, -1)
	if len(clusterRules) == 0 {
		t.Fatal("no rule in the ClusterRole")
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
	for _, rule := range clusterRules {
		verbs := rule[2]
		for _, resource := range strings.Split(rule[1], ", ") {
			if resource == "selfsubjectaccessreviews" {
				continue
			}
			words, ok := clusterReads[resource]
			if !ok {
				t.Errorf("the ClusterRole reads %s, which the disclosure and the threat model do not name", resource)
				continue
			}
			seen++
			if verbs != "get, list" {
				t.Errorf("%s: ClusterRole grants verbs [%s], the table only covers get, list", resource, verbs)
			}
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

	nsRules := ruleRE.FindAllStringSubmatch(namespaceRole, -1)
	if len(nsRules) == 0 {
		t.Fatal("no rule in the kyyard-agent-deploy Role")
	}
	seenNS := 0
	for _, rule := range nsRules {
		verbs := strings.Split(rule[2], ", ")
		for _, resource := range strings.Split(rule[1], ", ") {
			want, ok := namespaceGrants[resource]
			if !ok {
				t.Errorf("the namespace Role grants %s, which the disclosure does not name", resource)
				continue
			}
			seenNS++
			if !slices.Equal(verbs, want.verbs) {
				t.Errorf("%s: Role grants verbs %v, the table expects %v", resource, verbs, want.verbs)
			}
			if want.write {
				if !strings.Contains(namespaceDisclosure, "create, update and delete") || !strings.Contains(namespaceDisclosure, want.phrase) {
					t.Errorf("%s: namespace disclosure missing the create/update/delete grant", resource)
				}
			} else {
				if !strings.Contains(namespaceDisclosure, "create "+want.phrase) || !strings.Contains(namespaceDisclosure, "never update or delete") {
					t.Errorf("%s: namespace disclosure missing the create-only grant", resource)
				}
			}
		}
	}
	if seenNS != len(namespaceGrants) {
		t.Errorf("the namespace Role grants %d resources, the wording names %d", seenNS, len(namespaceGrants))
	}
}
