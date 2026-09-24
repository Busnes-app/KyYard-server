package protocol

import (
	"strings"
	"testing"
	"time"
)

func TestInspectionResultValidation(t *testing.T) {
	target := InspectionTarget{ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}
	good := ContainerInspection{Target: target, ObservedAt: time.Now(), State: "running", RestartPolicy: "no", NetworkMode: "none", ImagePlatform: ImagePlatform{OS: "linux", Architecture: "amd64"}, ConfigurationVerified: true, Unsupported: []string{}}
	if err := good.Validate(target, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ContainerInspection){
		func(r *ContainerInspection) { r.ConfigurationVerified = false },
		func(r *ContainerInspection) { r.Target.ContainerID = strings.Repeat("c", 64) },
		func(r *ContainerInspection) { r.ObservedAt = time.Now().Add(-time.Minute) },
		func(r *ContainerInspection) { r.ObservedAt = time.Now().Add(time.Minute) },
		func(r *ContainerInspection) { r.NetworkMode = "secret-canary" },
		func(r *ContainerInspection) { r.ImagePlatform.Architecture = strings.Repeat("a", 65) },
		func(r *ContainerInspection) { r.Mounts.Bind = -1 },
		func(r *ContainerInspection) { r.Mounts.Bind = 64; r.Mounts.Tmpfs = 1 },
		func(r *ContainerInspection) { r.Mounts.ReadOnly = 1 },
		func(r *ContainerInspection) {
			r.Ports = []Port{{Container: 80, Host: 80, HostIP: "secret-canary", Protocol: "tcp"}}
		},
		func(r *ContainerInspection) {
			r.Ports = []Port{{Container: 80, Host: 0, HostIP: "0.0.0.0", Protocol: "tcp"}}
		},
	} {
		bad := good
		mutate(&bad)
		if bad.Validate(target, time.Now()) == nil {
			t.Fatalf("invalid result accepted: %+v", bad)
		}
	}
}

// ConfigurationVerified says exactly that no code was reported, and codes come from one closed list.
func TestInspectionUnsupportedVocabulary(t *testing.T) {
	target := InspectionTarget{ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}
	base := ContainerInspection{Target: target, ObservedAt: time.Now(), State: "running", RestartPolicy: "no", NetworkMode: "bridge", ImagePlatform: ImagePlatform{OS: "linux", Architecture: "amd64"}}
	for name, tc := range map[string]struct {
		verified bool
		codes    []string
		ok       bool
	}{
		"verified, no codes":     {true, []string{}, true},
		"verified, nil codes":    {true, nil, true},
		"unsupported with codes": {false, []string{"privileged", "devices"}, true},
		"every code":             {false, UnsupportedCodes, true},
		"verified with a code":   {true, []string{"privileged"}, false},
		"unverified, no codes":   {false, []string{}, false},
		"unknown code":           {false, []string{"secret-canary"}, false},
		"repeated code":          {false, []string{"privileged", "privileged"}, false},
		"too many":               {false, make([]string, MaxUnsupported+1), false},
	} {
		r := base
		r.ConfigurationVerified, r.Unsupported = tc.verified, tc.codes
		if err := r.Validate(target, time.Now()); (err == nil) != tc.ok {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(UnsupportedCodes) != 29 || len(UnsupportedCodes) > MaxUnsupported || UnsupportedCodes[0] != "mount_type" || UnsupportedCodes[28] != "image_config" {
		t.Fatalf("vocabulary: %v", UnsupportedCodes)
	}
}
