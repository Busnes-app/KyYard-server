package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/client-go/util/exec"
)

var (
	ErrExecUnavailable = errors.New("exec unavailable: the client has no cluster configuration")
	ErrExecClosed      = errors.New("exec stream closed; process exit is not established")
	ErrExecIdle        = errors.New("exec input idle timeout")
	ErrExecLifetime    = errors.New("exec absolute timeout")
	errExecEnded       = errors.New("exec process ended")
	errExecStream      = errors.New("exec stream failed; process exit is not established")
)

// streamer is the part of remotecommand.Executor a session drives; tests inject a fake.
type streamer interface {
	StreamWithContext(ctx context.Context, opts remotecommand.StreamOptions) error
}

// spdyExecutor builds executors on pods/{name}/exec (POST, the `create pods/exec` grant) that
// refuse redirects.
func spdyExecutor(cfg *rest.Config, cs k8s.Interface) func(namespace, pod string, opts *corev1.PodExecOptions) (streamer, error) {
	return func(namespace, pod string, opts *corev1.PodExecOptions) (streamer, error) {
		transport, upgrader, err := spdy.RoundTripperFor(cfg)
		if err != nil {
			return nil, err
		}
		url := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").VersionedParams(opts, scheme.ParameterCodec).URL()
		return remotecommand.NewSPDYExecutorRejectRedirects(transport, upgrader, http.MethodPost, url)
	}
}

// podExec is one TTY stream (stdout and stderr merged) into a pod's container. One reader and
// one writer may run concurrently; Close unblocks both. Closing is not a promise that the
// process has ended. No output or argv is logged.
type podExec struct {
	stdinR, stdoutR *io.PipeReader
	stdinW, stdoutW *io.PipeWriter
	sizes           chan remotecommand.TerminalSize
	ctx             context.Context
	cancel          context.CancelCauseFunc
	stopLifetime    context.CancelFunc
	once            sync.Once
	idleMu          sync.Mutex
	idle            *time.Timer
	idleBudget      time.Duration
	mu              sync.Mutex
	ended           bool
	exit            *int
}

// OpenExec attaches a TTY to spec.Pod's container, running spec.Argv, once the pod read back
// has the same UID, is Running and has that container, its namespace enforces Pod Security
// baseline or restricted and the pod itself meets baseline (protocol.ErrExecPodSecurity), and an
// access review allows create pods/exec on it (protocol.ErrExecForbidden). See protocol §7.
func (c *Client) OpenExec(ctx context.Context, spec protocol.ExecSpec) (client.ExecSession, error) {
	return c.openExec(ctx, spec, protocol.ExecIdleTimeout, protocol.ExecAbsoluteTimeout)
}

func (c *Client) openExec(parent context.Context, spec protocol.ExecSpec, idle, lifetime time.Duration) (*podExec, error) {
	if err := spec.ValidateFor(protocol.RuntimeKubernetes); err != nil {
		return nil, err
	}
	if c.executor == nil {
		return nil, ErrExecUnavailable
	}
	t := spec.Pod
	getCtx, cancelGet := context.WithTimeout(parent, callBudget)
	pod, err := c.cs.CoreV1().Pods(t.Namespace).Get(getCtx, t.Name, metav1.GetOptions{})
	cancelGet()
	switch {
	case apierrors.IsNotFound(err):
		return nil, errors.New("exec target pod not found")
	case err != nil:
		return nil, errors.New("exec target pod could not be read")
	case string(pod.UID) != t.UID || pod.Status.Phase != corev1.PodRunning || !hasContainer(pod, t.Container):
		return nil, errors.New("exec target identity or running state changed")
	}
	if err := c.execAllowed(parent, t, pod); err != nil {
		return nil, err
	}
	st, err := c.executor(t.Namespace, t.Name, &corev1.PodExecOptions{Container: t.Container, Command: spec.Argv, Stdin: true, Stdout: true, TTY: true})
	if err != nil {
		return nil, errors.New("exec transport could not be configured")
	}
	lifetimeCtx, stopLifetime := context.WithTimeoutCause(parent, lifetime, ErrExecLifetime)
	ctx, cancel := context.WithCancelCause(lifetimeCtx)
	s := &podExec{sizes: make(chan remotecommand.TerminalSize, 1), ctx: ctx, cancel: cancel, stopLifetime: stopLifetime, idleBudget: idle, idle: time.NewTimer(idle)}
	s.stdinR, s.stdinW = io.Pipe()
	s.stdoutR, s.stdoutW = io.Pipe()
	go func() {
		select {
		case <-ctx.Done():
			s.closeWith(context.Cause(ctx))
		case <-s.idle.C:
			s.closeWith(ErrExecIdle)
		}
	}()
	go func() {
		s.finish(st.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: s.stdinR, Stdout: s.stdoutW, Tty: true, TerminalSizeQueue: s}))
	}()
	return s, nil
}

