package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/exec"
)

// fakeStreamer stands in for the SPDY executor: it echoes stdin to stdout until it reads "exit",
// forwards every size the queue yields and returns result (or the context's error).
type fakeStreamer struct {
	sizes  chan remotecommand.TerminalSize
	result error
	// block makes Stream wait for its context without touching stdin or stdout.
	block bool
	opts  remotecommand.StreamOptions
}

func (f *fakeStreamer) StreamWithContext(ctx context.Context, opts remotecommand.StreamOptions) error {
	f.opts = opts
	go func() {
		for s := opts.TerminalSizeQueue.Next(); s != nil; s = opts.TerminalSizeQueue.Next() {
			select {
			case f.sizes <- *s:
			default:
			}
		}
	}()
	if f.block {
		<-ctx.Done()
		if f.result != nil {
			return f.result
		}
		return ctx.Err()
	}
	buf := make([]byte, 1024)
	for {
		n, err := opts.Stdin.Read(buf)
		if err != nil {
			return err
		}
		if _, err := opts.Stdout.Write(buf[:n]); err != nil {
			return err
		}
		if bytes.Contains(buf[:n], []byte("exit")) {
			return f.result
		}
	}
}

const execPodUID = "7c6b5a49-3827-4165-9504-a3b2c1d0e9f8"

func execPod(mutate func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-0", UID: execPodUID},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}, {Name: "sidecar"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

func execSpec() protocol.ExecSpec {
	return protocol.ExecSpec{Argv: []string{"sh"}, Pod: &protocol.PodTarget{Namespace: "shop", Name: "web-0", Container: "app", UID: execPodUID}}
}

// execClient is a fake-clientset client whose executor is f; got receives the exec options.
func execClient(t *testing.T, f *fakeStreamer, pod *corev1.Pod) (*Client, *corev1.PodExecOptions) {
	t.Helper()
	c := NewFromClientset(fake.NewSimpleClientset(pod))
	got := &corev1.PodExecOptions{}
	c.executor = func(namespace, name string, opts *corev1.PodExecOptions) (streamer, error) {
		if namespace != "shop" || name != "web-0" {
			t.Errorf("executor for %s/%s", namespace, name)
		}
		*got = *opts
		return f, nil
	}
	return c, got
}

func newFake(result error) *fakeStreamer {
	return &fakeStreamer{sizes: make(chan remotecommand.TerminalSize, 8), result: result}
}

func TestPodExecEchoesAndReportsTheExitCode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result error
		code   *int
	}{
		{"exit 3", exec.CodeExitError{Err: errors.New("command terminated with exit code 3"), Code: 3}, ptr(3)},
		// client-go answers nil both for a success status and for an error stream that ended
		// empty (a dropped connection), so nil is no proof of exit 0.
		{"nil is unknown", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(tc.result)
			c, got := execClient(t, f, execPod(nil))
			s, err := c.OpenExec(context.Background(), execSpec())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if st, err := s.Inspect(context.Background()); err != nil || !st.Running || st.ExitCode != nil {
				t.Fatalf("before exit: %+v %v", st, err)
			}
			if _, err := s.Write([]byte("hello\n")); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 64)
			if n, err := s.Read(buf); err != nil || string(buf[:n]) != "hello\n" {
				t.Fatalf("read %q %v", buf[:n], err)
			}
			if _, err := s.Write([]byte("exit\n")); err != nil {
				t.Fatal(err)
			}
			rest, err := io.ReadAll(s)
			if err != nil || string(rest) != "exit\n" {
				t.Fatalf("rest %q %v", rest, err)
			}
			st, err := s.Inspect(context.Background())
			if err != nil || st.Running || (st.ExitCode == nil) != (tc.code == nil) || (st.ExitCode != nil && *st.ExitCode != *tc.code) {
				t.Fatalf("after exit: %+v %v", st, err)
			}
			if got.Container != "app" || strings.Join(got.Command, " ") != "sh" || !got.Stdin || !got.Stdout || got.Stderr || !got.TTY {
				t.Fatalf("exec options %+v", got)
			}
			if !f.opts.Tty || f.opts.Stderr != nil || f.opts.TerminalSizeQueue == nil {
				t.Fatalf("stream options %+v", f.opts)
			}
			if _, err := s.Write([]byte("late")); err == nil {
				t.Fatal("a write after the process ended succeeded")
			}
		})
	}
}

