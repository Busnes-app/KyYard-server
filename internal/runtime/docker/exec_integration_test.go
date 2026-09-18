package docker

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// Explicit opt-in: this test creates only its own network-isolated disposable
// container from an already-present image with /bin/sh. It never pulls an image.
func TestExecRealDocker(t *testing.T) {
	image := os.Getenv("KY_TEST_DOCKER_EXEC_IMAGE")
	if image == "" {
		t.Skip("set KY_TEST_DOCKER_EXEC_IMAGE to an existing shell image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "docker", "run", "-d", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pull", "never", image, "sh", "-c", "sleep 120").CombinedOutput()
	if err != nil {
		t.Fatalf("fixture: %v: %s", err, raw)
	}
	id := strings.TrimSpace(string(raw))
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(cleanup, "docker", "rm", "-fv", id).CombinedOutput(); err != nil {
			t.Errorf("cleanup: %v: %s", err, out)
		}
	})
	raw, err = exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Image}}", id).Output()
	if err != nil {
		t.Fatal(err)
	}
	client := New("/var/run/docker.sock")
	session, err := client.OpenExec(ctx, protocol.ExecSpec{Container: id, ImageID: strings.TrimSpace(string(raw)), User: "65534:65534", Argv: []string{"/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Resize(ctx, protocol.TerminalSize{Rows: 41, Columns: 113}); err != nil {
		t.Fatal(err)
	}
	output := make(chan []byte, 1)
	readErrors := make(chan error, 1)
	go func() { b, err := io.ReadAll(io.LimitReader(session, 32768)); output <- b; readErrors <- err }()
	// Interactive output proves raw PTY input, resize and the explicitly selected user.
	if _, err := session.Write([]byte("stty size; id -u; printf 'exec-proof-ok\\n'; exit 7\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-output:
		if err := <-readErrors; err != nil {
			t.Fatalf("read: %v: %q", err, out)
		}
		text := string(out)
		for _, want := range []string{"41 113", "65534", "exec-proof-ok"} {
			if !strings.Contains(text, want) {
				t.Errorf("missing %q in %q", want, text)
			}
		}
	case <-ctx.Done():
		t.Fatal("PTY did not finish", ctx.Err())
	}
	// Attachment EOF may precede Docker publishing the exit result. Inspect, don't infer.
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, err := session.Inspect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !status.Running {
			if status.ExitCode == nil || *status.ExitCode != 7 {
				t.Fatal("wrong exit status", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exec did not exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
