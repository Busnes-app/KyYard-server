package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// Slots belong to Run, not a connection: slow cancellation cannot gain new
// runtime workers through reconnect. Results live only in this session.
type inspections struct {
	mu       sync.Mutex
	live     map[string]context.CancelFunc
	seen     map[string]time.Time
	slots    chan struct{}
	ctx      context.Context
	endpoint string
	nonce    []byte
	opts     *Options
	out      chan<- outFrame
	frames   frameFamily
}

// frameFamily is what differs between the inspection and configuration handlers: frame names,
// size and grant bounds, whether the runtime call exists, and how an answer is built.
type frameFamily struct {
	cancel, result string
	maxBytes       int
	lifetime       time.Duration
	ready          bool
	reply          func(ctx context.Context, req protocol.InspectionOpen) any
	refusal        func(request, status string) any
}

func newInspections(ctx context.Context, endpoint string, nonce []byte, opts *Options, out chan<- outFrame) *inspections {
	s := &inspections{live: map[string]context.CancelFunc{}, seen: map[string]time.Time{}, slots: opts.inspectionSlots, ctx: ctx, endpoint: endpoint, nonce: nonce, opts: opts, out: out}
	s.frames = frameFamily{
		cancel: protocol.TypeInspectionCancel, result: protocol.TypeInspectionResult,
		maxBytes: protocol.MaxInspectionFrameBytes, lifetime: protocol.InspectionLifetime, ready: opts.Inspect != nil,
		refusal: func(request, status string) any { return protocol.InspectionResult{Request: request, Status: status} },
		reply: func(ctx context.Context, req protocol.InspectionOpen) any {
			reply := protocol.InspectionResult{Request: req.Request, Status: "unavailable"}
			if result, err := opts.Inspect(ctx, req.Target); err == nil && result != nil && result.Validate(req.Target, time.Now(), true) == nil {
				reply.Status = "ok"
				reply.Result = result
			}
			return reply
		},
	}
	return s
}

// newConfigurations serves configuration.open with its own one-slot admission and 20 s grants:
// a container's configuration on Docker, a workload's pod template on a cluster (an
// object-shaped target only; the UID shape is inspection's).
func newConfigurations(ctx context.Context, endpoint string, nonce []byte, opts *Options, out chan<- outFrame) *inspections {
	s := &inspections{live: map[string]context.CancelFunc{}, seen: map[string]time.Time{}, slots: opts.configurationSlots, ctx: ctx, endpoint: endpoint, nonce: nonce, opts: opts, out: out}
	ready := opts.Configure != nil
	if opts.Kubernetes {
		ready = opts.ReadWorkload != nil
	}
	s.frames = frameFamily{
		cancel: protocol.TypeConfigurationCancel, result: protocol.TypeConfigurationResult,
		maxBytes: protocol.MaxConfigurationFrameBytes, lifetime: protocol.ConfigurationLifetime, ready: ready,
		refusal: func(request, status string) any {
			return protocol.ConfigurationResult{Request: request, Status: status}
		},
		reply: func(ctx context.Context, req protocol.InspectionOpen) any {
			reply := protocol.ConfigurationResult{Request: req.Request, Status: "unavailable"}
			if opts.Kubernetes {
				if result, err := opts.ReadWorkload(ctx, req.Target); err == nil && result != nil && result.Validate(req.Target.Workload, time.Now()) == nil {
					reply.Status, reply.Workload = "ok", result
				}
				return reply
			}
			if result, err := opts.Configure(ctx, req.Target); err == nil && result != nil && result.Validate(req.Target, time.Now()) == nil {
				reply.Status = "ok"
				reply.Result = result
			}
			return reply
		},
	}
	return s
}

// runtime is the target shape this agent answers: a cluster agent reads Deployments.
func (s *inspections) runtime() string {
	if s.opts.Kubernetes {
		return protocol.RuntimeKubernetes
	}
	return protocol.RuntimeDocker
}
func (s *inspections) handle(f protocol.Envelope, active bool) error {
	invalid := errors.New("invalid inspection frame")
	if len(f.Payload) > s.frames.maxBytes {
		return invalid
	}
	if f.Type == s.frames.cancel {
		var req protocol.InspectionCancel
		if json.Unmarshal(f.Payload, &req) != nil || req.Request == "" {
			return invalid
		}
		s.mu.Lock()
		stop := s.live[req.Request]
		s.mu.Unlock()
		if stop != nil {
			stop()
		}
		return nil
	}
	var req protocol.InspectionOpen
	if json.Unmarshal(f.Payload, &req) != nil || req.ValidateWithin(time.Now(), s.frames.lifetime, s.runtime()) != nil || !active || req.Endpoint != s.endpoint || !bytes.Equal(req.Connection, s.nonce) {
		return invalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, expiry := range s.seen {
		if !expiry.After(time.Now()) {
			delete(s.seen, id)
		}
	}
	if _, seen := s.seen[req.Request]; seen || s.live[req.Request] != nil {
		return invalid
	}
	if len(s.seen) >= 1024 {
		return errors.New("inspection grant limit reached")
	}
	s.seen[req.Request] = req.Expires
	status := ""
	if !s.frames.ready {
		status = "unavailable"
	} else {
		select {
		case s.slots <- struct{}{}:
		default:
			status = "busy"
		}
	}
	if status != "" {
		// Refusals cannot block the heartbeat loop behind a slow connection.
		select {
		case s.out <- outFrame{s.frames.result, s.frames.refusal(req.Request, status)}:
		default:
			return errors.New("inspection result queue full")
		}
		return nil
	}
	ctx, stop := context.WithDeadline(s.ctx, req.Expires)
	s.live[req.Request] = stop
	go s.run(ctx, req, stop)
	return nil
}
func (s *inspections) run(ctx context.Context, req protocol.InspectionOpen, stop context.CancelFunc) {
	defer func() { stop(); s.mu.Lock(); delete(s.live, req.Request); s.mu.Unlock(); <-s.slots }()
	reply := s.frames.reply(ctx, req)
	// The server closes the socket on an oversized answer: one that does not fit is unavailable.
	if raw, err := json.Marshal(reply); err != nil || len(raw) > s.frames.maxBytes {
		reply = s.frames.refusal(req.Request, "unavailable")
	}
	if ctx.Err() != nil {
		return
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case s.out <- outFrame{s.frames.result, reply}:
	case <-ctx.Done():
	case <-timer.C:
	}
}