func TestPodExecResizeReachesTheQueue(t *testing.T) {
	f := newFake(nil)
	f.block = true
	c, _ := execClient(t, f, execPod(nil))
	s, err := c.OpenExec(context.Background(), execSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Resize(context.Background(), protocol.TerminalSize{Rows: 24, Columns: 80}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-f.sizes:
		if got.Width != 80 || got.Height != 24 {
			t.Fatalf("size %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the size never reached the queue")
	}
	if err := s.Resize(context.Background(), protocol.TerminalSize{Rows: 0, Columns: 80}); err == nil {
		t.Fatal("an invalid size was accepted")
	}
	// A burst larger than the queue never blocks the caller.
	for i := 1; i <= 64; i++ {
		if err := s.Resize(context.Background(), protocol.TerminalSize{Rows: i, Columns: 80}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPodExecCloseUnblocksReadAndWrite(t *testing.T) {
	// An exit status arriving on a stream we cancelled is not believed.
	f := newFake(exec.CodeExitError{Err: errors.New("command terminated with exit code 0"), Code: 0})
	f.block = true
	c, _ := execClient(t, f, execPod(nil))
	s, err := c.OpenExec(context.Background(), execSpec())
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	go func() { _, err := s.Read(make([]byte, 16)); errs <- err }()
	go func() { _, err := s.Write([]byte("typed")); errs <- err }()
	time.Sleep(20 * time.Millisecond)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.Close() }()
	}
	wg.Wait()
	for range 2 {
		select {
		case err := <-errs:
			if err == nil {
				t.Fatal("a blocked call returned no error after Close")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Close did not unblock Read and Write")
		}
	}
	waitStopped(t, s)
}

func TestPodExecTransportErrorHasNoExitCode(t *testing.T) {
	f := newFake(errors.New("stream reset"))
	c, _ := execClient(t, f, execPod(nil))
	s, err := c.OpenExec(context.Background(), execSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Write([]byte("exit")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(s); err == nil {
		t.Fatal("a transport failure read as a clean end")
	}
	st, err := s.Inspect(context.Background())
	if err != nil || st.Running || st.ExitCode != nil {
		t.Fatalf("status %+v %v", st, err)
	}
}

func TestPodExecDeadlinesClose(t *testing.T) {
	for _, tc := range []struct {
		name           string
		idle, lifetime time.Duration
		want           error
	}{
		{"idle", 50 * time.Millisecond, time.Hour, ErrExecIdle},
		{"lifetime", time.Hour, 50 * time.Millisecond, ErrExecLifetime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(nil)
			f.block = true
			c, _ := execClient(t, f, execPod(nil))
			s, err := c.openExec(context.Background(), execSpec(), tc.idle, tc.lifetime)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			done := make(chan error, 1)
			go func() { _, err := s.Read(make([]byte, 16)); done <- err }()
			select {
			case err := <-done:
				if !errors.Is(err, tc.want) {
					t.Fatalf("read ended with %v, want %v", err, tc.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the deadline never closed the session")
			}
		})
	}
}

func TestPodExecInputResetsTheIdleDeadline(t *testing.T) {
	f := newFake(nil)
	c, _ := execClient(t, f, execPod(nil))
	s, err := c.openExec(context.Background(), execSpec(), 300*time.Millisecond, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	buf := make([]byte, 16)
	for range 12 {
		time.Sleep(50 * time.Millisecond)
		if _, err := s.Write([]byte("k")); err != nil {
			t.Fatalf("input did not keep the session open: %v", err)
		}
		if _, err := s.Read(buf); err != nil {
			t.Fatal(err)
		}
	}
}

// waitStopped waits for Stream to return: Inspect then reports no exit code.
func waitStopped(t *testing.T, s interface {
	Inspect(context.Context) (protocol.ExecStatus, error)
}) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, err := s.Inspect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !st.Running {
			if st.ExitCode != nil {
				t.Fatalf("a closed attachment reported exit code %d", *st.ExitCode)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the stream never returned after Close")
}

func TestPodExecRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		spec func(*protocol.ExecSpec)
	}{
		{"uid mismatch", execPod(func(p *corev1.Pod) { p.UID = "11111111-2222-4333-8444-555555555555" }), nil},
		{"not running", execPod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }), nil},
		{"missing container", execPod(nil), func(s *protocol.ExecSpec) { s.Pod.Container = "other" }},
		{"init container only", execPod(func(p *corev1.Pod) { p.Spec.InitContainers = []corev1.Container{{Name: "setup"}} }), func(s *protocol.ExecSpec) { s.Pod.Container = "setup" }},
		{"pod gone", execPod(func(p *corev1.Pod) { p.Name = "web-1" }), nil},
		{"docker shaped", execPod(nil), func(s *protocol.ExecSpec) { s.Pod = nil }},
		{"no uid", execPod(nil), func(s *protocol.ExecSpec) { s.Pod.UID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewFromClientset(fake.NewSimpleClientset(tc.pod))
			c.executor = func(string, string, *corev1.PodExecOptions) (streamer, error) {
				t.Fatal("the executor was built for a refused target")
				return nil, nil
			}
			spec := execSpec()
			if tc.spec != nil {
				tc.spec(&spec)
			}
			if s, err := c.OpenExec(context.Background(), spec); err == nil {
				s.Close()
				t.Fatal("exec opened")
			}
		})
	}
}

func TestPodExecUnavailableWithoutAClusterConfig(t *testing.T) {
	c := NewFromClientset(fake.NewSimpleClientset(execPod(nil)))
	if _, err := c.OpenExec(context.Background(), execSpec()); !errors.Is(err, ErrExecUnavailable) {
		t.Fatalf("err %v", err)
	}
}

// TestPodExecOnARealCluster runs `sh -c 'echo hi; exit 3'` in a fixture pod of the cluster
// KY_TEST_KUBECONFIG names, as that kubeconfig's user, and reads the output and the exit code.
// KY_TEST_EXEC_IMAGE names a digest-pinned image with sh that keeps running under `sleep`.
func TestPodExecOnARealCluster(t *testing.T) {
	path := os.Getenv("KY_TEST_KUBECONFIG")
	if path == "" {
		t.Skip("KY_TEST_KUBECONFIG is not set")
	}
	image := os.Getenv("KY_TEST_EXEC_IMAGE")
	if image == "" {
		t.Fatal("KY_TEST_EXEC_IMAGE must name a digest-pinned image with sh")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cs := k8s.NewForConfigOrDie(cfg)
	const ns = "kyyard-test-exec"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	_ = cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	t.Cleanup(func() { _ = cs.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{}) })
	for {
		_, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second) // a namespace from an earlier run is still terminating
	}
	pod, err := cs.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "shell"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "shell", Image: image, Command: []string{"sleep", "3600"}}}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for pod.Status.Phase != corev1.PodRunning {
		if ctx.Err() != nil {
			t.Fatalf("pod phase %s", pod.Status.Phase)
		}
		time.Sleep(time.Second)
		if pod, err = cs.CoreV1().Pods(ns).Get(ctx, "shell", metav1.GetOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	s, err := c.OpenExec(ctx, protocol.ExecSpec{Argv: []string{"sh", "-c", "echo hi; exit 3"}, Pod: &protocol.PodTarget{Namespace: ns, Name: "shell", Container: "shell", UID: string(pod.UID)}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out, err := io.ReadAll(s)
	if err != nil || !strings.Contains(string(out), "hi") {
		t.Fatalf("output %q %v", out, err)
	}
	st, err := s.Inspect(ctx)
	if err != nil || st.Running || st.ExitCode == nil || *st.ExitCode != 3 {
		t.Fatalf("status %+v %v", st, err)
	}
}
