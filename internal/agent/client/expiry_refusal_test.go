package client

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/coder/websocket"
)

// Exercise the authenticated session, including a subsequent useful request and
// an expired replay. A refusal alone must not hide a lost socket or replay cache.
func TestExpiryOnlyGrantKeepsSession(t *testing.T) {
	for _, family := range []string{"exec", "inspection", "configuration"} {
		for _, timing := range []string{"past", "future"} {
			t.Run(family+"/"+timing, func(t *testing.T) {
				exerciseExpiryGrant(t, family, timing, false)
			})
		}
		t.Run(family+"/wrong-scope-and-expired", func(t *testing.T) {
			exerciseExpiryGrant(t, family, "past", true)
		})
	}
}

func exerciseExpiryGrant(t *testing.T, family, timing string, wrongScope bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	checked := make(chan error, 1)
	var calls atomic.Int32
	reqType, resultType := protocol.TypeInspectionOpen, protocol.TypeInspectionResult
	lifetime := protocol.InspectionLifetime
	if family == "exec" {
		reqType, resultType, lifetime = protocol.TypeExecOpen, protocol.TypeExecClose, protocol.ExecGrantLifetime
	} else if family == "configuration" {
		reqType, resultType, lifetime = protocol.TypeConfigurationOpen, protocol.TypeConfigurationResult, protocol.ConfigurationLifetime
	}
	request := func(id string, expires time.Time) any {
		endpoint := "endpoint"
		if wrongScope {
			endpoint = "other"
		}
		if family == "exec" {
			req := execRequest()
			req.Stream, req.Expires, req.Endpoint = id, expires, endpoint
			return req
		}
		req := inspectionRequest()
		req.Request, req.Expires, req.Endpoint = id, expires, endpoint
		return req
	}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := func() error {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return err
			}
			defer conn.CloseNow()
			if err := write(ctx, conn, protocol.TypeChallenge, protocol.SignChallenge(testInstance, make([]byte, 32), agentNonce(r), r.Host)); err != nil {
				return err
			}
			if _, err := read(ctx, conn); err != nil {
				return err
			}
			if err := write(ctx, conn, protocol.TypeHello, protocol.Hello{State: "approved", HeartbeatSeconds: 1}); err != nil {
				return err
			}
			readResult := func() (protocol.Envelope, error) {
				for {
					frame, err := read(ctx, conn)
					if err != nil {
						return frame, err
					}
					if frame.Type == resultType {
						return frame, nil
					}
					if frame.Type == protocol.TypeHeartbeat {
						if err := write(ctx, conn, protocol.TypeHeartbeat, nil); err != nil {
							return frame, err
						}
					}
				}
			}
			expires := time.Now().Add(-time.Second)
			if timing == "future" {
				expires = time.Now().Add(lifetime + time.Hour)
			}
			expired := request("expiry-only", expires)
			if err := write(ctx, conn, reqType, expired); err != nil {
				return err
			}
			frame, err := readResult()
			if wrongScope {
				if err == nil {
					return errors.New("wrong scope was answered instead of ending the socket")
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if calls.Load() != 0 {
					return errors.New("wrong scope reached runtime")
				}
				return nil
			}
			if err != nil {
				return fmt.Errorf("expiry-only grant dropped socket: %w", err)
			}
			if family == "exec" {
				var result protocol.ExecClose
				if err := json.Unmarshal(frame.Payload, &result); err != nil {
					return err
				}
				if result.Stream != "expiry-only" || result.ExitCode != nil {
					return fmt.Errorf("invalid refusal: %+v", result)
				}
			} else {
				var result struct {
					Request string `json:"request"`
					Status  string `json:"status"`
				}
				if err := json.Unmarshal(frame.Payload, &result); err != nil {
					return err
				}
				if result.Request != "expiry-only" || result.Status != "unavailable" {
					return fmt.Errorf("invalid refusal: %+v", result)
				}
			}
			if calls.Load() != 0 {
				return errors.New("refused expiry reached runtime")
			}
			if err := write(ctx, conn, reqType, request("following", time.Now().Add(lifetime/2))); err != nil {
				return err
			}
			if _, err := readResult(); err != nil {
				return fmt.Errorf("fresh grant lost with expired predecessor: %w", err)
			}
			if calls.Load() != 1 {
				return fmt.Errorf("fresh request runtime calls = %d, want 1", calls.Load())
			}
			if err := write(ctx, conn, reqType, expired); err != nil {
				return err
			}
			if _, err := readResult(); err == nil {
				return errors.New("expired replay was answered instead of ending the socket")
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if calls.Load() != 1 {
				return errors.New("expired replay reached runtime")
			}
			return nil
		}()
		select {
		case checked <- err:
		case <-ctx.Done():
		}
	}))
	defer stub.Close()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	id := &Identity{EndpointID: "endpoint", PrivateKey: key, InstanceFingerprint: testInstanceFingerprint, Server: stub.URL}
	opts := Options{HTTPClient: stub.Client(), IdentityDir: t.TempDir(), Log: log.New(io.Discard, "", 0),
		Exec: func(context.Context, protocol.ExecSpec) (ExecSession, error) {
			calls.Add(1)
			return nil, errors.New("private runtime detail")
		},
		Inspect: func(context.Context, protocol.InspectionTarget) (*protocol.ContainerInspection, error) {
			calls.Add(1)
			return nil, errors.New("private runtime detail")
		},
		Configure: func(context.Context, protocol.InspectionTarget) (*protocol.ContainerConfiguration, error) {
			calls.Add(1)
			return nil, errors.New("private runtime detail")
		},
	}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, id, opts) }()
	select {
	case err := <-checked:
		cancel()
		if err != nil {
			t.Error(err)
		}
	case <-ctx.Done():
		t.Error("session check timed out", ctx.Err())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("agent did not stop")
	}
}

func TestInspectionConnectionHistoryIsBounded(t *testing.T) {
	for _, configuration := range []bool{false, true} {
		t.Run(fmt.Sprint(configuration), func(t *testing.T) {
			req := inspectionRequest()
			req.Expires = time.Now().Add(10 * time.Second)
			out := make(chan outFrame, 8)
			opts := &Options{}
			s := newInspections(context.Background(), req.Endpoint, req.Connection, opts, out)
			typ := protocol.TypeInspectionOpen
			if configuration {
				s = newConfigurations(context.Background(), req.Endpoint, req.Connection, opts, out)
				typ = protocol.TypeConfigurationOpen
			}
			for i := 0; i < maxSessionGrantIDs; i++ {
				s.seen[fmt.Sprint(i)] = struct{}{}
			}
			if err := s.handle(execFrame(typ, req), true); err == nil || len(s.seen) != maxSessionGrantIDs || len(out) != 0 {
				t.Fatal("full connection history must end without eviction or runtime admission", err)
			}
		})
	}
}
