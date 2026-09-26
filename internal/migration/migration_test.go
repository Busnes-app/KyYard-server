package migration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

var classes = []protocol.StorageClass{{Name: "standard", Default: true}, {Name: "fast"}}

func verified() protocol.ContainerInspection {
	return protocol.ContainerInspection{State: "running", RestartPolicy: "always", NetworkMode: "custom", NetworkCount: 1, Health: "none", Unsupported: []string{}, ConfigurationVerified: true}
}

// stateful is db mounting named volume data and web publishing 8080, both inspected clean.
func stateful(choices store.KubernetesExtension) Input {
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

var chosenData = store.KubernetesExtension{Volumes: map[string]store.KubernetesVolume{"data": {StorageClass: "fast", Size: "10Gi", AccessMode: protocol.AccessReadWriteOnce}}}

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
	golden(t, "named_volume", Analyze(stateful(store.KubernetesExtension{})))
	golden(t, "named_volume_chosen", Analyze(stateful(chosenData)))
	if !Analyze(stateful(chosenData)).Ready || Analyze(stateful(store.KubernetesExtension{})).Ready {
		t.Fatal("readiness does not follow the choice")
	}
	defaulted := store.KubernetesExtension{Volumes: map[string]store.KubernetesVolume{"data": {Size: "1Gi", AccessMode: protocol.AccessReadWriteOnce}}}
	if !Analyze(stateful(defaulted)).Ready {
		t.Fatal("the default class while one is reported")
	}
	gone := stateful(chosenData)
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

// Every code the analyzer can produce is in Codes, and every code in Codes is produced by some
// input, so the vocabulary the web translates is exactly the one reported.
func TestVocabularyIsExact(t *testing.T) {
	scheduling := verified()
	scheduling.Unsupported = []string{"pid_mode", "ulimits", "network", "read_only_rootfs", "dns"}
	extra := stateful(store.KubernetesExtension{})
	extra.Spec.Volumes = append(extra.Spec.Volumes, store.DeclaredVolume{Name: "ext", External: true})
	extra.Spec.Services[0].Volumes = []store.ApplicationVolume{{Kind: "named", Source: "ext", Target: "/ext"}}
	extra.Inspections["web"] = scheduling
	seen := map[string]bool{}
	for _, in := range []Input{stateful(store.KubernetesExtension{}), stateful(chosenData), blocked(), extra} {
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
}

// The checklist is fixed, and a mount target reaches the copy recipe as one shell word.
func TestChecklist(t *testing.T) {
	in := stateful(chosenData)
	in.Spec.Services[1].Volumes[0].Target = "/var/lib/it's data"
	steps := Analyze(in).Checklist
	var codes []string
	for _, s := range steps {
		codes = append(codes, s.Code)
	}
	if !slices.Equal(codes, ChecklistCodes) {
		t.Fatalf("steps %v", codes)
	}
	if want := `docker run --rm -v shop_data:/from:ro busybox tar -C /from -cf - . | kubectl -n shop exec -i deploy/shop-on-cluster-db -- tar -C '/var/lib/it'\''s data' -xf -`; steps[2].Commands[0] != want {
		t.Fatalf("copy %q", steps[2].Commands[0])
	}
	in.Volumes = nil
	if Analyze(in).Checklist[2].Code != "validate_destination" {
		t.Fatal("a copy step for a volume the host does not have")
	}
}

// The web's code tables are checked against this vocabulary through a generated fixture.
// KY_UPDATE_FIXTURES=1 rewrites it.
func TestWebVocabularyFixture(t *testing.T) {
	want, err := json.MarshalIndent(map[string][]string{"codes": Codes, "checklist": ChecklistCodes, "assumptions": AssumptionCodes}, "", "  ")
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
