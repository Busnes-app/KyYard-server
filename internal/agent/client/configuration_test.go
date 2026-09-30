package client

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/coder/websocket"
)

func configurationRequest(ttl time.Duration) protocol.ConfigurationOpen {
	r := inspectionRequest()
	r.Expires = time.Now().Add(ttl)
	return r
}

func minimalConfiguration(target protocol.InspectionTarget) *protocol.ContainerConfiguration {
	return &protocol.ContainerConfiguration{Target: target, ObservedAt: time.Now(), Name: "web", ImageID: target.ImageID, Image: protocol.ImagePull{Reference: "ghcr.io/acme/web:1", Digest: "sha256:" + strings.Repeat("c", 64), Tag: "ghcr.io/acme/web:1"}, NetworkMode: "bridge", Restart: "no", Env: []protocol.EnvEntry{}}
}

func newConfigurationsFor(req protocol.ConfigurationOpen, opts *Options, out chan outFrame) *inspections {
	opts.configurationSlots = make(chan struct{}, 1)
	return newConfigurations(context.Background(), req.Endpoint, req.Connection, opts, out)
}

func TestConfigurationAnswersOnlyValidGrants(t *testing.T) {
	out := make(chan outFrame, 8)
	opts := &Options{Configure: func(_ context.Context, target protocol.InspectionTarget) (*protocol.ContainerConfiguration, error) {
		return minimalConfiguration(target), nil
	}}
	req := configurationRequest(15 * time.Second)
	s := newConfigurationsFor(req, opts, out)
	if err := s.handle(execFrame(protocol.TypeConfigurationOpen, req), true); err != nil {
		t.Fatal(err)
	}
	reply := nextExecFrame(t, out, protocol.TypeConfigurationResult).Payload.(protocol.ConfigurationResult)
	if reply.Status != "ok" || reply.Result == nil || reply.Request != req.Request {
		t.Fatalf("reply %+v", reply)
	}
	if err := s.handle(execFrame(protocol.TypeConfigurationOpen, req), true); err == nil {
		t.Fatal("replay accepted")
	}
	for name, mutate := range map[string]func(*protocol.ConfigurationOpen){
		"wrong nonce":    func(r *protocol.ConfigurationOpen) { r.Connection = []byte("wrong") },
		"wrong endpoint": func(r *protocol.ConfigurationOpen) { r.Endpoint = "other" },
		"22 s grant":     func(r *protocol.ConfigurationOpen) { r.Expires = time.Now().Add(22 * time.Second) },
	} {
		bad := configurationRequest(15 * time.Second)
		mutate(&bad)
		s := newConfigurationsFor(req, opts, out)
		if err := s.handle(execFrame(protocol.TypeConfigurationOpen, bad), true); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if err := newConfigurationsFor(req, opts, out).handle(execFrame(protocol.TypeConfigurationOpen, configurationRequest(15*time.Second)), false); err == nil {
		t.Fatal("pending agent read configuration")
	}
	if len(out) != 0 {
		t.Fatal("ignored grants were answered")
	}
}

func TestConfigurationBusyUnavailableAndRedaction(t *testing.T) {
	out := make(chan outFrame, 8)
	release := make(chan struct{})
	opts := &Options{Configure: func(ctx context.Context, target protocol.InspectionTarget) (*protocol.ContainerConfiguration, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, errors.New("secret-canary")
	}}
	first := configurationRequest(15 * time.Second)
	s := newConfigurationsFor(first, opts, out)
	if err := s.handle(execFrame(protocol.TypeConfigurationOpen, first), true); err != nil {
		t.Fatal(err)
	}
	second := configurationRequest(15 * time.Second)
	second.Request = "second"
	if err := s.handle(execFrame(protocol.TypeConfigurationOpen, second), true); err != nil {
		t.Fatal(err)
	}
	if r := nextExecFrame(t, out, protocol.TypeConfigurationResult).Payload.(protocol.ConfigurationResult); r.Request != "second" || r.Status != "busy" {
		t.Fatalf("second open %+v", r)
	}
	close(release)
	if r := nextExecFrame(t, out, protocol.TypeConfigurationResult).Payload.(protocol.ConfigurationResult); r.Request != first.Request || r.Status != "unavailable" || r.Result != nil {
		t.Fatalf("runtime error exposed: %+v", r)
	}

	none := newConfigurationsFor(first, &Options{}, out)
	third := configurationRequest(15 * time.Second)
	third.Request = "third"
	if err := none.handle(execFrame(protocol.TypeConfigurationOpen, third), true); err != nil {
		t.Fatal(err)
	}
	if r := nextExecFrame(t, out, protocol.TypeConfigurationResult).Payload.(protocol.ConfigurationResult); r.Status != "unavailable" {
		t.Fatalf("nil Configure %+v", r)
	}
}

func TestConfigurationRejectsAnAnswerForAnotherTarget(t *testing.T) {
	out := make(chan outFrame, 8)
	opts := &Options{Configure: func(_ context.Context, target protocol.InspectionTarget) (*protocol.ContainerConfiguration, error) {
		other := target
		other.ContainerID = strings.Repeat("d", 64)
		return minimalConfiguration(other), nil
	}}
	req := configurationRequest(15 * time.Second)
	s := newConfigurationsFor(req, opts, out)
	if err := s.handle(execFrame(protocol.TypeConfigurationOpen, req), true); err != nil {
		t.Fatal(err)
	}
	if r := nextExecFrame(t, out, protocol.TypeConfigurationResult).Payload.(protocol.ConfigurationResult); r.Status != "unavailable" || r.Result != nil {
		t.Fatalf("%+v", r)
	}
}

func TestConfigurationCapabilityIsDockerOnlyAndOptional(t *testing.T) {
	configure := func(context.Context, protocol.InspectionTarget) (*protocol.ContainerConfiguration, error) {
		return nil, nil
	}
	if slices.Contains(helloCapabilities(&Options{}), protocol.CapabilityContainerConfigure) {
		t.Fatal("advertised without Configure")
	}
	docker := helloCapabilities(&Options{Configure: configure})
	if !slices.Contains(docker, protocol.CapabilityContainerConfigure) || !protocol.CapabilitiesFit(protocol.RuntimeDocker, docker) {
		t.Fatalf("Docker capabilities %v", docker)
	}
	if slices.Contains(helloCapabilities(&Options{Kubernetes: true, Configure: configure}), protocol.CapabilityContainerConfigure) {
		t.Fatal("Kubernetes agent advertised container.configure")
	}
}

// Run routes configuration.open to the configuration handler and answers configuration.result.
func TestConfigurationRoutedThroughTheSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	checked := make(chan error, 1)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checked <- func() error {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return err
			}
			defer conn.CloseNow()
			if err = write(ctx, conn, protocol.TypeChallenge, protocol.Challenge{Nonce: make([]byte, 32), InstanceFingerprint: "instance", Versions: []int{1}}); err != nil {
				return err
			}
			if _, err = read(ctx, conn); err != nil {
				return err
			}
			if err = write(ctx, conn, protocol.TypeHello, protocol.Hello{State: "approved", HeartbeatSeconds: 1}); err != nil {
				return err
			}
			req := configurationRequest(15 * time.Second)
			if err = write(ctx, conn, protocol.TypeConfigurationOpen, req); err != nil {
				return err
			}
			for {
				frame, err := read(ctx, conn)
				if err != nil {
					return err
				}
				if frame.Type != protocol.TypeConfigurationResult {
					continue
				}
				var reply protocol.ConfigurationResult
				if err := json.Unmarshal(frame.Payload, &reply); err != nil {
					return err
				}
				if reply.Request != req.Request || reply.Status != "ok" || reply.Result == nil {
					return errors.New("configuration not answered: " + reply.Status)
				}
				return nil
			}
		}()
	}))
	defer stub.Close()
	_, key, _ := ed25519.GenerateKey(nil)
	id := &Identity{EndpointID: "endpoint", PrivateKey: key, InstanceFingerprint: "instance", Server: stub.URL}
	opts := Options{HTTPClient: stub.Client(), IdentityDir: t.TempDir(), Log: log.New(io.Discard, "", 0), Configure: func(_ context.Context, target protocol.InspectionTarget) (*protocol.ContainerConfiguration, error) {
		return minimalConfiguration(target), nil
	}}
	done := make(chan error, 1)
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

