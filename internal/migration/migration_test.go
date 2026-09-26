package migration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes/render"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

var classes = []protocol.StorageClass{{Name: "standard", Default: true}, {Name: "fast"}}

func verified() protocol.ContainerInspection {
	return protocol.ContainerInspection{State: "running", RestartPolicy: "always", NetworkMode: "custom", NetworkCount: 1, Health: "none", Unsupported: []string{}, ConfigurationVerified: true}
}

// stateful is db mounting named volume data and web publishing 8080, both inspected clean.
func stateful(choices store.MigrationChoices) Input {
	return Input{
		Spec: store.ApplicationSpec{Kind: "compose.v1", Volumes: []store.DeclaredVolume{{Name: "data"}}, Services: []store.ApplicationService{
			{Name: "web", Image: "ghcr.io/org/web:1", Restart: "always", Ports: []store.ApplicationPort{{Target: 80, Published: 8080, Protocol: "tcp"}}},
			{Name: "db", Image: "ghcr.io/org/db:1", Restart: "unless-stopped", Volumes: []store.ApplicationVolume{{Kind: "named", Source: "data", Target: "/var/lib/db"}}},
		}},
		Project:     "shop",
		Containers:  map[string]protocol.Container{"web": {Name: "shop-web", Networks: []string{"shop_default"}}, "db": {Name: "shop-db", Networks: []string{"shop_default"}}},
		Inspections: map[string]protocol.ContainerInspection{"web": verified(), "db": verified()},
		Volumes:     []protocol.Volume{{Name: "shop_data"}},
		Destination: Destination{Namespace: "shop", Project: "shop-on-cluster", StorageClasses: classes},
		Choices:     choices,
	}
}

// blocked has a bind, a volume two services share, a host-address port, an inspection reporting
// privileged and resource limits, a service never inspected, a healthcheck and a restart policy
// a Deployment cannot express.
func blocked() Input {
	privileged := verified()
	privileged.Unsupported, privileged.ConfigurationVerified, privileged.Health = []string{"privileged", "resource_limits"}, false, "healthy"
	return Input{
		Spec: store.ApplicationSpec{Kind: "compose.v1", Volumes: []store.DeclaredVolume{{Name: "cache"}}, Services: []store.ApplicationService{
			{Name: "api", Image: "ghcr.io/org/api:1", Restart: "on-failure", Ports: []store.ApplicationPort{{Target: 80, Published: 8080, HostIP: "127.0.0.1", Protocol: "tcp"}}, Volumes: []store.ApplicationVolume{{Kind: "bind", Source: "/srv/api", Target: "/etc/api", ReadOnly: true}, {Kind: "named", Source: "cache", Target: "/cache"}}},
			{Name: "worker", Image: "ghcr.io/org/worker:1", Volumes: []store.ApplicationVolume{{Kind: "named", Source: "cache", Target: "/cache"}}},
		}},
		Project:     "shop",
		Containers:  map[string]protocol.Container{"api": {Networks: []string{"shop_default", "proxy"}}, "worker": {Networks: []string{"host"}}},
		Inspections: map[string]protocol.ContainerInspection{"api": privileged},
		Volumes:     []protocol.Volume{{Name: "shop_cache"}},
		Destination: Destination{Namespace: "shop", Project: "shop-on-cluster", StorageClasses: classes},
	}
}

var chosenData = store.MigrationChoices{Volumes: map[string]store.KubernetesVolume{"data": {StorageClass: "fast", Size: "10Gi", AccessMode: protocol.AccessReadWriteOnce}}}

// acknowledgedAll answers every finding the two-service fixtures ask to acknowledge.
var (
	acknowledgedAll = []string{"network_references", "port_unpublished"}
	ackedData       = store.MigrationChoices{Volumes: chosenData.Volumes, Acknowledged: acknowledgedAll}
)

