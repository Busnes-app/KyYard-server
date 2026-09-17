package protocol

import (
	"bytes"
	"encoding/json"
	"runtime"
	"slices"
	"testing"
)

// A payload at the byte cap made of the smallest possible elements must not expand into a
// Go value many times its size: the list caps apply while decoding, not after.
func TestBoundedDecodeCapsAllocation(t *testing.T) {
	var b bytes.Buffer
	b.WriteString(`{"generation":1,"containers":[`)
	for i := 0; b.Len() < MaxSnapshotBytes-16; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"c"}`)
	}
	b.WriteString(`]}`)
	payload := b.Bytes()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	var s Snapshot
	err := UnmarshalSnapshotBounded(payload, &s)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Containers) != MaxContainers || !slices.Contains(s.Truncated, "containers") {
		t.Fatalf("decode not capped: %d containers, truncated %v", len(s.Containers), s.Truncated)
	}
	if used := after.TotalAlloc - before.TotalAlloc; used > 8<<20 {
		t.Fatalf("bounded decode allocated %d bytes for a %d-byte payload", used, len(payload))
	}
	// The plain decoder is what the bound protects against.
	var plain Snapshot
	if json.Unmarshal(payload, &plain) != nil || len(plain.Containers) <= MaxContainers {
		t.Fatal("test payload does not exercise the bound")
	}
}

func TestBoundedHelloCapsCapabilities(t *testing.T) {
	var b bytes.Buffer
	b.WriteString(`{"state":"active","capabilities":[`)
	for i := 0; i < 500; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"cap"`)
	}
	b.WriteString(`]}`)
	var h Hello
	if err := UnmarshalHelloBounded(b.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if h.State != "active" || len(h.Capabilities) != 64 {
		t.Fatalf("hello not bounded: %+v", h)
	}
}