// configuration.cancel stops the read, sends no answer and frees the slot.
func TestConfigurationCancelStopsTheRead(t *testing.T) {
	out := make(chan outFrame, 8)
	started, cancelled := make(chan struct{}, 1), make(chan struct{}, 1)
	var blocked atomic.Bool
	opts := &Options{Configure: func(ctx context.Context, target protocol.InspectionTarget) (*protocol.ContainerConfiguration, error) {
		if !blocked.Swap(true) {
			started <- struct{}{}
			<-ctx.Done()
			cancelled <- struct{}{}
		}
		return minimalConfiguration(target), nil
	}}
	req := configurationRequest(15 * time.Second)
	s := newConfigurationsFor(req, opts, out)
	if err := s.handle(execFrame(protocol.TypeConfigurationOpen, req), true); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := s.handle(execFrame(protocol.TypeConfigurationCancel, protocol.InspectionCancel{Request: req.Request}), true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancel not delivered")
	}
	next := configurationRequest(15 * time.Second)
	next.Request = "next"
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := s.handle(execFrame(protocol.TypeConfigurationOpen, next), true); err != nil {
			t.Fatal(err)
		}
		r := nextExecFrame(t, out, protocol.TypeConfigurationResult).Payload.(protocol.ConfigurationResult)
		if r.Request == req.Request {
			t.Fatalf("a cancelled read was answered: %+v", r)
		}
		if r.Status == "ok" {
			break
		}
		// busy until the cancelled read has released its slot; a fresh request ID each try.
		if r.Status != "busy" || time.Now().After(deadline) {
			t.Fatalf("after cancel: %+v", r)
		}
		next.Request += "x"
	}
}

