package client

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/coder/websocket"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func inspectionRequest() protocol.InspectionOpen {
	return protocol.InspectionOpen{Request: "request", Endpoint: "endpoint", Actor: "actor", Connection: make([]byte, 32), Expires: time.Now().Add(24 * time.Second), Target: protocol.InspectionTarget{ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}}
}
func TestInspectionTransportRedactionAndGrantChecks(t *testing.T) {
	req := inspectionRequest()
	out := make(chan outFrame, 8)
	opts := &Options{inspectionSlots: make(chan struct{}, 2), Inspect: func(context.Context, protocol.InspectionTarget) (*protocol.ContainerInspection, error) {
		return nil, errors.New("secret-canary")
	}}
	s := newInspections(context.Background(), req.Endpoint, req.Connection, opts, out)
	if err := s.handle(execFrame(protocol.TypeInspectionOpen, req), true); err != nil {
		t.Fatal(err)
	}
	reply := nextExecFrame(t, out, protocol.TypeInspectionResult).Payload.(protocol.InspectionResult)
	if reply.Status != "unavailable" || reply.Result != nil {
		t.Fatal("runtime error exposed")
	}
	if err := s.handle(execFrame(protocol.TypeInspectionOpen, req), true); err == nil {
		t.Fatal("replay accepted")
	}
	for _, mutate := range []func(*protocol.InspectionOpen){func(r *protocol.InspectionOpen) { r.Endpoint = "other" }, func(r *protocol.InspectionOpen) { r.Connection = []byte("wrong") }, func(r *protocol.InspectionOpen) { r.Expires = time.Now().Add(-time.Second) }, func(r *protocol.InspectionOpen) { r.Target.ContainerID = "name" }} {
		bad := inspectionRequest()
		mutate(&bad)
		s := newInspections(context.Background(), req.Endpoint, req.Connection, opts, out)
		if err := s.handle(execFrame(protocol.TypeInspectionOpen, bad), true); err == nil {
			t.Fatal("bad grant accepted")
		}
	}
	s = newInspections(context.Background(), req.Endpoint, req.Connection, opts, out)
	if err := s.handle(execFrame(protocol.TypeInspectionOpen, req), false); err == nil {
		t.Fatal("pending agent read runtime")
	}
}
func TestInspectionTransportCancellationKeepsAdmissionAcrossReconnect(t *testing.T) {
	req := inspectionRequest()
	out := make(chan outFrame, 8)
	started := make(chan struct{}, 2)
	cancelled := make(chan struct{}, 2)
	release := make(chan struct{})
	opts := &Options{inspectionSlots: make(chan struct{}, 2), Inspect: func(ctx context.Context, _ protocol.InspectionTarget) (*protocol.ContainerInspection, error) {
		started <- struct{}{}
		<-ctx.Done()
		cancelled <- struct{}{}
		<-release
		return nil, ctx.Err()
	}}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	defer close(release)
	s := newInspections(ctx, req.Endpoint, req.Connection, opts, out)
	for _, id := range []string{"a", "b"} {
		req.Request = id
		if err := s.handle(execFrame(protocol.TypeInspectionOpen, req), true); err != nil {
			t.Fatal(err)
		}
		<-started
	}
	if err := s.handle(execFrame(protocol.TypeInspectionCancel, protocol.InspectionCancel{Request: "a"}), true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancel not delivered")
	}
	stop()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("disconnect not delivered")
	}
	successor := newInspections(context.Background(), req.Endpoint, req.Connection, opts, out)
	req.Request = "c"
	if err := successor.handle(execFrame(protocol.TypeInspectionOpen, req), true); err != nil {
		t.Fatal(err)
	}
	if reply := nextExecFrame(t, out, protocol.TypeInspectionResult).Payload.(protocol.InspectionResult); reply.Status != "busy" {
		t.Fatal("reconnect bypassed worker bound")
	}
}

func TestInspectionTransportKeepsHeartbeatsResponsive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	started, stopped := make(chan struct{}), make(chan struct{})
	checked := make(chan error, 1)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			checked <- err
			return
		}
		defer conn.CloseNow()
		if err = write(ctx, conn, protocol.TypeChallenge, protocol.Challenge{Nonce: make([]byte, 32), InstanceFingerprint: "instance", Versions: []int{1}}); err != nil {
			checked <- err
			return
		}
		if _, err = read(ctx, conn); err != nil {
			checked <- err
			return
		}
		if err = write(ctx, conn, protocol.TypeHello, protocol.Hello{State: "approved", HeartbeatSeconds: 1}); err != nil {
			checked <- err
			return
		}
		req := inspectionRequest()
		if err = write(ctx, conn, protocol.TypeInspectionOpen, req); err != nil {
			checked <- err
			return
		}
		select {
		case <-started:
		case <-ctx.Done():
			checked <- ctx.Err()
			return
		}
		advertised := false
		for beats := 0; beats < 2; {
			frame, err := read(ctx, conn)
			if err != nil {
				checked <- err
				return
			}
			switch frame.Type {
			case protocol.TypeHello:
				var hello protocol.Hello
				json.Unmarshal(frame.Payload, &hello)
				advertised = slices.Contains(hello.Capabilities, "container.inspect")
			case protocol.TypeHeartbeat:
				beats++
				if err = write(ctx, conn, protocol.TypeHeartbeat, nil); err != nil {
					checked <- err
					return
				}
			}
		}
		if !advertised {
			checked <- errors.New("inspection capability not advertised")
			return
		}
		if err = write(ctx, conn, protocol.TypeInspectionCancel, protocol.InspectionCancel{Request: req.Request}); err != nil {
			checked <- err
			return
		}
		select {
		case <-stopped:
			checked <- nil
		case <-ctx.Done():
			checked <- ctx.Err()
		}
	}))
	defer stub.Close()
	_, key, _ := ed25519.GenerateKey(nil)
	id := &Identity{EndpointID: "endpoint", PrivateKey: key, InstanceFingerprint: "instance", Server: stub.URL}
	done := make(chan error, 1)
	opts := Options{HTTPClient: stub.Client(), IdentityDir: t.TempDir(), Log: log.New(io.Discard, "", 0), Inspect: func(ctx context.Context, _ protocol.InspectionTarget) (*protocol.ContainerInspection, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}}
	go func() { done <- Run(ctx, id, opts) }()
	select {
	case err := <-checked:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("agent survived cancellation")
	}
}
