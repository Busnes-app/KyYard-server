// kyyard-agent enrolls a host with a KyYard control plane and keeps it connected.
//
//	printf '%s\n' "$TOKEN" | kyyard-agent --server https://kyyard.example --identity-dir /var/lib/kyyard-agent
//
// The token is read from stdin on the first run only; afterwards the identity on disk is used.
package main

import (
	"context"
	"crypto/ed25519"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/docker"
)

var version = "dev"

func main() {
	enrollOnly := flag.Bool("enroll-only", false, "enroll, print the key fingerprint and exit before connecting")
	server := flag.String("server", "", "control plane origin, e.g. https://kyyard.example")
	dir := flag.String("identity-dir", "/var/lib/kyyard-agent", "directory holding the agent identity (0700)")
	name := flag.String("name", "", "endpoint name to propose at enrollment (default: hostname)")
	rotate := flag.Duration("rotate-every", 30*24*time.Hour, "offer a new identity key this often (0 disables)")
	socket := flag.String("docker-socket", "/var/run/docker.sock", "Docker Engine socket to inventory (empty disables)")
	inventoryEvery := flag.Duration("inventory-every", time.Minute, "how often to report a fresh inventory snapshot")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	httpClient := &http.Client{Timeout: 30 * time.Second}
	// The Docker adapter is optional at start: a host whose daemon is down still enrolls and
	// reports facts, and the snapshot call keeps retrying the socket on its own schedule.
	var snapshot func(context.Context) (*protocol.Snapshot, error)
	var metrics func(context.Context, []string) protocol.Metrics
	var operate func(context.Context, protocol.Command) (string, string)
	var logs func(context.Context, protocol.LogRequest, func([]byte) error) error
	runtimeVersion := ""
	if *socket != "" {
		engine := docker.New(*socket)
		if facts, err := engine.Engine(ctx); err != nil {
			log.Printf("docker at %s not reachable (%v); reporting facts only until it is", *socket, err)
		} else {
			runtimeVersion = facts.Version
		}
		snapshot = engine.Snapshot
		metrics = engine.Stats
		operate = func(cctx context.Context, cmd protocol.Command) (string, string) { return engine.Operate(cctx, cmd) }
		logs = engine.Logs
	}

	id, err := client.LoadIdentity(*dir)
	if err != nil {
		log.Fatalf("identity: %v", err)
	}
	if id != nil && *enrollOnly {
		log.Fatalf("already enrolled as %s with %s; refusing a new enrollment in %s; restart without --enroll-only to reuse this identity", id.EndpointID, id.Server, *dir)
	}
	if id == nil {
		if *server == "" {
			log.Fatal("--server is required for enrollment")
		}
		token, err := client.ReadToken(os.Stdin)
		if err != nil {
			log.Fatalf("enrollment: %v", err)
		}
		if *name == "" {
			*name, _ = os.Hostname()
		}
		id, err = client.Enroll(ctx, httpClient, *server, *dir, *name, token, runtimeVersion)
		if err != nil {
			log.Fatalf("enrollment: %v", err)
		}
		log.Printf("enrolled as %s; waiting for approval", id.EndpointID)
	} else if *server != "" && *server != id.Server {
		log.Fatalf("identity is enrolled with %s, not %s; remove %s to re-enroll", id.Server, *server, *dir)
	}
	log.Printf("agent key fingerprint: %s", protocol.Fingerprint(ed25519.PrivateKey(id.PrivateKey).Public().(ed25519.PublicKey)))
	if *enrollOnly {
		return
	}
	if err := client.Run(ctx, id, client.Options{HTTPClient: httpClient, Version: version, IdentityDir: *dir, RotateEvery: *rotate, Snapshot: snapshot, Metrics: metrics, Operate: operate, Logs: logs, InventoryEvery: *inventoryEvery}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