// execAllowed refuses a namespace that does not enforce Pod Security baseline or restricted
// (entering a privileged pod is host access; the same rule as a deploy), a pod that does not
// meet baseline itself (it may predate the label) and an agent whose own grant lacks create
// pods/exec on the pod, each with its fixed error so the operator sees why.
func (c *Client) execAllowed(parent context.Context, t *protocol.PodTarget, pod *corev1.Pod) error {
	ctx, cancel := context.WithTimeout(parent, callBudget)
	defer cancel()
	r := &run{c: c, parent: parent, namespace: t.Namespace}
	switch _, code, _ := r.podSecurity(ctx); code {
	case "":
	case "pod_security":
		return protocol.ErrExecPodSecurity
	case "forbidden":
		return protocol.ErrExecForbidden
	default:
		return errors.New("exec target namespace could not be read")
	}
	if v := baselineViolation(pod); v != "" {
		return fmt.Errorf("%w: %s", protocol.ErrExecPodSecurity, v)
	}
	ok, err := r.review(ctx, authorizationv1.ResourceAttributes{Namespace: t.Namespace, Verb: "create", Resource: "pods", Subresource: "exec", Name: t.Name})
	switch {
	case err != nil:
		return errors.New("exec access review failed")
	case !ok:
		return protocol.ErrExecForbidden
	}
	return nil
}

// Pod Security baseline's allowed capability additions, safe sysctls and SELinux types
// (https://kubernetes.io/docs/concepts/security/pod-security-standards/#baseline).
var (
	baselineCapabilities = []corev1.Capability{"AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "MKNOD", "NET_BIND_SERVICE", "SETFCAP", "SETGID", "SETPCAP", "SETUID", "SYS_CHROOT"}
	baselineSysctls      = []string{"kernel.shm_rmid_forced", "net.ipv4.ip_local_port_range", "net.ipv4.ip_unprivileged_port_start", "net.ipv4.tcp_syncookies", "net.ipv4.ping_group_range",
		"net.ipv4.ip_local_reserved_ports", "net.ipv4.tcp_keepalive_time", "net.ipv4.tcp_fin_timeout", "net.ipv4.tcp_keepalive_intvl", "net.ipv4.tcp_keepalive_probes"}
	baselineSELinuxTypes = []string{"", "container_t", "container_init_t", "container_kvm_t", "container_engine_t"}
)

const (
	appArmorAnnotation = "container.apparmor.security.beta.kubernetes.io/"
	// Legacy seccomp annotations: ignored since Kubernetes 1.27, still refused at unconfined.
	seccompPodAnnotation       = "seccomp.security.alpha.kubernetes.io/pod"
	seccompContainerAnnotation = "container.seccomp.security.alpha.kubernetes.io/"
)

// baselineViolation is the first Pod Security baseline control pod breaks, as a fixed detail
// word, or "" when it meets baseline. Every container, init and ephemeral included, is checked.
func baselineViolation(pod *corev1.Pod) string {
	s := pod.Spec
	if s.HostNetwork || s.HostPID || s.HostIPC {
		return "host_namespace"
	}
	if slices.ContainsFunc(s.Volumes, func(v corev1.Volume) bool { return v.HostPath != nil }) {
		return "host_path"
	}
	for k, v := range pod.Annotations {
		if strings.HasPrefix(k, appArmorAnnotation) && v != "" && v != "runtime/default" && !strings.HasPrefix(v, "localhost/") {
			return "apparmor"
		}
		if (k == seccompPodAnnotation || strings.HasPrefix(k, seccompContainerAnnotation)) && v == "unconfined" {
			return "seccomp"
		}
	}
	if p := s.SecurityContext; p != nil {
		if slices.ContainsFunc(p.Sysctls, func(c corev1.Sysctl) bool { return !slices.Contains(baselineSysctls, c.Name) }) {
			return "sysctl"
		}
		if v := securityViolation(p.SeccompProfile, p.AppArmorProfile, p.SELinuxOptions, p.WindowsOptions); v != "" {
			return v
		}
	}
	containers := slices.Concat(s.InitContainers, s.Containers)
	for _, e := range s.EphemeralContainers {
		containers = append(containers, corev1.Container(e.EphemeralContainerCommon))
	}
	for _, c := range containers {
		if slices.ContainsFunc(c.Ports, func(p corev1.ContainerPort) bool { return p.HostPort != 0 }) {
			return "host_port"
		}
		sc := c.SecurityContext
		switch {
		case sc == nil:
			continue
		case sc.Privileged != nil && *sc.Privileged:
			return "privileged"
		case sc.Capabilities != nil && slices.ContainsFunc(sc.Capabilities.Add, func(c corev1.Capability) bool { return !slices.Contains(baselineCapabilities, c) }):
			return "capabilities"
		case sc.ProcMount != nil && *sc.ProcMount != corev1.DefaultProcMount:
			return "proc_mount"
		}
		if v := securityViolation(sc.SeccompProfile, sc.AppArmorProfile, sc.SELinuxOptions, sc.WindowsOptions); v != "" {
			return v
		}
	}
	return ""
}

// securityViolation checks the controls a pod and a container security context share.
func securityViolation(seccomp *corev1.SeccompProfile, apparmor *corev1.AppArmorProfile, selinux *corev1.SELinuxOptions, windows *corev1.WindowsSecurityContextOptions) string {
	switch {
	case seccomp != nil && seccomp.Type == corev1.SeccompProfileTypeUnconfined:
		return "seccomp"
	case apparmor != nil && apparmor.Type == corev1.AppArmorProfileTypeUnconfined:
		return "apparmor"
	case selinux != nil && (selinux.User != "" || selinux.Role != "" || !slices.Contains(baselineSELinuxTypes, selinux.Type)):
		return "selinux"
	case windows != nil && windows.HostProcess != nil && *windows.HostProcess:
		return "host_process"
	}
	return ""
}

func ptrTo(v int) *int { return &v }

func hasContainer(pod *corev1.Pod, name string) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return true
		}
	}
	return false
}

