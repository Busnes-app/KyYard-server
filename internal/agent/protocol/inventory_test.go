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
