package migration

import (
	"slices"
	"strings"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// ChecklistCodes are the operator's steps, in order; the web has a sentence for each. The
// checklist is display only: the migration's status moves by the operator's confirmations.
var ChecklistCodes = []string{"grant_namespace", "create_destination", "copy_volume", "validate_destination", "switch_traffic", "confirm_cutover"}

// Step is one checklist step with its commands, the real names filled in.
type Step struct {
	Code     string   `json:"code"`
	Commands []string `json:"commands,omitempty"`
}

// checklist lists the steps; copy_volume appears when a named volume a single service mounts
// exists on the source host, with one copy recipe per volume into its destination claim.
func checklist(in Input, users map[string]int) []Step {
	ns := in.Destination.Namespace
	out := []Step{{Code: "grant_namespace", Commands: []string{"kubectl label namespace " + ns + " pod-security.kubernetes.io/enforce=baseline --overwrite"}}, {Code: "create_destination"}}
	services := make([]string, 0, len(in.Spec.Services))
	for _, s := range in.Spec.Services {
		services = append(services, s.Name)
	}
	names := protocol.KubernetesNames(in.Destination.Project, services)
	var copies []string
	for _, v := range in.Spec.Volumes {
		host := store.VolumeHostName(in.Project, v)
		if users[v.Name] != 1 || !slices.ContainsFunc(in.Volumes, func(pv protocol.Volume) bool { return pv.Name == host }) {
			continue
		}
		for _, s := range in.Spec.Services {
			for _, m := range s.Volumes {
				if m.Kind == "named" && m.Source == v.Name {
					copies = append(copies, "docker run --rm -v "+host+":/from:ro busybox tar -C /from -cf - . | kubectl -n "+ns+" exec -i deploy/"+names[s.Name]+" -- tar -C "+shellQuote(m.Target)+" -xf -")
				}
			}
		}
	}
	if len(copies) > 0 {
		out = append(out, Step{Code: "copy_volume", Commands: copies})
	}
	return append(out, Step{Code: "validate_destination"}, Step{Code: "switch_traffic"}, Step{Code: "confirm_cutover"})
}

// shellQuote makes s one POSIX shell word: a mount target is any display-safe absolute path.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
