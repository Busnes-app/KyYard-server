package protocol

import (
	"bytes"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// An inventory frame is agent-supplied. The server decodes it bounded, clamps it and stores
// Shrink's encoding; that encoding must fit the caps and be a fixed point of the same path, or
// what is stored is not what the next read would accept.
func FuzzSnapshotDecodeClamp(f *testing.F) {
	for _, doc := range []string{
		`{"generation":1}`,
		`{"generation":1,"kubernetes":null}`,
		`{"kubernetes":{"pods":{}}}`,
		`{"generation":1,"containers":[{"id":"a","mounts":[{"kind":"volume","source":"v","target":"/m"}]},{"id":"b"},{"id":"c","mounts":[]},{"id":"d","mounts":null}]}`,
		`{"generation":1,"kubernetes":{"nodes":[{"name":"n1","ready":true}],"pods":[{"namespace":"shop","name":"web","containers":[{"name":"c"}]}]}}`,
		`{"generation":2,"observed_at":"2026-01-02T03:04:05+01:00","engine":{"runtime":"docker","hostname":"h‮"},"containers":[{"id":"x","name":"web‮evil\nline","labels":{"k\u0000":"v","k":"w"},"health":"bogus","ports":[{"host_ip":"0.0.0.0","host":80,"container":80,"protocol":"tcp"}],"network_attachments":[{"name":"bridge","ip":"fe80::1","ip6":"10.0.0.1"}]}],"images":[{"id":"i","tags":["a"]}],"networks":[{"id":"n"}],"volumes":[{"name":"v"}],"truncated":["containers","bogus"]}`,
		`{"containers":[{"mounts":[{"kind":"tmpfs","source":"` + strings.Repeat("s", MaxImageRefBytes+1) + `","target":"/t"}]}]}`,
		`{"containers":{}}`, `[]`, `null`, `{"generation":-1}`,
	} {
		f.Add([]byte(doc))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > MaxSnapshotBytes {
			return // refused before decoding
		}
		var s Snapshot
		if UnmarshalSnapshotBounded(data, &s) != nil {
			return
		}
		Clamp(&s)
		if len(s.Containers) > MaxContainers || len(s.Images) > MaxImages || len(s.Networks) > MaxNetworks || len(s.Volumes) > MaxVolumes {
			t.Fatalf("clamped lists over their caps: %d %d %d %d", len(s.Containers), len(s.Images), len(s.Networks), len(s.Volumes))
		}
		for _, c := range s.Containers {
			if len(c.Labels) > MaxLabels || len(c.Mounts) > MaxMounts || len(c.NetworkAttachments) > MaxNetworkAttachments || len(c.Networks) > MaxNetworkAttachments || len(c.Name) > MaxNameBytes || len(c.Image) > MaxImageRefBytes {
				t.Fatalf("clamped container over its bounds: %+v", c)
			}
		}
		stored := Shrink(&s)
		var back Snapshot
		if err := UnmarshalSnapshotBounded(stored, &back); err != nil {
			t.Fatalf("stored encoding does not decode: %v\n%s", err, stored)
		}
		Clamp(&back)
		again := Shrink(&back)
		if !bytes.Equal(stored, again) {
			t.Fatalf("stored encoding is not a fixed point:\n%s\n%s", stored, again)
		}
	})
}

// CleanText guards every agent string the server stores. Its output is valid UTF-8 within max
// bytes, carries none of the characters it removes, is a prefix of the uncut cleaning, and
// cleaning it again changes nothing.
func FuzzCleanText(f *testing.F) {
	f.Add("héllo wörld", 7)
	f.Add("web‮evil\nline", MaxNameBytes)
	f.Add("a b c⁦d⁩", 16)
	f.Add("\xff\xfe\x00x", 4)
	f.Add("日本語", 2)
	f.Add("", 0)
	f.Add("abc", -1)
	f.Fuzz(func(t *testing.T, s string, max int) {
		got := CleanText(s, max)
		if len(got) > max && got != "" {
			t.Fatalf("CleanText(%q, %d) = %q, over max", s, max, got)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("CleanText(%q, %d) = %q, invalid UTF-8", s, max, got)
		}
		for _, r := range got {
			if unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == utf8.RuneError {
				t.Fatalf("CleanText(%q, %d) = %q keeps %U", s, max, got, r)
			}
		}
		if !strings.HasPrefix(CleanText(s, len(s)), got) {
			t.Fatalf("CleanText(%q, %d) = %q is not a prefix of the uncut cleaning", s, max, got)
		}
		if again := CleanText(got, max); again != got {
			t.Fatalf("CleanText not idempotent: %q then %q", got, again)
		}
	})
}
