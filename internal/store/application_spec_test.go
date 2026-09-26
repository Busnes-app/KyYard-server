package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/store"
)

func withVolumes() store.ApplicationSpec {
	spec := desired("postgres:17")
	spec.Volumes = []store.DeclaredVolume{{Name: "db"}, {Name: "shared", External: true}}
	spec.Services[0].Volumes = []store.ApplicationVolume{
		{Kind: "named", Source: "db", Target: "/var/lib/postgresql/data"},
		{Kind: "named", Source: "shared", Target: "/shared", ReadOnly: true},
		{Kind: "bind", Source: "/srv/cfg", Target: "/etc/app", ReadOnly: true},
	}
	return spec
}

func TestApplicationSpecVolumeBounds(t *testing.T) {
	if err := store.ValidateApplicationSpec(withVolumes()); err != nil {
		t.Fatalf("valid volumes refused: %v", err)
	}
	cases := map[string]func(*store.ApplicationSpec){
		"undeclared":       func(s *store.ApplicationSpec) { s.Services[0].Volumes[0].Source = "cache" },
		"relative bind":    func(s *store.ApplicationSpec) { s.Services[0].Volumes[2].Source = "data" },
		"dot bind":         func(s *store.ApplicationSpec) { s.Services[0].Volumes[2].Source = "./data" },
		"unclean bind":     func(s *store.ApplicationSpec) { s.Services[0].Volumes[2].Source = "/data/../etc" },
		"slash bind":       func(s *store.ApplicationSpec) { s.Services[0].Volumes[2].Source = "/srv/cfg/" },
		"control bind":     func(s *store.ApplicationSpec) { s.Services[0].Volumes[2].Source = "/srv/\ncfg" },
		"relative target":  func(s *store.ApplicationSpec) { s.Services[0].Volumes[0].Target = "var/lib" },
		"unclean target":   func(s *store.ApplicationSpec) { s.Services[0].Volumes[0].Target = "/var/../lib" },
		"space target":     func(s *store.ApplicationSpec) { s.Services[0].Volumes[0].Target = "/x " },
		"256-byte bind":    func(s *store.ApplicationSpec) { s.Services[0].Volumes[2].Source = "/" + strings.Repeat("a", 255) },
		"root target":      func(s *store.ApplicationSpec) { s.Services[0].Volumes[0].Target = "/" },
		"duplicate target": func(s *store.ApplicationSpec) { s.Services[0].Volumes[1].Target = "/var/lib/postgresql/data" },
		"tmpfs":            func(s *store.ApplicationSpec) { s.Services[0].Volumes[0].Kind = "tmpfs" },
		"empty kind":       func(s *store.ApplicationSpec) { s.Services[0].Volumes[0].Kind = "" },
		"bad name":         func(s *store.ApplicationSpec) { s.Volumes[0].Name = "-db"; s.Services[0].Volumes[0].Source = "-db" },
		"long name": func(s *store.ApplicationSpec) {
			s.Volumes[0].Name = strings.Repeat("d", 65)
			s.Services[0].Volumes[0].Source = s.Volumes[0].Name
		},
		"duplicate declared": func(s *store.ApplicationSpec) { s.Volumes = append(s.Volumes, store.DeclaredVolume{Name: "db"}) },
		"33 on a service": func(s *store.ApplicationSpec) {
			s.Services[0].Volumes = nil
			for i := 0; i < 33; i++ {
				s.Services[0].Volumes = append(s.Services[0].Volumes, store.ApplicationVolume{Kind: "named", Source: "db", Target: fmt.Sprint("/v", i)})
			}
		},
		"65 declared": func(s *store.ApplicationSpec) {
			for i := 0; i < 63; i++ {
				s.Volumes = append(s.Volumes, store.DeclaredVolume{Name: fmt.Sprint("v", i)})
			}
		},
	}
	for name, mutate := range cases {
		spec := withVolumes()
		mutate(&spec)
		if err := store.ValidateApplicationSpec(spec); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	at := withVolumes()
	at.Services[0].Volumes = nil
	for i := 0; i < 32; i++ {
		at.Services[0].Volumes = append(at.Services[0].Volumes, store.ApplicationVolume{Kind: "named", Source: "db", Target: fmt.Sprint("/v", i)})
	}
	for i := 0; i < 62; i++ {
		at.Volumes = append(at.Volumes, store.DeclaredVolume{Name: fmt.Sprint("v", i)})
	}
	at.Services[0].Volumes[0] = store.ApplicationVolume{Kind: "bind", Source: "/" + strings.Repeat("a", 254), Target: "/v0"}
	if err := store.ValidateApplicationSpec(at); err != nil {
		t.Fatalf("32 mounts and 64 declared refused: %v", err)
	}
}

// A revision saved before the two-character minimum existed may still hold a one-character
// volume name; ValidVolumeName (and so preflight, mapping and comparison on the stored spec)
// must keep validating it. Only a newly declared name is held to the stricter grammar.
func TestApplicationSpecOneCharacterVolumeNameStillValidates(t *testing.T) {
	spec := withVolumes()
	spec.Volumes[0].Name = "a"
	spec.Services[0].Volumes[0].Source = "a"
	if err := store.ValidateApplicationSpec(spec); err != nil {
		t.Fatalf("a stored one-character volume name refused: %v", err)
	}
	if store.ValidDeclaredVolumeName("a") {
		t.Fatal("a one-character name accepted for a new declaration")
	}
}

func TestVolumeHostName(t *testing.T) {
	if got := store.VolumeHostName("shop", store.DeclaredVolume{Name: "db"}); got != "shop_db" {
		t.Fatal(got)
	}
	if got := store.VolumeHostName("shop", store.DeclaredVolume{Name: "db", External: true}); got != "db" {
		t.Fatal(got)
	}
}

// A spec without volumes must encode exactly as before, so stored revision digests stay valid.
func TestApplicationSpecVolumesRoundTrip(t *testing.T) {
	raw, _ := json.Marshal(desired("nginx:1"))
	if strings.Contains(string(raw), "volumes") {
		t.Fatalf("volume-free spec encoding changed: %s", raw)
	}
	ctx := context.Background()
	st, a := setupTenantAccess(t)
	a.EnvironmentID = "env-a"
	ts := st.Tenancy()
	app, err := ts.CreateApplication(ctx, a, "db", withVolumes())
	mustTenant(t, err)
	rev, err := ts.ReadApplicationRevision(ctx, a, app.ID, 1)
	mustTenant(t, err)
	got, _ := json.Marshal(rev.Spec)
	want, _ := json.Marshal(withVolumes())
	if string(got) != string(want) {
		t.Fatalf("volumes lost: %s", got)
	}
}

// A kubernetes extension names declared volumes only, each with a valid class, size and access
// mode; an empty one is refused, and the digest covers it.
func TestApplicationSpecKubernetesExtension(t *testing.T) {
	good := func() store.ApplicationSpec {
		spec := withVolumes()
		spec.Kubernetes = &store.KubernetesExtension{Volumes: map[string]store.KubernetesVolume{
			"db":     {StorageClass: "fast.ssd", Size: "20Gi", AccessMode: "ReadWriteOnce"},
			"shared": {Size: "1Ti", AccessMode: "ReadWriteOnce"},
		}}
		return spec
	}
	if err := store.ValidateApplicationSpec(good()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*store.ApplicationSpec){
		"empty":      func(s *store.ApplicationSpec) { s.Kubernetes.Volumes = nil },
		"undeclared": func(s *store.ApplicationSpec) { s.Kubernetes.Volumes["cache"] = s.Kubernetes.Volumes["db"] },
		"bad size": func(s *store.ApplicationSpec) {
			s.Kubernetes.Volumes["db"] = store.KubernetesVolume{StorageClass: "fast", Size: "20GB", AccessMode: "ReadWriteOnce"}
		},
		"too large": func(s *store.ApplicationSpec) {
			s.Kubernetes.Volumes["db"] = store.KubernetesVolume{Size: "17Ti", AccessMode: "ReadWriteOnce"}
		},
		"bad class": func(s *store.ApplicationSpec) {
			s.Kubernetes.Volumes["db"] = store.KubernetesVolume{StorageClass: "Fast", Size: "1Gi", AccessMode: "ReadWriteOnce"}
		},
		"read write many": func(s *store.ApplicationSpec) {
			s.Kubernetes.Volumes["db"] = store.KubernetesVolume{Size: "1Gi", AccessMode: "ReadWriteMany"}
		},
		"17 volumes": func(s *store.ApplicationSpec) {
			for i := range 15 {
				name := fmt.Sprint("v", i)
				s.Volumes = append(s.Volumes, store.DeclaredVolume{Name: name})
				s.Kubernetes.Volumes[name] = store.KubernetesVolume{Size: "1Gi", AccessMode: "ReadWriteOnce"}
			}
		},
	} {
		spec := good()
		mutate(&spec)
		if err := store.ValidateApplicationSpec(spec); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	plain, extended := withVolumes(), good()
	a, _ := json.Marshal(plain)
	b, _ := json.Marshal(extended)
	if strings.Contains(string(a), "kubernetes") || !strings.Contains(string(b), `"kubernetes":{"volumes":{"db":{"storage_class":"fast.ssd","size":"20Gi","access_mode":"ReadWriteOnce"}`) {
		t.Fatalf("encoded %s / %s", a, b)
	}
}
