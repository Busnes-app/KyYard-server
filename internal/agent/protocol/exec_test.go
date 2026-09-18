package protocol

import (
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