// finish records how the stream ended, then ends stdout. nil is exit 0 and an ExitError its
// code, unless the session cancelled the stream itself (Close, idle, lifetime): then the exit is
// unknown.
func (s *podExec) finish(err error) {
	var exit exec.ExitError
	var code *int
	if s.ctx.Err() == nil {
		if err == nil {
			code = ptrTo(0)
		} else if errors.As(err, &exit) {
			code = ptrTo(exit.ExitStatus())
		}
	}
	s.mu.Lock()
	s.ended, s.exit = true, code
	s.mu.Unlock()
	if err != nil && code == nil {
		s.stdoutW.CloseWithError(errExecStream)
	} else {
		s.stdoutW.Close()
	}
	s.stdinR.CloseWithError(errExecEnded)
}

// Next feeds Resize into the executor; nil stops its resize loop.
func (s *podExec) Next() *remotecommand.TerminalSize {
	select {
	case size := <-s.sizes:
		return &size
	case <-s.ctx.Done():
		return nil
	}
}

func (s *podExec) Read(p []byte) (int, error) {
	if err := context.Cause(s.ctx); err != nil {
		return 0, err
	}
	if len(p) > protocol.MaxExecChunkBytes {
		p = p[:protocol.MaxExecChunkBytes]
	}
	n, err := s.stdoutR.Read(p)
	if cause := context.Cause(s.ctx); cause != nil {
		n, err = 0, cause
	}
	if err != nil {
		s.Close()
	}
	return n, err
}

func (s *podExec) Write(p []byte) (int, error) {
	if len(p) > protocol.MaxExecChunkBytes {
		return 0, errors.New("exec input chunk exceeds limit")
	}
	if err := context.Cause(s.ctx); err != nil {
		return 0, err
	}
	n, err := s.stdinW.Write(p)
	if n > 0 {
		s.touch()
	}
	if err != nil {
		cause := context.Cause(s.ctx)
		s.Close()
		if cause != nil {
			err = cause
		}
	}
	return n, err
}

// touch resets the idle deadline on operator input; output alone does not.
func (s *podExec) touch() {
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	if s.ctx.Err() == nil {
		s.idle.Reset(s.idleBudget)
	}
}

func (s *podExec) closeWith(reason error) {
	s.once.Do(func() {
		s.cancel(reason)
		s.stopLifetime()
		s.idleMu.Lock()
		s.idle.Stop()
		s.idleMu.Unlock()
		s.stdinR.CloseWithError(reason)
		s.stdoutR.CloseWithError(reason)
	})
}

func (s *podExec) Close() error { s.closeWith(ErrExecClosed); return nil }

// Resize queues the newest size, replacing one the executor has not taken yet.
func (s *podExec) Resize(_ context.Context, size protocol.TerminalSize) error {
	if err := size.Validate(); err != nil {
		return err
	}
	if err := context.Cause(s.ctx); err != nil {
		return err
	}
	next := remotecommand.TerminalSize{Width: uint16(size.Columns), Height: uint16(size.Rows)}
	select {
	case s.sizes <- next:
	default:
		select {
		case <-s.sizes:
		default:
		}
		select {
		case s.sizes <- next:
		default:
		}
	}
	s.touch()
	return nil
}

// Inspect reports Running until the stream returns, then the exit code when one was received.
func (s *podExec) Inspect(context.Context) (protocol.ExecStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return protocol.ExecStatus{Running: !s.ended, ExitCode: s.exit}, nil
}
