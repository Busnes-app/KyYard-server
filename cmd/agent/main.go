// kyyard-agent enrolls a Docker host or a Kubernetes cluster with a KyYard control plane and
// keeps it connected.
//
//	printf '%s\n' "$TOKEN" | kyyard-agent --server https://kyyard.example --identity-dir /var/lib/kyyard-agent
//	kyyard-agent --kubernetes --link-file /etc/kyyard/link --docker-socket=   (the generated manifest)
//
// The token is used on the first run only; afterwards the stored identity is used.
package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/runtime/docker"
	"github.com/Busnes-app/kyyard-server/internal/runtime/kubernetes"
)

var version = "dev"

func main() {
	link := flag.String("link", "", "enrollment link copied from KyYard (first start only)")
	linkFile := flag.String("link-file", "", "file holding the enrollment link (first start only; a missing file means none)")
	enrollOnly := flag.Bool("enroll-only", false, "enroll, print the key fingerprint and exit before connecting")
	server := flag.String("server", "", "control plane origin, e.g. https://kyyard.example")
	dir := flag.String("identity-dir", "/var/lib/kyyard-agent", "directory holding the agent identity (0700)")
	name := flag.String("name", "", "endpoint name to propose at enrollment (default: hostname)")
	rotate := flag.Duration("rotate-every", 30*24*time.Hour, "offer a new identity key this often (0 disables)")
	socket := flag.String("docker-socket", "/var/run/docker.sock", "Docker Engine socket to inventory (empty disables)")
	inventoryEvery := flag.Duration("inventory-every", time.Minute, "how often to report a fresh inventory snapshot")
	kube := flag.Bool("kubernetes", false, "inventory the cluster this agent runs in, as its ServiceAccount (needs --docker-socket=)")
	identitySecret := flag.String("identity-secret", "kyyard-agent-identity", "with --kubernetes: the Secret in the agent's namespace that holds its identity")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)
	if err := checkFlags(*kube, *socket, *link, *linkFile); err != nil {
		log.Fatal(err)
	}
	if *linkFile != "" {
		var err error
		if *link, err = readLinkFile(*linkFile); err != nil {
			log.Fatalf("enrollment link: %v", err)
		}
	}
	var linkToken string
	if *link != "" {
		if *server != "" {
			log.Fatal("use --link or --server, not both")
		}
		origin, token, err := client.ParseLink(*link)
		if err != nil {
			log.Fatal(err)
		}
		*server, linkToken = origin, token
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	httpClient := &http.Client{Timeout: 30 * time.Second}
	// The Docker adapter is optional at start: a host whose daemon is down still enrolls and
	// reports facts, and the snapshot call keeps retrying the socket on its own schedule.
	var snapshot func(context.Context) (*protocol.Snapshot, error)
	var metrics func(context.Context, []string) protocol.Metrics
	var operate func(context.Context, protocol.Command) (string, string)
	var logs func(context.Context, protocol.LogRequest, func([]byte) error) error
	var inspect func(context.Context, protocol.InspectionTarget) (*protocol.ContainerInspection, error)
	var exec func(context.Context, protocol.ExecSpec) (client.ExecSession, error)
	var deploy func(context.Context, protocol.DeploymentRequest, func()) protocol.DeploymentResult
	var remove func(context.Context, protocol.RemovalRequest, func()) protocol.DeploymentResult
	runtimeVersion := ""
	var identities client.IdentityStore = client.DirStore(*dir)
	where := *dir
	var facts map[string]string
	if *kube {
		cluster, err := kubernetes.InCluster()
		if err != nil {
			log.Fatalf("kubernetes: %v", err)
		}
		namespace, err := kubernetes.OwnNamespace()
		if err != nil {
			log.Fatalf("kubernetes: %v", err)
		}
		secret, err := kubernetes.NewSecretIdentityStore(cluster, namespace, *identitySecret)
		if err != nil {
			log.Fatalf("identity: %v", err)
		}
		identities, where = fatalOnConflict{secret}, "Secret "+namespace+"/"+*identitySecret
		snapshot, logs, deploy, remove = cluster.Snapshot, cluster.Logs, cluster.Deploy, cluster.Remove
		facts = cluster.Facts(ctx)
	} else if *socket != "" {
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
		inspect = engine.InspectContainer
		deploy = engine.Deploy
		remove = engine.Remove
		exec = func(ctx context.Context, spec protocol.ExecSpec) (client.ExecSession, error) {
			return engine.OpenExec(ctx, spec)
		}
	}

	if facts == nil {
		facts = client.Facts(runtimeVersion)
	}

	id, err := identities.Load()
	if err != nil {
		log.Fatalf("identity: %v", err)
	}
	if id != nil && *enrollOnly {
		log.Fatalf("already enrolled as %s with %s; refusing a new enrollment in %s; restart without --enroll-only to reuse this identity", id.EndpointID, id.Server, where)
	}
	if id == nil {
		if *server == "" {
			log.Fatal("--server is required for enrollment")
		}
		token := linkToken
		if token == "" {
			token, err = client.ReadToken(os.Stdin)
			if err != nil {
				log.Fatalf("enrollment: %v", err)
			}
		}
		if *name == "" {
			*name, _ = os.Hostname()
		}
		id, err = client.Enroll(ctx, httpClient, *server, identities, *name, token, facts)
		if err != nil {
			log.Fatalf("enrollment: %v", err)
		}
		log.Printf("enrolled as %s; waiting for approval", id.EndpointID)
	} else if linkToken != "" && !id.MatchesEnrollment(*server, linkToken) {
		log.Fatalf("the identity in %s belongs to another enrollment; reuse the original link or clear that identity", where)
	} else if *server != "" && *server != id.Server {
		log.Fatalf("identity is enrolled with %s, not %s; remove %s to re-enroll", id.Server, *server, where)
	}
	log.Printf("agent key fingerprint: %s", protocol.Fingerprint(ed25519.PrivateKey(id.PrivateKey).Public().(ed25519.PublicKey)))
	if *enrollOnly {
		return
	}
	opts := client.Options{HTTPClient: httpClient, Version: version, Identities: identities, Kubernetes: *kube, RotateEvery: *rotate, Snapshot: snapshot, Metrics: metrics, Operate: operate, Logs: logs, Exec: exec, Inspect: inspect, Deploy: deploy, Remove: remove, InventoryEvery: *inventoryEvery}
	if !*kube {
		// The command ledger lives beside a host's identity; a cluster agent runs no commands.
		opts.IdentityDir = *dir
	}
	if err := client.Run(ctx, id, opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

// checkFlags refuses combinations that cannot mean anything.
func checkFlags(kube bool, socket, link, linkFile string) error {
	if link != "" && linkFile != "" {
		return errors.New("use --link or --link-file, not both")
	}
	if kube && socket != "" {
		return errors.New("kubernetes and docker are exclusive: pass --docker-socket= with --kubernetes")
	}
	return nil
}

// readLinkFile returns the link a mounted Secret holds, or "" when the file is absent: the
// manifest mounts that Secret optional, so a pod restarted after the operator deleted the
// spent Secret starts from its stored identity.
func readLinkFile(path string) (string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", err
	}
	if len(raw) > 4096 {
		return "", errors.New("the link file holds more than a link")
	}
	return strings.TrimSpace(string(raw)), nil
}

// fatalOnConflict stops the agent when its identity Secret was written by someone else: two
// agents sharing one identity is the failure the Secret store exists to prevent, and the pod
// restart that follows reads the Secret afresh.
type fatalOnConflict struct {
	*kubernetes.SecretIdentityStore
}

func (f fatalOnConflict) Save(id *client.Identity) error {
	err := f.SecretIdentityStore.Save(id)
	if errors.Is(err, kubernetes.ErrIdentityConflict) {
		log.Fatalf("identity: %v", err)
	}
	return err
}
