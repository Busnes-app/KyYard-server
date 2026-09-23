package api

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/docker"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// RunLocalDocker runs the existing authenticated agent against the local socket,
// inside the server process. No enrollment token, separate container or shell
// command is needed. Tenant authorization still gates every operator request.
func (s *Server) RunLocalDocker(ctx context.Context) error {
	socket := s.config.Server.DockerSocket
	if socket == "" {
		return nil
	}
	info, err := os.Stat(socket)
	if err != nil {
		return fmt.Errorf("local Docker socket unavailable at %s; mount the host Docker socket into KyYard: %w", socket, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("local Docker path %s is not a socket", socket)
	}
	if len(s.config.Security.InstanceKey) != ed25519.SeedSize {
		return errors.New("local Docker requires the persisted instance key")
	}
	// Domain-separated identity from a key already covered by capsule recovery.
	// Only the public key is stored in SQL; no new recoverable secret is introduced.
	mac := hmac.New(sha256.New, s.config.Security.InstanceKey)
	_, _ = mac.Write([]byte("kyyard/local-docker-agent/v1"))
	key := ed25519.NewKeyFromSeed(mac.Sum(nil))
	generation, err := s.store.Tenancy().InitializeLocalDocker(ctx, key.Public().(ed25519.PublicKey))
	if err != nil {
		return fmt.Errorf("local Docker initialization refused: %w", err)
	}
	// A private loopback listener lets the built-in agent use the same handshake,
	// commands, logs and revocation as remote agents, independent of proxy settings.
	// It exposes only the authenticated agent connection route.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/agent/v1/connect", s.tracked(func(w http.ResponseWriter, r *http.Request) {
		// Public loopback/proxy traffic must not consume this listener's retry budget.
		s.agentConnect(w, r, "builtin-agent-connect")
	}))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second}
	served := make(chan struct{})
	go func() { defer close(served); _ = server.Serve(listener) }()
	defer func() { _ = server.Close(); <-served }()
	dir := filepath.Join(s.config.Database.DataDir, "local-agent")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	id := &client.Identity{EndpointID: store.LocalDockerEndpointID, PrivateKey: key, InstanceFingerprint: s.instanceFingerprint(), Server: "http://" + listener.Addr().String(), Generation: generation}
	engine := docker.New(socket)
	// Only the command ledger needs disk persistence. Generation resumes from SQL;
	// client.SaveIdentity must not persist the derived private key or ephemeral URL.
	return client.Run(ctx, id, client.Options{Version: "builtin", CommandDir: dir, Snapshot: engine.Snapshot, Metrics: engine.Stats, Operate: engine.Operate, Logs: engine.Logs, Inspect: engine.InspectContainer, Deploy: engine.Deploy, Remove: engine.Remove, Exec: func(ctx context.Context, spec protocol.ExecSpec) (client.ExecSession, error) {
		return engine.OpenExec(ctx, spec)
	}, InventoryEvery: time.Minute})
}