// golden compares got with testdata/name.json; KY_UPDATE_FIXTURES=1 rewrites it.
func golden(t *testing.T, name string, got Report) {
	t.Helper()
	raw, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	path := filepath.Join("testdata", name+".json")
	if os.Getenv("KY_UPDATE_FIXTURES") == "1" {
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, want) {
		t.Fatalf("%s differs (%v):\n%s", path, err, raw)
	}
}

// A named volume asks for a choice until one with a reported class is made; then the report is
// ready and the checklist copies the volume into the destination's Deployment.
func TestAnalyzeNamedVolume(t *testing.T) {
	golden(t, "named_volume", Analyze(stateful(store.MigrationChoices{})))
	if !Analyze(stateful(ackedData)).Ready || Analyze(stateful(store.MigrationChoices{Acknowledged: acknowledgedAll})).Ready {
		t.Fatal("readiness does not follow the choice")
	}
	defaulted := store.MigrationChoices{Volumes: map[string]store.KubernetesVolume{"data": {Size: "1Gi", AccessMode: protocol.AccessReadWriteOnce}}, Acknowledged: acknowledgedAll}
	if !Analyze(stateful(defaulted)).Ready {
		t.Fatal("the default class while one is reported")
	}
	gone := stateful(ackedData)
	gone.Destination.StorageClasses = []protocol.StorageClass{{Name: "standard"}}
	if r := Analyze(gone); r.Ready || r.Services[0].Findings[0] != (Finding{AxisStorage, ChoiceRequired, "volume_named", "data"}) {
		t.Fatalf("a choice whose class the cluster no longer reports: %+v", r.Services[0])
	}
}

// Binds, shared volumes, host addresses, host networking, privileged and scheduling flags block;
// dropped limits and healthchecks ask for a choice; a service never inspected is never read as
// supported on the axes an inspection decides.
func TestAnalyzeBlocked(t *testing.T) {
	r := Analyze(blocked())
	golden(t, "blocked", r)
	if r.Ready || r.Services[0].Class != Blocked || r.Services[1].Class != Blocked {
		t.Fatalf("report %+v", r)
	}
}

// In an application of several services each one is addressed by its destination name and one
// publishing no port is unreachable: both ask for the operator's acknowledgement, the checklist
// lists every rename, and a single service needs neither.
func TestAnalyzeServiceReferences(t *testing.T) {
	r := Analyze(stateful(chosenData))
	golden(t, "named_volume_chosen", r)
	db := r.Services[0].Findings
	if r.Ready || !slices.Contains(db, Finding{AxisNetworking, ChoiceRequired, "network_references", "shop-on-cluster-db"}) || !slices.Contains(db, Finding{AxisPorts, ChoiceRequired, "port_unpublished", ""}) {
		t.Fatalf("db %+v", db)
	}
	if refs := r.Checklist[slices.IndexFunc(r.Checklist, func(s Step) bool { return s.Code == "update_references" })]; !slices.Equal(refs.Commands, []string{"web → shop-on-cluster-web", "db → shop-on-cluster-db"}) {
		t.Fatalf("references %+v", refs)
	}
	acked := chosenData
	acked.Acknowledged = []string{"network_references"}
	if Analyze(stateful(acked)).Ready {
		t.Fatal("ready with an unreachable service unacknowledged")
	}
	acked.Acknowledged = []string{"network_references", "port_unpublished"}
	a := Analyze(stateful(acked))
	golden(t, "named_volume_acknowledged", a)
	if !a.Ready || !slices.Contains(a.Services[0].Findings, Finding{AxisNetworking, Supported, "network_references", "shop-on-cluster-db"}) {
		t.Fatalf("acknowledged %+v", a)
	}
	single := stateful(chosenData)
	single.Spec.Services = single.Spec.Services[1:]
	s := Analyze(single)
	if !s.Ready || !slices.Contains(s.Services[0].Findings, Finding{AxisNetworking, Supported, "networking_supported", ""}) || !slices.Contains(s.Services[0].Findings, Finding{AxisPorts, Supported, "port_unpublished", ""}) || slices.ContainsFunc(s.Checklist, func(x Step) bool { return x.Code == "update_references" }) {
		t.Fatalf("single service %+v", s)
	}
}

