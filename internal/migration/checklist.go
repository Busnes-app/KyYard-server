package migration

import (
	"slices"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// ChecklistCodes are the operator's steps, in order; the web has a sentence for each. The
// checklist is display only: the migration's status moves by the operator's confirmations.
var ChecklistCodes = []string{"grant_namespace", "create_destination", "update_references", "copy_volume", "validate_destination", "switch_traffic", "confirm_cutover"}

// Step is one checklist step with its lines, the real names filled in: shell commands, or for
// update_references the pairs "<service> → <destination name>".
type Step struct {
	Code     string   `json:"code"`
	Commands []string `json:"commands,omitempty"`
}

// checklist lists the steps. update_references appears in an application of several services;
// copy_volume when a named volume a single service mounts exists on the source host. Every
// interpolated name is held to a DNS-label or volume-name grammar, so none needs quoting;
// $HELPER_IMAGE is the operator's digest-pinned image with sh and tar. The helper sleeps until the
// recipe deletes it, so a long copy is never cut off.
func checklist(in Input, users map[string]int, names map[string]string) []Step {
	ns := in.Destination.Namespace
	// No --overwrite: kubectl refuses to change an existing label, so a stricter one stays.
	out := []Step{{Code: "grant_namespace", Commands: []string{"kubectl label namespace " + ns + " pod-security.kubernetes.io/enforce=baseline"}}, {Code: "create_destination"}}
	if len(in.Spec.Services) > 1 {
		var pairs []string
		for _, s := range in.Spec.Services {
			pairs = append(pairs, s.Name+" → "+names[s.Name])
		}
		out = append(out, Step{Code: "update_references", Commands: pairs})
	}
	declared := make([]string, 0, len(in.Spec.Volumes))
	for _, v := range in.Spec.Volumes {
		declared = append(declared, v.Name)
	}
	claims := protocol.KubernetesNames(in.Destination.Project, declared)
	var copies []string
	for _, s := range in.Spec.Services {
		deploy, pod := names[s.Name], copyPod(names[s.Name])
		var recipe []string
		for _, v := range in.Spec.Volumes {
			host := store.VolumeHostName(in.Project, v)
			mounted := slices.ContainsFunc(s.Volumes, func(m store.ApplicationVolume) bool { return m.Kind == "named" && m.Source == v.Name })
			if !mounted || users[v.Name] != 1 || !slices.ContainsFunc(in.Volumes, func(pv protocol.Volume) bool { return pv.Name == host }) {
				continue
			}
			overrides := `{"spec":{"containers":[{"name":"` + pod + `","volumeMounts":[{"name":"to","mountPath":"/to"}]}],"volumes":[{"name":"to","persistentVolumeClaim":{"claimName":"` + claims[v.Name] + `"}}]}}`
			recipe = append(recipe,
				"kubectl -n "+ns+" run "+pod+` --image="$HELPER_IMAGE" --restart=Never --override-type=strategic --overrides='`+overrides+`' -- sleep infinity`,
				"kubectl -n "+ns+" wait --for=condition=Ready pod/"+pod+" --timeout=5m",
				// The destination's first start wrote into the claim; the copy must not mix datasets.
				"kubectl -n "+ns+" exec "+pod+` -- sh -c 'rm -rf /to/* /to/..?* /to/.[!.]*'`,
				"docker run --rm -v "+host+`:/from:ro "$HELPER_IMAGE" tar -C /from -cf - . | kubectl -n `+ns+" exec -i "+pod+" -- tar -C /to -xf -",
				"kubectl -n "+ns+" delete pod "+pod)
		}
		if len(recipe) > 0 {
			// Scaling returns at once; the pod must be gone, not terminating, before the copy.
			copies = append(copies, "kubectl -n "+ns+" scale deploy/"+deploy+" --replicas=0",
				"kubectl -n "+ns+" wait --for=delete pod -l "+podSelector(in.Destination.Project, s.Name)+" --timeout=5m")
			copies = append(copies, recipe...)
			copies = append(copies, "kubectl -n "+ns+" scale deploy/"+deploy+" --replicas=1")
		}
	}
	if len(copies) > 0 {
		out = append(out, Step{Code: "copy_volume", Commands: copies})
	}
	return append(out, Step{Code: "validate_destination"}, Step{Code: "switch_traffic"}, Step{Code: "confirm_cutover"})
}

// podSelector selects a service's pods by the labels render gives them: the project as
// app.kubernetes.io/instance and the service as kyyard.busnes.app/service.
func podSelector(project, service string) string {
	return "app.kubernetes.io/instance=" + project + ",kyyard.busnes.app/service=" + service
}

// copyPod names the helper pod after its Deployment; kubectl run names the container the same,
// and a container name is a DNS label of at most 63 characters.
func copyPod(deploy string) string {
	if len(deploy) > 58 {
		deploy = deploy[:58]
	}
	return deploy + "-copy"
}
