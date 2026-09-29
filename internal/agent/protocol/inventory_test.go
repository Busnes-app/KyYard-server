package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// The worst legal snapshot must fit the shared byte limit after shrinking, and text is safe.
func TestShrinkFitsTheSharedLimitAndClampConforms(t *testing.T) {
	s := &Snapshot{}
	for i := 0; i < MaxContainers; i++ {
		labels := map[string]string{}
		for j := 0; j < MaxLabels; j++ {
			labels[strings.Repeat("k", MaxLabelBytes-3)+string(rune('a'+j%26))+string(rune('a'+(j/26)%26))+"x"] = strings.Repeat("v", MaxLabelBytes)
		}
		s.Containers = append(s.Containers, Container{ID: strings.Repeat("c", 64), Name: strings.Repeat("n", MaxNameBytes), Image: strings.Repeat("i", MaxImageRefBytes), Labels: labels})
	}
	Clamp(s)
	raw := Shrink(s)
	if len(raw) > MaxSnapshotBytes {
		t.Fatalf("shrunk snapshot is %d bytes, over %d", len(raw), MaxSnapshotBytes)
	}
	if len(s.Truncated) == 0 {
		t.Fatal("shrinking named nothing")
	}
	var back Snapshot
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	// Undeclared fields, unsafe text and oversized maps do not survive Clamp.
	dirty := &Snapshot{Containers: make([]Container, MaxContainers+5)}
	big := map[string]string{}
	for j := 0; j < MaxLabels*2; j++ {
		big[strings.Repeat("k", j+1)] = "v"
	}
	dirty.Containers[0] = Container{Name: "web‮evil\nline", Labels: big, Image: "img "}
	Clamp(dirty)
	if len(dirty.Containers) != MaxContainers || len(dirty.Containers[0].Labels) != MaxLabels || strings.ContainsAny(dirty.Containers[0].Name, "‮\n") || dirty.Containers[0].Name != "webevilline" || dirty.Containers[0].Image != "img" {
		t.Fatalf("clamp: %d containers, %d labels, name %q image %q", len(dirty.Containers), len(dirty.Containers[0].Labels), dirty.Containers[0].Name, dirty.Containers[0].Image)
	}
	if got := CleanText("héllo wörld", 7); got != "héllo " || !json.Valid([]byte(`"`+got+`"`)) {
		t.Fatalf("rune-safe cut: %q", got)
	}
}

// Mounts decode at most MaxMounts per container; an absent list stays nil (not reported),
// an empty one stays empty (reported, none).
func TestContainerMountsDecodeBounded(t *testing.T) {
	var many []string
	for i := 0; i < MaxMounts+8; i++ {
		many = append(many, `{"kind":"volume","source":"v","target":"/m`+strings.Repeat("x", i)+`"}`)
	}
	raw := []byte(`{"generation":1,"containers":[{"id":"a","mounts":[` + strings.Join(many, ",") + `]},{"id":"b"},{"id":"c","mounts":[]},{"id":"d","mounts":null}]}`)
	var s Snapshot
	if err := UnmarshalSnapshotBounded(raw, &s); err != nil {
		t.Fatal(err)
	}
	a, b, c, d := s.Containers[0], s.Containers[1], s.Containers[2], s.Containers[3]
	if len(a.Mounts) != MaxMounts || !a.MountsTruncated || a.Mounts[0].Kind != MountVolume || a.Mounts[0].Target != "/m" {
		t.Fatalf("capped: %d mounts, truncated %v, first %+v", len(a.Mounts), a.MountsTruncated, a.Mounts[0])
	}
	if b.Mounts != nil || b.MountsTruncated || c.Mounts == nil || len(c.Mounts) != 0 || c.MountsTruncated || d.Mounts != nil {
		t.Fatalf("absent %v, empty %v, null %v", b.Mounts, c.Mounts, d.Mounts)
	}
	// The same bound applies to a plain decode, which is how a stored snapshot is read back.
	var plain Container
	if err := json.Unmarshal([]byte(`{"id":"a","mounts":[`+strings.Join(many, ",")+`]}`), &plain); err != nil || len(plain.Mounts) != MaxMounts || !plain.MountsTruncated {
		t.Fatalf("plain decode: %v %d", err, len(plain.Mounts))
	}
	back, _ := json.Marshal(c)
	if !strings.Contains(string(back), `"mounts":[]`) {
		t.Fatalf("empty mounts must survive encoding: %s", back)
	}
}

