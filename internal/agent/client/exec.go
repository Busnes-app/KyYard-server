package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// ExecSession is the runtime adapter boundary. Close must unblock Read and Write;
// it closes the attachment, without promising that the process has been killed.
// Implementations must permit concurrent Close calls and enforce the protocol
// idle/absolute deadlines; the transport bounds queue waits separately.
type ExecSession interface {
	io.ReadWriteCloser
	Resize(context.Context, protocol.TerminalSize) error
	Inspect(context.Context) (protocol.ExecStatus, error)
}

// Admission belongs to Run, not the socket: an adapter slow to cancel keeps its
// slot across reconnects until all of its workers have actually finished.
type execBudget struct {
	mu     sync.Mutex
	actors map[string]bool
}

func (b *execBudget) take(actor string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.actors == nil {
		b.actors = make(map[string]bool)
	}
	if b.actors[actor] || len(b.actors) >= protocol.MaxExecStreamsPerEndpoint {
		return false
	}
	b.actors[actor] = true
	return true
}
func (b *execBudget) release(actor string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.actors, actor)
}

type execInput struct {
	data []byte
	size *protocol.TerminalSize
}
type execStream struct {
	input chan execInput
	stop  context.CancelCauseFunc
}

type execStreams struct {
	mu   sync.Mutex
	live map[string]*execStream
	// Consumed grants remain until expiry, even after their worker ends. At the
	// fixed ceiling new opens are refused, rather than evicting replay protection.
	seen     map[string]time.Time
	budget   *execBudget
	endpoint string
	nonce    []byte
	ctx      context.Context
	opts     *Options
	out      chan<- outFrame
}

func newExecStreams(ctx context.Context, endpoint string, nonce []byte, b *execBudget, opts *Options, out chan<- outFrame) *execStreams {
	return &execStreams{live: make(map[string]*execStream), seen: make(map[string]time.Time), budget: b, endpoint: endpoint, nonce: nonce, ctx: ctx, opts: opts, out: out}
}

var errExecProtocol = errors.New("invalid exec frame")
var errExecCapacity = errors.New("exec stream limit reached")
var errExecUnavailable = errors.New("this agent has no exec runtime; not started")

