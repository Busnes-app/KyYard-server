package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExecSpecBoundaries(t *testing.T) {
	valid := ExecSpec{Container: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), User: "1000:1000", Argv: []string{"/bin/sh", "-c", "printf '%s' \"$HOME\""}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ExecSpec){
		"container name":   func(s *ExecSpec) { s.Container = "web" },
		"path traversal":   func(s *ExecSpec) { s.Container = "../../images" },
		"no image pin":     func(s *ExecSpec) { s.ImageID = "" },
		"no explicit user": func(s *ExecSpec) { s.User = "" },
		"invalid user":     func(s *ExecSpec) { s.User = "root\nother" },
		"no command":       func(s *ExecSpec) { s.Argv = nil },
		"empty executable": func(s *ExecSpec) { s.Argv = []string{""} },
		"NUL argument":     func(s *ExecSpec) { s.Argv = []string{"sh", "a\x00b"} },
		"invalid UTF8":     func(s *ExecSpec) { s.Argv = []string{"sh", string([]byte{255})} },
		"large argv":       func(s *ExecSpec) { s.Argv = []string{strings.Repeat("x", MaxExecArgumentBytes+1)} },
		"many arguments":   func(s *ExecSpec) { s.Argv = make([]string, MaxExecArgs+1); s.Argv[0] = "sh" },
	} {
		t.Run(name, func(t *testing.T) {
			s := valid
			change(&s)
			if s.Validate() == nil {
				t.Fatal("accepted invalid exec spec")
			}
		})
	}
	for _, size := range []TerminalSize{{0, 80}, {24, 0}, {513, 80}, {24, 513}, {-1, 80}} {
		if size.Validate() == nil {
			t.Fatal("accepted size", size)
		}
	}
	if (TerminalSize{512, 512}).Validate() != nil {
		t.Fatal("refused maximum terminal size")
	}
}

func TestExecSpecPodTarget(t *testing.T) {
	valid := ExecSpec{Pod: &PodTarget{Namespace: "shop", Name: "web-7d9f8b6c5-x2x4z", Container: "web", UID: testPodUID}, Argv: []string{"/bin/sh"}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	if valid.ValidateFor(RuntimeKubernetes) != nil || valid.ValidateFor(RuntimeDocker) == nil {
		t.Fatal("pod exec belongs to a cluster")
	}
	docker := ExecSpec{Container: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), User: "1000", Argv: []string{"sh"}}
	if docker.ValidateFor(RuntimeDocker) != nil || docker.ValidateFor(RuntimeKubernetes) == nil {
		t.Fatal("container exec belongs to docker")
	}
	for name, change := range map[string]func(*ExecSpec){
		"pod and container id": func(s *ExecSpec) { s.Container = strings.Repeat("a", 64) },
		"pod and image id":     func(s *ExecSpec) { s.ImageID = "sha256:" + strings.Repeat("b", 64) },
		"pod and user":         func(s *ExecSpec) { s.User = "root" },
		"no container":         func(s *ExecSpec) { s.Pod.Container = "" },
		"no uid":               func(s *ExecSpec) { s.Pod.UID = "" },
		"uid not a uuid":       func(s *ExecSpec) { s.Pod.UID = "x" },
		"namespace":            func(s *ExecSpec) { s.Pod.Namespace = "../x" },
		"pod name":             func(s *ExecSpec) { s.Pod.Name = "Web" },
		"no argv":              func(s *ExecSpec) { s.Argv = nil },
		"NUL argv":             func(s *ExecSpec) { s.Argv = []string{"sh", "\x00"} },
	} {
		t.Run(name, func(t *testing.T) {
			s := valid
			p := *valid.Pod
			s.Pod = &p
			change(&s)
			if s.Validate() == nil {
				t.Fatal("accepted")
			}
		})
	}
	raw, _ := json.Marshal(docker)
	if strings.Contains(string(raw), "pod") {
		t.Fatalf("docker exec spec grew a pod key: %s", raw)
	}
}
