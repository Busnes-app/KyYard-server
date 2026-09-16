// kyyard-agent enrolls a host with a KyYard control plane and keeps it connected.
//
//	printf '%s\n' "$TOKEN" | kyyard-agent --server https://kyyard.example --identity-dir /var/lib/kyyard-agent
//
// The token is read from stdin on the first run only; afterwards the identity on disk is used.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Busness-app/kyyard-server/internal/agent/client"
)

var version = "dev"

func main() {
	server := flag.String("server", "", "control plane origin, e.g. https://kyyard.example")
	dir := flag.String("identity-dir", "/var/lib/kyyard-agent", "directory holding the agent identity (0700)")
	name := flag.String("name", "", "endpoint name to propose at enrollment (default: hostname)")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	httpClient := &http.Client{Timeout: 30 * time.Second}

	id, err := client.LoadIdentity(*dir)
	if err != nil {
		log.Fatalf("identity: %v", err)
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
		id, err = client.Enroll(ctx, httpClient, *server, *dir, *name, token)
		if err != nil {
			log.Fatalf("enrollment: %v", err)
		}
		log.Printf("enrolled as %s; waiting for approval", id.EndpointID)
	} else if *server != "" && *server != id.Server {
		log.Fatalf("identity is enrolled with %s, not %s; remove %s to re-enroll", id.Server, *server, *dir)
	}
	if err := client.Run(ctx, id, client.Options{HTTPClient: httpClient, Version: version}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
