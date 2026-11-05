package client

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// Virtual time crosses all three retention windows without a real-time sleep or
// a production clock hook. A previously valid, consumed grant stays a replay.
func TestConsumedGrantSurvivesExpiryWindow(t *testing.T) {
	for _, family := range []string{"exec", "inspection", "configuration"} {
		t.Run(family, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				out := make(chan outFrame, 8)
				if family == "exec" {
					req := execRequest()
					s := newExecStreams(context.Background(), req.Endpoint, req.Connection, &execBudget{}, &Options{}, out)
					if err := s.handle(execFrame(protocol.TypeExecOpen, req), true); !errors.Is(err, errExecUnavailable) {
						t.Fatal(err)
					}
					time.Sleep(2 * time.Minute)
					if err := s.handle(execFrame(protocol.TypeExecOpen, req), true); !errors.Is(err, errExecProtocol) {
						t.Fatal("delayed consumed exec replay was treated as expiry-only", err)
					}
					return
				}
				req := inspectionRequest()
				req.Expires = time.Now().Add(10 * time.Second)
				opts := &Options{}
				s := newInspections(context.Background(), req.Endpoint, req.Connection, opts, out)
				typ := protocol.TypeInspectionOpen
				if family == "configuration" {
					s = newConfigurations(context.Background(), req.Endpoint, req.Connection, opts, out)
					typ = protocol.TypeConfigurationOpen
				}
				if err := s.handle(execFrame(typ, req), true); err != nil {
					t.Fatal(err)
				}
				<-out
				time.Sleep(2 * time.Minute)
				if err := s.handle(execFrame(typ, req), true); err == nil {
					t.Fatal("delayed consumed replay was treated as expiry-only")
				}
				if len(out) != 0 {
					t.Fatal("delayed replay received a refusal instead of ending the session")
				}
			})
		})
	}
}
