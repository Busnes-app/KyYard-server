package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

var (
	ErrExecClosed       = errors.New("exec stream closed; process exit is not established")
	ErrExecIdle         = errors.New("exec input idle timeout")
	ErrExecLifetime     = errors.New("exec absolute timeout")
	ErrExecStartUnknown = errors.New("exec start outcome is unknown; do not automatically retry")
)

// ExecSession is one raw PTY stream (stdout/stderr combined). One reader and one
// writer may run concurrently; Close unblocks both. No output or command is logged.
// Closing an attachment is NOT a promise to terminate the container process.
type ExecSession struct {
	client        *Client
	id, container string
	rw            io.ReadWriteCloser
	ctx           context.Context
	cancel        context.CancelCauseFunc
	stopLifetime  context.CancelFunc
	once          sync.Once
	idleMu        sync.Mutex
	idle          *time.Timer
	idleBudget    time.Duration
}

// OpenExec creates and starts one PTY against an immutable, running container and
// image identity. Callers must authorize/admit the session before calling this; this
// method is not exposed by any HTTP or agent frame handler yet. See protocol §7.
func (c *Client) OpenExec(ctx context.Context, spec protocol.ExecSpec) (*ExecSession, error) {
	return c.openExec(ctx, spec, protocol.ExecIdleTimeout, protocol.ExecAbsoluteTimeout)
}
func (c *Client) openExec(parent context.Context, spec protocol.ExecSpec, idle, lifetime time.Duration) (*ExecSession, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	lifetimeCtx, stopLifetime := context.WithTimeoutCause(parent, lifetime, ErrExecLifetime)
	ctx, cancel := context.WithCancelCause(lifetimeCtx)
	// The opening handshake has its own short budget; it must not inherit eight hours.
	setup := time.AfterFunc(callBudget, func() { cancel(context.DeadlineExceeded) })
	owned := false
	defer func() {
		setup.Stop()
		if !owned {
			cancel(ErrExecClosed)
			stopLifetime()
		}
	}()
	var inspected struct {
		ID    string `json:"Id"`
		Image string
		State struct{ Running, Paused, Restarting bool }
	}
	if err := c.get(ctx, "/containers/"+spec.Container+"/json", &inspected); err != nil {
		return nil, err
	}
	if inspected.ID != spec.Container || inspected.Image != spec.ImageID || !inspected.State.Running || inspected.State.Paused || inspected.State.Restarting {
		return nil, errors.New("exec target identity or running state changed")
	}
	body := struct {
		AttachStdin, AttachStdout, AttachStderr, Tty, Privileged bool
		User                                                     string
		Cmd, Env                                                 []string
	}{true, true, true, true, false, spec.User, spec.Argv, []string{"TERM=xterm-256color"}}
	response, err := c.execPost(ctx, "/containers/"+spec.Container+"/exec", body, http.StatusCreated)
	if err != nil {
		return nil, err
	}
	var created struct {
		ID string `json:"Id"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&created)
	response.Body.Close()
	if err != nil || !protocol.ValidExecID(created.ID) {
		return nil, errors.New("runtime returned an invalid exec identity")
	}
	response, err = c.execPost(ctx, "/exec/"+created.ID+"/start", struct{ Detach, Tty bool }{false, true}, http.StatusSwitchingProtocols)
	if err != nil {
		return nil, errors.Join(ErrExecStartUnknown, err)
	}
	rw, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		response.Body.Close()
		return nil, errors.Join(ErrExecStartUnknown, errors.New("runtime did not provide duplex exec I/O"))
	}
	s := &ExecSession{client: c, id: created.ID, container: spec.Container, rw: rw, ctx: ctx, cancel: cancel, stopLifetime: stopLifetime, idleBudget: idle}
	s.idle = time.NewTimer(idle)
	owned = true
	// Upgraded response bodies are outside ordinary HTTP cancellation handling.
	go func() {
		select {
		case <-ctx.Done():
			s.closeWith(context.Cause(ctx))
		case <-s.idle.C:
			s.closeWith(ErrExecIdle)
		}
	}()
	if ctx.Err() != nil {
		s.Close()
		return nil, errors.Join(ErrExecStartUnknown, context.Cause(ctx))
	}
	return s, nil
}

func (c *Client) execPost(ctx context.Context, path string, body any, want int) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if want == http.StatusSwitchingProtocols {
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "tcp")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	// Docker documents resize as 201, while current Engines return 200.
	if resp.StatusCode != want && !(want == http.StatusOK && resp.StatusCode == http.StatusCreated) {
		resp.Body.Close()
		return nil, &statusError{path: path, status: resp.StatusCode}
	}
	return resp, nil
}
func (s *ExecSession) Read(p []byte) (int, error) {
	if err := context.Cause(s.ctx); err != nil {
		return 0, err
	}
	if len(p) > protocol.MaxExecChunkBytes {
		p = p[:protocol.MaxExecChunkBytes]
	}
	n, err := s.rw.Read(p)
	if cause := context.Cause(s.ctx); cause != nil {
		n, err = 0, cause
	}
	if err != nil {
		s.Close()
	}
	return n, err
}
func (s *ExecSession) Write(p []byte) (int, error) {
	if len(p) > protocol.MaxExecChunkBytes {
		return 0, errors.New("exec input chunk exceeds limit")
	}
	if err := context.Cause(s.ctx); err != nil {
		return 0, err
	}
	n, err := s.rw.Write(p)
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
func (s *ExecSession) touch() {
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	if s.ctx.Err() == nil {
		s.idle.Reset(s.idleBudget)
	}
}
func (s *ExecSession) closeWith(reason error) {
	s.once.Do(func() {
		s.cancel(reason)
		s.stopLifetime()
		s.idleMu.Lock()
		s.idle.Stop()
		s.idleMu.Unlock()
		s.rw.Close()
	})
}
func (s *ExecSession) Close() error { s.closeWith(ErrExecClosed); return nil }

// Resize counts as operator activity. Runtime output alone does not reset idle time.
func (s *ExecSession) Resize(ctx context.Context, size protocol.TerminalSize) error {
	if err := size.Validate(); err != nil {
		return err
	}
	if err := context.Cause(s.ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	q := url.Values{"h": {strconv.Itoa(size.Rows)}, "w": {strconv.Itoa(size.Columns)}}
	resp, err := s.client.execPost(ctx, "/exec/"+s.id+"/resize?"+q.Encode(), nil, http.StatusOK)
	if err != nil {
		return err
	}
	resp.Body.Close()
	s.touch()
	return nil
}

// Inspect can run after Close with a fresh context. EOF alone is not an exit code.
func (s *ExecSession) Inspect(ctx context.Context) (protocol.ExecStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, callBudget)
	defer cancel()
	var result struct {
		ID, ContainerID string
		Running         *bool
		ExitCode        *int
	}
	if err := s.client.get(ctx, "/exec/"+s.id+"/json", &result); err != nil {
		return protocol.ExecStatus{}, err
	}
	if result.ID != s.id || result.ContainerID != s.container || result.Running == nil || (!*result.Running && result.ExitCode == nil) {
		return protocol.ExecStatus{}, errors.New("runtime returned incomplete or mismatched exec status")
	}
	status := protocol.ExecStatus{Running: *result.Running}
	if !status.Running {
		status.ExitCode = result.ExitCode
	}
	return status, nil
}
