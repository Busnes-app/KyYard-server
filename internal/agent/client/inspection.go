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
}

func newInspections(ctx context.Context, endpoint string, nonce []byte, opts *Options, out chan<- outFrame) *inspections {
	return &inspections{live: map[string]context.CancelFunc{}, seen: map[string]time.Time{}, slots: opts.inspectionSlots, ctx: ctx, endpoint: endpoint, nonce: nonce, opts: opts, out: out}
}
func (s *inspections) handle(f protocol.Envelope, active bool) error {
	invalid := errors.New("invalid inspection frame")
	if len(f.Payload) > protocol.MaxInspectionFrameBytes {
		return invalid
	}
	if f.Type == protocol.TypeInspectionCancel {
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
	if json.Unmarshal(f.Payload, &req) != nil || req.Validate(time.Now()) != nil || !active || req.Endpoint != s.endpoint || !bytes.Equal(req.Connection, s.nonce) {
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
	if s.opts.Inspect == nil {
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
		case s.out <- outFrame{protocol.TypeInspectionResult, protocol.InspectionResult{Request: req.Request, Status: status}}:
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
	result, err := s.opts.Inspect(ctx, req.Target)
	reply := protocol.InspectionResult{Request: req.Request, Status: "unavailable"}
	if err == nil && result != nil && result.Validate(req.Target, time.Now(), true) == nil {
		reply.Status = "ok"
		reply.Result = result
	}
	if ctx.Err() != nil {
		return
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case s.out <- outFrame{protocol.TypeInspectionResult, reply}:
	case <-ctx.Done():
	case <-timer.C:
	}
}