func TestClampMounts(t *testing.T) {
	s := &Snapshot{Containers: []Container{{Mounts: make([]Mount, MaxMounts+1)}, {}}}
	s.Containers[0].Mounts[0] = Mount{Kind: "tmpfs", Source: "/srv\nx", Target: "/‮t"}
	s.Containers[0].Mounts[1] = Mount{Kind: MountBind, Source: "/srv", Target: "/t"}
	Clamp(s)
	c := s.Containers[0]
	if len(c.Mounts) != MaxMounts || !c.MountsTruncated || c.Mounts[0].Kind != MountOther || c.Mounts[0].Source != "/srvx" || c.Mounts[0].Target != "/t" || c.Mounts[1].Kind != MountBind {
		t.Fatalf("clamp: %d %v %+v", len(c.Mounts), c.MountsTruncated, c.Mounts[:2])
	}
	if s.Containers[1].Mounts != nil {
		t.Fatal("clamp invented a mount list for a container that reported none")
	}
	// A path cut at the text bound no longer names the mount: the list is marked incomplete.
	long := &Snapshot{Containers: []Container{{Mounts: []Mount{{Kind: MountBind, Source: "/" + strings.Repeat("s", MaxImageRefBytes), Target: "/t"}}}, {Mounts: []Mount{{Kind: MountBind, Source: "/srv", Target: "/t"}}}}}
	Clamp(long)
	if !long.Containers[0].MountsTruncated || long.Containers[1].MountsTruncated {
		t.Fatalf("cut path: %v, intact path: %v", long.Containers[0].MountsTruncated, long.Containers[1].MountsTruncated)
	}
}

// Mounts go before containers do: a host full of mounts keeps every container, each marked.
func TestShrinkDropsMountsBeforeContainers(t *testing.T) {
	s := &Snapshot{}
	for i := 0; i < MaxContainers; i++ {
		c := Container{ID: strings.Repeat("c", 64)}
		for j := 0; j < MaxMounts; j++ {
			c.Mounts = append(c.Mounts, Mount{Kind: MountBind, Source: strings.Repeat("s", MaxImageRefBytes), Target: strings.Repeat("t", MaxImageRefBytes)})
		}
		s.Containers = append(s.Containers, c)
	}
	Clamp(s)
	if raw := Shrink(s); len(raw) > MaxSnapshotBytes || len(s.Containers) != MaxContainers || len(s.Containers[0].Mounts) != 0 || !s.Containers[0].MountsTruncated {
		t.Fatalf("%d bytes, %d containers, %d mounts", len(raw), len(s.Containers), len(s.Containers[0].Mounts))
	}
}

func TestContainerNetworksDecodeBothShapes(t *testing.T) {
	old := []byte(`{"containers":[{"id":"c1","name":"web","networks":["shop_default","bridge"]}]}`)
	var s Snapshot
	if err := UnmarshalSnapshotBounded(old, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Containers[0].Networks) != 2 || s.Containers[0].Networks[0] != (NetworkAttachment{Name: "shop_default"}) {
		t.Fatalf("string networks: %+v", s.Containers[0].Networks)
	}
	if !s.Containers[0].StartedAt.IsZero() || s.Containers[0].Health != "" {
		t.Fatalf("old agent must report no start or health: %+v", s.Containers[0])
	}
	current := []byte(`{"containers":[{"id":"c1","name":"web","started_at":"2026-09-29T10:00:00Z","health":"healthy","restart_policy":"unless-stopped","networks":[{"name":"shop_default","ip":"172.18.0.3","ip6":""}]}]}`)
	s = Snapshot{}
	if err := UnmarshalSnapshotBounded(current, &s); err != nil {
		t.Fatal(err)
	}
	c := s.Containers[0]
	if c.Networks[0] != (NetworkAttachment{Name: "shop_default", IP: "172.18.0.3"}) || c.Health != "healthy" || c.RestartPolicy != "unless-stopped" || c.StartedAt.Year() != 2026 {
		t.Fatalf("attachment networks: %+v", c)
	}
	var n NetworkAttachment
	if err := json.Unmarshal([]byte(`42`), &n); err == nil {
		t.Fatal("a number is neither shape")
	}
}

func TestClampBoundsNewContainerFields(t *testing.T) {
	var nets []NetworkAttachment
	for i := 0; i < MaxNetworkAttachments+3; i++ {
		nets = append(nets, NetworkAttachment{Name: "n", IP: "not an ip"})
	}
	s := Snapshot{Containers: []Container{{ID: "c1", Health: "bogus\x00", RestartPolicy: strings.Repeat("r", 40), Networks: nets}}}
	Clamp(&s)
	c := s.Containers[0]
	if len(c.Networks) != MaxNetworkAttachments || c.Networks[0].IP != "" {
		t.Fatalf("networks not bounded or ip not validated: %d %q", len(c.Networks), c.Networks[0].IP)
	}
	if c.Health != "" || len(c.RestartPolicy) != 32 {
		t.Fatalf("health %q policy %q", c.Health, c.RestartPolicy)
	}
	s = Snapshot{Containers: []Container{{ID: "c1", Health: "unhealthy", Networks: []NetworkAttachment{{Name: "b", IP: "10.0.0.2", IP6: "fd00::2"}}}}}
	Clamp(&s)
	if s.Containers[0].Health != "unhealthy" || s.Containers[0].Networks[0].IP != "10.0.0.2" || s.Containers[0].Networks[0].IP6 != "fd00::2" {
		t.Fatalf("valid values must survive: %+v", s.Containers[0])
	}
	s = Snapshot{Containers: []Container{{ID: "c1"}}}
	Clamp(&s)
	if s.Containers[0].Networks == nil {
		t.Fatal("nil networks must become an empty list")
	}
}