// A healthcheck, resource limits and a read-only root the destination drops are choices until
// the operator acknowledges each drop; the acknowledged finding is supported and says so.
func TestAnalyzeAcknowledgedDrops(t *testing.T) {
	in := stateful(ackedData)
	dropping := verified()
	dropping.Health, dropping.Unsupported, dropping.ConfigurationVerified = "healthy", []string{"resource_limits", "read_only_rootfs"}, false
	in.Inspections["db"] = dropping
	for _, acknowledged := range [][]string{acknowledgedAll, append(slices.Clone(acknowledgedAll), "healthcheck_dropped"), append(slices.Clone(acknowledgedAll), "healthcheck_dropped", "resource_limits_dropped")} {
		in.Choices.Acknowledged = acknowledged
		if Analyze(in).Ready {
			t.Fatalf("ready with %v acknowledged", acknowledged)
		}
	}
	in.Choices.Acknowledged = append(slices.Clone(acknowledgedAll), "healthcheck_dropped", "resource_limits_dropped", "read_only_rootfs")
	r := Analyze(in)
	db := r.Services[0].Findings
	for _, want := range []Finding{{AxisProbes, Supported, "healthcheck_dropped", "acknowledged"}, {AxisResources, Supported, "resource_limits_dropped", "resource_limits"}, {AxisFlags, Supported, "read_only_rootfs", "acknowledged"}} {
		if !slices.Contains(db, want) {
			t.Errorf("no %+v in %+v", want, db)
		}
	}
	if !r.Ready {
		t.Fatalf("every drop acknowledged: %+v", r)
	}
}

// An agent without container.inspect.health answers no health: the probes axis is unknown, never
// read as support. An image_config override still says the healthcheck is dropped.
func TestAnalyzeUnknownHealth(t *testing.T) {
	in := stateful(chosenData)
	unknown := verified()
	unknown.Health = ""
	in.Inspections["db"] = unknown
	if f := Analyze(in).Services[0].Findings; !slices.Contains(f, Finding{AxisProbes, ChoiceRequired, "inspection_unavailable", "agent"}) || slices.ContainsFunc(f, func(x Finding) bool { return x.Code == "probes_supported" }) {
		t.Fatalf("empty health: %+v", f)
	}
	unknown.Unsupported = []string{"image_config"}
	in.Inspections["db"] = unknown
	if f := Analyze(in).Services[0].Findings; !slices.Contains(f, Finding{AxisProbes, ChoiceRequired, "healthcheck_dropped", ""}) {
		t.Fatalf("empty health with an override: %+v", f)
	}
}

// Every code the analyzer can produce is in Codes, and every code in Codes is produced by some
// input, so the vocabulary the web translates is exactly the one reported.
func TestVocabularyIsExact(t *testing.T) {
	scheduling := verified()
	scheduling.Unsupported = []string{"pid_mode", "ulimits", "network", "read_only_rootfs", "dns"}
	extra := stateful(store.MigrationChoices{})
	extra.Spec.Volumes = append(extra.Spec.Volumes, store.DeclaredVolume{Name: "ext", External: true})
	extra.Spec.Services[0].Volumes = []store.ApplicationVolume{{Kind: "named", Source: "ext", Target: "/ext"}}
	extra.Inspections["web"] = scheduling
	single := stateful(chosenData)
	single.Spec.Services = single.Spec.Services[1:]
	seen := map[string]bool{}
	for _, in := range []Input{stateful(store.MigrationChoices{}), stateful(chosenData), blocked(), extra, single} {
		for _, s := range Analyze(in).Services {
			for _, f := range s.Findings {
				if !slices.Contains(Codes, f.Code) {
					t.Errorf("%s: %s is not in Codes", s.Name, f.Code)
				}
				seen[f.Code] = true
			}
		}
	}
	for _, c := range Codes {
		if !seen[c] {
			t.Errorf("%s is never produced", c)
		}
	}
	for _, c := range store.MigrationAcknowledgeable {
		if !slices.Contains(Codes, c) {
			t.Errorf("the store acknowledges %s, which is not in Codes", c)
		}
	}
}