// handle runs on the socket loop. Runtime I/O runs only in workers; full input
// queues cancel the attachment instead of delaying heartbeat or revocation frames.
func (s *execStreams) handle(f protocol.Envelope, active bool) error {
	if len(f.Payload) > protocol.MaxExecFrameBytes {
		return errExecProtocol
	}
	switch f.Type {
	case protocol.TypeExecOpen:
		var req protocol.ExecOpen
		if json.Unmarshal(f.Payload, &req) != nil || req.Validate(time.Now()) != nil || req.Endpoint != s.endpoint || !bytes.Equal(req.Connection, s.nonce) {
			return errExecProtocol
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		now := time.Now()
		for id, expiry := range s.seen {
			if !expiry.After(now) {
				delete(s.seen, id)
			}
		}
		if _, used := s.seen[req.Stream]; used || s.live[req.Stream] != nil {
			return errExecProtocol
		}
		if !active {
			return errExecProtocol
		}
		if len(s.seen) >= 1024 {
			return errExecCapacity
		}
		s.seen[req.Stream] = req.Expires
		if s.opts.Exec == nil {
			return errExecUnavailable
		}
		if !s.budget.take(req.Actor) {
			return errExecCapacity
		}
		ctx, stop := context.WithCancelCause(s.ctx)
		stream := &execStream{input: make(chan execInput, 8), stop: stop}
		s.live[req.Stream] = stream
		go s.run(ctx, req, stream)
	case protocol.TypeExecInput, protocol.TypeExecResize, protocol.TypeExecCancel:
		var id string
		var input execInput
		switch f.Type {
		case protocol.TypeExecInput:
			var v protocol.ExecData
			if json.Unmarshal(f.Payload, &v) != nil || len(v.Data) == 0 || len(v.Data) > protocol.MaxExecChunkBytes {
				return errExecProtocol
			}
			id, input.data = v.Stream, v.Data
		case protocol.TypeExecResize:
			var v protocol.ExecResize
			if json.Unmarshal(f.Payload, &v) != nil || v.Size.Validate() != nil {
				return errExecProtocol
			}
			id, input.size = v.Stream, &v.Size
		case protocol.TypeExecCancel:
			var v protocol.ExecStream
			if json.Unmarshal(f.Payload, &v) != nil {
				return errExecProtocol
			}
			id = v.Stream
		}
		s.mu.Lock()
		stream := s.live[id]
		s.mu.Unlock()
		if stream == nil {
			return nil
		} // late input/cancel after EOF
		if f.Type == protocol.TypeExecCancel {
			stream.stop(errors.New("attachment cancelled; process exit unknown"))
			return nil
		}
		select {
		case stream.input <- input:
		default:
			stream.stop(errors.New("terminal input queue full; process exit unknown"))
		}
	}
	return nil
}

func (s *execStreams) run(ctx context.Context, req protocol.ExecOpen, stream *execStream) {
	defer func() {
		stream.stop(context.Canceled)
		s.mu.Lock()
		delete(s.live, req.Stream)
		s.mu.Unlock()
		s.budget.release(req.Actor)
	}()
	closed := protocol.ExecClose{Stream: req.Stream, Reason: "exec start failed; process exit unknown"}
	// Close follows every queued output chunk on the same FIFO. If the peer stops
	// draining, these (at most four) workers keep their slots until disconnect.
	defer func() { deliverFrame(s.ctx, s.out, outFrame{protocol.TypeExecClose, closed}) }()
	if ctx.Err() != nil || !req.Expires.After(time.Now()) {
		closed.Reason = "exec grant expired or cancelled before start"
		return
	}
	runtime, err := s.opts.Exec(ctx, req.Spec)
	if err != nil {
		return
	} // daemon errors can contain argv; never forward them
	defer runtime.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = runtime.Close() })
	defer stopClose()
	if ctx.Err() != nil {
		closed.Reason = "attachment cancelled; process exit unknown"
		return
	}
	if runtime.Resize(ctx, req.Size) != nil {
		closed.Reason = "terminal resize failed; process exit unknown"
		return
	}
	if !s.send(ctx, outFrame{protocol.TypeExecReady, protocol.ExecStream{Stream: req.Stream}}) {
		closed.Reason = "attachment cancelled; process exit unknown"
		return
	}
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		for {
			select {
			case <-ctx.Done():
				return
			case v := <-stream.input:
				if ctx.Err() != nil {
					return
				}
				var err error
				if v.size != nil {
					err = runtime.Resize(ctx, *v.size)
				} else {
					var n int
					n, err = runtime.Write(v.data)
					if err == nil && n != len(v.data) {
						err = io.ErrShortWrite
					}
				}
				if err != nil {
					stream.stop(errors.New("terminal input failed; process exit unknown"))
					return
				}
			}
		}
	}()
	// Join input before releasing admission, even if the adapter ignores cancellation.
	defer func() { stream.stop(context.Canceled); _ = runtime.Close(); <-inputDone }()
	buf := make([]byte, protocol.MaxExecChunkBytes)
	for {
		n, err := runtime.Read(buf)
		if n > 0 && !s.send(ctx, outFrame{protocol.TypeExecOutput, protocol.ExecData{Stream: req.Stream, Data: bytes.Clone(buf[:n])}}) {
			break
		}
		if err != nil {
			if errors.Is(err, io.EOF) && ctx.Err() == nil {
				inspectCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
				status, inspectErr := runtime.Inspect(inspectCtx)
				cancel()
				if inspectErr == nil && ctx.Err() == nil && !status.Running && status.ExitCode != nil {
					closed.Reason, closed.ExitCode = "process exited", status.ExitCode
					return
				}
			}
			break
		}
	}
	closed.Reason = "attachment closed; process exit unknown"
	if cause := context.Cause(ctx); cause != nil && cause != context.Canceled {
		closed.Reason = cause.Error()
	}
}

func (s *execStreams) send(ctx context.Context, f outFrame) bool {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	case s.out <- f:
		return true
	}
}