// An answer that passes Validate but does not fit the frame with its envelope is sent as
// unavailable: the server closes the socket on an oversized frame.
func TestConfigurationOversizeAnswerIsUnavailable(t *testing.T) {
	out := make(chan outFrame, 8)
	req := configurationRequest(15 * time.Second)
	// Full argv entries, then the last one trimmed so the configuration is one byte under the cap.
	big := minimalConfiguration(req.Target)
	full := strings.Repeat("a", protocol.MaxArgvEntryBytes)
	size := func() int { raw, _ := json.Marshal(big); return len(raw) }
	for size() < protocol.MaxConfigurationFrameBytes {
		if len(big.Command) < protocol.MaxArgv {
			big.Command = append(big.Command, full)
		} else {
			big.Entrypoint = append(big.Entrypoint, full)
		}
	}
	last := &big.Entrypoint[len(big.Entrypoint)-1]
	*last = (*last)[:len(*last)-(size()-protocol.MaxConfigurationFrameBytes+1)]
	if n := size(); n != protocol.MaxConfigurationFrameBytes-1 {
		t.Fatalf("fixture is %d bytes", n)
	}
	if err := big.Validate(req.Target, time.Now()); err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	opts := &Options{Configure: func(context.Context, protocol.InspectionTarget) (*protocol.ContainerConfiguration, error) {
		return big, nil
	}}
	s := newConfigurationsFor(req, opts, out)
	if err := s.handle(execFrame(protocol.TypeConfigurationOpen, req), true); err != nil {
		t.Fatal(err)
	}
	if r := nextExecFrame(t, out, protocol.TypeConfigurationResult).Payload.(protocol.ConfigurationResult); r.Status != "unavailable" || r.Result != nil {
		t.Fatalf("oversize answer: %s %v", r.Status, r.Result != nil)
	}
}
