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
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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
		return f.result
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

// execCluster is a fake API server holding pod in namespace shop, which enforces Pod Security
// at level ("" for no label), and whose access review answers allowed.
func execCluster(pod *corev1.Pod, level string, allowed bool) *fake.Clientset {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	if level != "" {
		ns.Labels = map[string]string{podSecurityEnforce: level}
	}
	cs := fake.NewSimpleClientset(pod, ns)
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		review.Status.Allowed = allowed
		return true, review, nil
	})
	return cs
}

// execClient is a fake-clientset client whose executor is f; got receives the exec options.
func execClient(t *testing.T, f *fakeStreamer, pod *corev1.Pod) (*Client, *corev1.PodExecOptions) {
	t.Helper()
	c := NewFromClientset(execCluster(pod, "baseline", true))
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
		{"nil with no cancel is exit 0", nil, ptr(0)},
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
	errs, ready := make(chan error, 2), make(chan struct{}, 2)
	go func() { ready <- struct{}{}; _, err := s.Read(make([]byte, 16)); errs <- err }()
	go func() { ready <- struct{}{}; _, err := s.Write([]byte("typed")); errs <- err }()
	<-ready
	<-ready
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

func TestPodExecNilAfterCloseIsUnknown(t *testing.T) {
	f := newFake(nil)
	f.block = true
	c, _ := execClient(t, f, execPod(nil))
	s, err := c.OpenExec(context.Background(), execSpec())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
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
			c := NewFromClientset(execCluster(tc.pod, "baseline", true))
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

// Exec needs the pod's namespace to enforce Pod Security baseline or restricted, and the agent's
// own grant for create pods/exec on that pod (a SelfSubjectAccessReview); each refusal is its
// fixed error, and the executor is never built.
func TestPodExecGates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		level   string
		allowed bool
		want    error
	}{
		{"no pod security label", "", true, protocol.ErrExecPodSecurity},
		{"privileged", "privileged", true, protocol.ErrExecPodSecurity},
		{"review denied", "restricted", false, protocol.ErrExecForbidden},
	} {
		cs := execCluster(execPod(nil), tc.level, tc.allowed)
		c := NewFromClientset(cs)
		c.executor = func(string, string, *corev1.PodExecOptions) (streamer, error) {
			t.Fatalf("%s: the executor was built", tc.name)
			return nil, nil
		}
		if _, err := c.OpenExec(context.Background(), execSpec()); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err %v", tc.name, err)
		}
	}
	cs := execCluster(execPod(nil), "restricted", true)
	c := NewFromClientset(cs)
	c.executor = func(string, string, *corev1.PodExecOptions) (streamer, error) { return newFake(nil), nil }
	s, err := c.OpenExec(context.Background(), execSpec())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	var attrs []authorizationv1.ResourceAttributes
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "selfsubjectaccessreviews" {
			attrs = append(attrs, *a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview).Spec.ResourceAttributes)
		}
	}
	if want := (authorizationv1.ResourceAttributes{Namespace: "shop", Verb: "create", Resource: "pods", Subresource: "exec", Name: "web-0"}); len(attrs) != 1 || attrs[0] != want {
		t.Fatalf("reviews %+v", attrs)
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

// Each Pod Security baseline control, broken once, is refused with its own detail word; a pod
// using only what baseline allows passes.
func TestBaselineViolation(t *testing.T) {
	sc := func(mutate func(*corev1.SecurityContext)) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Spec.Containers[1].SecurityContext = &corev1.SecurityContext{}
			mutate(p.Spec.Containers[1].SecurityContext)
		}
	}
	podSC := func(mutate func(*corev1.PodSecurityContext)) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Spec.SecurityContext = &corev1.PodSecurityContext{}
			mutate(p.Spec.SecurityContext)
		}
	}
	unconfined := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
	unmasked := corev1.UnmaskedProcMount
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.Pod)
		want   string
	}{
		{"compliant", func(p *corev1.Pod) {
			p.Annotations = map[string]string{"container.apparmor.security.beta.kubernetes.io/app": "runtime/default", "seccomp.security.alpha.kubernetes.io/pod": "runtime/default"}
			p.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
			p.Spec.SecurityContext = &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				Sysctls:        []corev1.Sysctl{{Name: "net.ipv4.ip_unprivileged_port_start", Value: "0"}},
				SELinuxOptions: &corev1.SELinuxOptions{Type: "container_t", Level: "s0:c1,c2"},
			}
			def := corev1.DefaultProcMount
			p.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 80}}
			p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr(false), ProcMount: &def,
				Capabilities:    &corev1.Capabilities{Add: []corev1.Capability{"NET_BIND_SERVICE", "CHOWN"}, Drop: []corev1.Capability{"ALL"}},
				AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeRuntimeDefault}}
		}, ""},
		{"privileged container", sc(func(s *corev1.SecurityContext) { s.Privileged = ptr(true) }), "privileged"},
		{"privileged init container", func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "setup", SecurityContext: &corev1.SecurityContext{Privileged: ptr(true)}}}
		}, "privileged"},
		{"privileged ephemeral container", func(p *corev1.Pod) {
			p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", SecurityContext: &corev1.SecurityContext{Privileged: ptr(true)}}}}
		}, "privileged"},
		{"host network", func(p *corev1.Pod) { p.Spec.HostNetwork = true }, "host_namespace"},
		{"host pid", func(p *corev1.Pod) { p.Spec.HostPID = true }, "host_namespace"},
		{"host ipc", func(p *corev1.Pod) { p.Spec.HostIPC = true }, "host_namespace"},
		{"host path", func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "root", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
		}, "host_path"},
		{"capability", sc(func(s *corev1.SecurityContext) {
			s.Capabilities = &corev1.Capabilities{Add: []corev1.Capability{"CHOWN", "SYS_ADMIN"}}
		}), "capabilities"},
		{"proc mount", sc(func(s *corev1.SecurityContext) { s.ProcMount = &unmasked }), "proc_mount"},
		{"pod seccomp", podSC(func(s *corev1.PodSecurityContext) { s.SeccompProfile = unconfined }), "seccomp"},
		{"container seccomp", sc(func(s *corev1.SecurityContext) { s.SeccompProfile = unconfined }), "seccomp"},
		{"sysctl", podSC(func(s *corev1.PodSecurityContext) { s.Sysctls = []corev1.Sysctl{{Name: "kernel.msgmax", Value: "1"}} }), "sysctl"},
		{"host port", func(p *corev1.Pod) {
			p.Spec.Containers[1].Ports = []corev1.ContainerPort{{ContainerPort: 80, HostPort: 8080}}
		}, "host_port"},
		{"apparmor annotation", func(p *corev1.Pod) {
			p.Annotations = map[string]string{"container.apparmor.security.beta.kubernetes.io/app": "unconfined"}
		}, "apparmor"},
		{"legacy pod seccomp annotation", func(p *corev1.Pod) {
			p.Annotations = map[string]string{"seccomp.security.alpha.kubernetes.io/pod": "unconfined"}
		}, "seccomp"},
		{"legacy container seccomp annotation", func(p *corev1.Pod) {
			p.Annotations = map[string]string{"container.seccomp.security.alpha.kubernetes.io/app": "unconfined"}
		}, "seccomp"},
		{"apparmor field", sc(func(s *corev1.SecurityContext) {
			s.AppArmorProfile = &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}
		}), "apparmor"},
		{"selinux type", sc(func(s *corev1.SecurityContext) { s.SELinuxOptions = &corev1.SELinuxOptions{Type: "spc_t"} }), "selinux"},
		{"selinux user", podSC(func(s *corev1.PodSecurityContext) { s.SELinuxOptions = &corev1.SELinuxOptions{User: "system_u"} }), "selinux"},
		{"host process", podSC(func(s *corev1.PodSecurityContext) {
			s.WindowsOptions = &corev1.WindowsSecurityContextOptions{HostProcess: ptr(true)}
		}), "host_process"},
	} {
		if got := baselineViolation(execPod(tc.mutate)); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A privileged pod predating its namespace's baseline label is refused with pod_security before
// the access review, and the executor is never built.
func TestPodExecRefusesANonBaselinePod(t *testing.T) {
	cs := execCluster(execPod(func(p *corev1.Pod) {
		p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr(true)}
	}), "baseline", true)
	c := NewFromClientset(cs)
	c.executor = func(string, string, *corev1.PodExecOptions) (streamer, error) {
		t.Fatal("the executor was built for a privileged pod")
		return nil, nil
	}
	if _, err := c.OpenExec(context.Background(), execSpec()); !errors.Is(err, protocol.ErrExecPodSecurity) || protocol.ExecRefusal(err) != "pod_security" {
		t.Fatalf("err %v", err)
	}
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "selfsubjectaccessreviews" {
			t.Fatal("the access review ran for a refused pod")
		}
	}
}
