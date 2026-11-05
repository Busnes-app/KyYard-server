package protocol

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGrantExpiryRequiresOtherwiseValidFields(t *testing.T) {
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	exec := ExecOpen{Stream: "stream", Actor: "actor", Endpoint: "endpoint", Connection: make([]byte, 32), Expires: now.Add(-time.Second),
		Spec: ExecSpec{Container: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), User: "65534", Argv: []string{"/bin/sh"}},
		Size: TerminalSize{Rows: 24, Columns: 80},
	}
	inspection := InspectionOpen{Request: "request", Actor: "actor", Endpoint: "endpoint", Connection: make([]byte, 32), Expires: exec.Expires,
		Target: InspectionTarget{ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000},
	}
	if !errors.Is(exec.Validate(now), ErrGrantExpiry) || !errors.Is(inspection.ValidateFor(now, RuntimeDocker), ErrGrantExpiry) {
		t.Fatal("valid fields with expired grant must have expiry classification")
	}
	for name, mutate := range map[string]func(*ExecOpen){
		"missing time":       func(r *ExecOpen) { r.Expires = time.Time{} },
		"missing actor":      func(r *ExecOpen) { r.Actor = "" },
		"invalid target":     func(r *ExecOpen) { r.Spec.Container = "name" },
		"invalid dimensions": func(r *ExecOpen) { r.Size.Rows = 0 },
	} {
		t.Run("exec/"+name, func(t *testing.T) {
			bad := exec
			mutate(&bad)
			if err := bad.Validate(now); err == nil || errors.Is(err, ErrGrantExpiry) {
				t.Fatal("malformed grant classified as expiry only", err)
			}
		})
	}
	for name, mutate := range map[string]func(*InspectionOpen){
		"missing time":   func(r *InspectionOpen) { r.Expires = time.Time{} },
		"missing actor":  func(r *InspectionOpen) { r.Actor = "" },
		"invalid target": func(r *InspectionOpen) { r.Target.ContainerID = "name" },
	} {
		t.Run("inspection/"+name, func(t *testing.T) {
			bad := inspection
			mutate(&bad)
			if err := bad.ValidateFor(now, RuntimeDocker); err == nil || errors.Is(err, ErrGrantExpiry) {
				t.Fatal("malformed grant classified as expiry only", err)
			}
		})
	}
	if err := inspection.ValidateFor(now, RuntimeKubernetes); err == nil || errors.Is(err, ErrGrantExpiry) {
		t.Fatal("wrong runtime classified as expiry only", err)
	}
}