// The checklist is fixed. The copy recipe stops the destination, fills the claim through a
// helper pod that mounts it, removes the helper and starts the destination again; it never
// writes under a running application and needs no tar in the application's image.
func TestChecklist(t *testing.T) {
	in := stateful(chosenData)
	steps := Analyze(in).Checklist
	var codes []string
	for _, s := range steps {
		codes = append(codes, s.Code)
	}
	if !slices.Equal(codes, ChecklistCodes) {
		t.Fatalf("steps %v", codes)
	}
	copyStep := steps[slices.Index(codes, "copy_volume")]
	overrides := `{"spec":{"containers":[{"name":"shop-on-cluster-db-copy","volumeMounts":[{"name":"to","mountPath":"/to"}]}],"volumes":[{"name":"to","persistentVolumeClaim":{"claimName":"shop-on-cluster-data"}}]}}`
	want := []string{
		"kubectl -n shop scale deploy/shop-on-cluster-db --replicas=0",
		"kubectl -n shop wait --for=delete pod -l " + render.LabelInstanceName + "=shop-on-cluster," + render.LabelService + "=db --timeout=5m",
		`kubectl -n shop run shop-on-cluster-db-copy --image="$HELPER_IMAGE" --restart=Never --override-type=strategic --overrides='` + overrides + `' -- sleep infinity`,
		"kubectl -n shop wait --for=condition=Ready pod/shop-on-cluster-db-copy --timeout=5m",
		`kubectl -n shop exec shop-on-cluster-db-copy -- sh -c 'rm -rf /to/* /to/..?* /to/.[!.]*'`,
		`docker run --rm -v shop_data:/from:ro "$HELPER_IMAGE" tar -C /from -cf - . | kubectl -n shop exec -i shop-on-cluster-db-copy -- tar -C /to -xf -`,
		"kubectl -n shop delete pod shop-on-cluster-db-copy",
		"kubectl -n shop scale deploy/shop-on-cluster-db --replicas=1",
	}
	if !slices.Equal(copyStep.Commands, want) {
		t.Fatalf("copy recipe:\n%s", strings.Join(copyStep.Commands, "\n"))
	}
	if !json.Valid([]byte(overrides)) || slices.ContainsFunc(copyStep.Commands, func(c string) bool { return strings.Contains(c, "exec -i deploy/") }) {
		t.Fatal("the recipe writes under the running application")
	}
	in.Volumes = nil
	if slices.ContainsFunc(Analyze(in).Checklist, func(s Step) bool { return s.Code == "copy_volume" }) {
		t.Fatal("a copy step for a volume the host does not have")
	}
}

// The web's code tables are checked against this vocabulary through a generated fixture.
// KY_UPDATE_FIXTURES=1 rewrites it.
func TestWebVocabularyFixture(t *testing.T) {
	want, err := json.MarshalIndent(map[string][]string{"codes": Codes, "checklist": ChecklistCodes, "assumptions": AssumptionCodes, "acknowledgeable": store.MigrationAcknowledgeable}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	path := filepath.Join("..", "..", "web", "src", "migration-codes.json")
	if os.Getenv("KY_UPDATE_FIXTURES") == "1" {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s is stale (%v): run KY_UPDATE_FIXTURES=1 go test ./internal/migration -run TestWebVocabularyFixture", path, err)
	}
}
