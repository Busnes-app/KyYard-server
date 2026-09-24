package client

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/coder/websocket"
)

var errSwitchKey = errors.New("switching keys")

// Errors the loop treats as terminal: reconnecting cannot help.
var (
	ErrRevoked         = errors.New("identity revoked by the control plane")
	ErrInstanceChanged = errors.New("control plane identity changed; re-enroll this host")
	ErrIncompatible    = errors.New("control plane does not speak this protocol version")
)

type Options struct {
	inspectionSlots chan struct{}
	Inspect         func(context.Context, protocol.InspectionTarget) (*protocol.ContainerInspection, error)
	HTTPClient      *http.Client
	Version         string
	Log             *log.Logger
	// IdentityDir is where the rising inventory generation and rotation state are written back.
	IdentityDir string
	// CommandDir overrides the durable command ledger directory for embedded agents
	// whose identity and inventory generation are maintained by their control plane.
	CommandDir string
	// RotateEvery is how often the agent offers a new key; zero disables rotation.
	RotateEvery time.Duration
	// Snapshot reads the runtime; nil reports facts only (no runtime reachable).
	Snapshot func(ctx context.Context) (*protocol.Snapshot, error)
	// Metrics samples the running containers named in the last snapshot; nil sends none.
	Metrics func(ctx context.Context, running []string) protocol.Metrics
	// InventoryEvery is how often a fresh snapshot is sent while connected.
	InventoryEvery time.Duration
	// Operate runs one container action and reports the outcome and a short reason. Nil means
	// this agent has no runtime to operate, and every command is refused rather than dropped.
	// The context ends when the session that carried the command does, so a dropped socket
	// stops work nobody is waiting for rather than leaving it running blind against the host.
	Operate func(ctx context.Context, cmd protocol.Command) (outcome, detail string)
	// Logs streams one container's log to sink until the log ends, the context is cancelled
	// or sink refuses. Nil means this agent has no runtime, and a request for logs is closed
	// with that reason rather than left open.
	Logs func(ctx context.Context, req protocol.LogRequest, sink func([]byte) error) error
	// Exec opens a PTY attachment after a nonce-bound control-plane grant.
	// Nil disables exec when no runtime adapter is configured.
	Exec func(context.Context, protocol.ExecSpec) (ExecSession, error)
	// Deploy applies one deployment plan and reports its result. It runs on the agent's root
	// context, not the session's, so a dropped socket never stops it; the request bounds it by
	// its deadline. Nil means this agent has no runtime, and every apply is refused.
	Deploy func(context.Context, protocol.DeploymentRequest) protocol.DeploymentResult
	// Remove tears down the containers a removal names and reports its result. It shares
	// Deploy's single slot and root context and is bounded by the request's deadline. Nil
	// means this agent has no runtime, and every removal is refused.
	Remove func(context.Context, protocol.RemovalRequest) protocol.DeploymentResult
	// OnState is called with the state the server reported at connect (tests).
	OnState func(state string)
}

// checkServerOrigin admits https anywhere and http only to loopback: enrollment carries the
// single-use token and every connection carries the identity, so neither may cross a network
// in the clear.
func checkServerOrigin(server string) (*url.URL, error) {
	u, err := url.Parse(server)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "https":
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && host != "localhost" {
			return nil, fmt.Errorf("refusing plaintext connection to %s; use https", host)
		}
	default:
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("server must be an origin without credentials, query or fragment")
	}
	return u, nil
}

// ConnectURL turns the enrolled server origin into the socket URL.
func ConnectURL(server string) (string, error) {
	u, err := checkServerOrigin(server)
	if err != nil {
		return "", err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = "/api/agent/v1/connect"
	return u.String(), nil
}

// Run keeps the agent connected until ctx ends or a terminal error occurs. Backoff starts at
// one second, doubles to a minute, and carries ±20 % jitter.
func Run(ctx context.Context, id *Identity, opts Options) error {
	if opts.Log == nil {
		opts.Log = log.Default()
	}
	target, err := ConnectURL(id.Server)
	if err != nil {
		return err
	}
	// The ledger and the in-flight limit belong to the agent, not to one connection. A command
	// started before a drop is still running after the redial, so a per-session limit would
	// grant four more with every reconnect, and a per-session ledger would serialise its stale
	// view over the file and erase what the new session recorded.
	commandDir := opts.CommandDir
	if commandDir == "" {
		commandDir = opts.IdentityDir
	}
	commands := openLedger(commandDir)
	deployments := newDeployer(ctx, commandDir, &opts)
	// A run outlives its session; Run returns only once the result is on disk.
	defer deployments.wait()
	running := newBudget()
	execRunning := &execBudget{}
	opts.inspectionSlots = make(chan struct{}, protocol.MaxInspectionsPerEndpoint)
	delay := time.Second
	for {
		err := session(ctx, id, target, &opts, commands, deployments, running, execRunning)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrRevoked) || errors.Is(err, ErrInstanceChanged) || errors.Is(err, ErrIncompatible) {
			return err
		}
		if errors.Is(err, errSwitchKey) {
			// Reconnect with the key the server now expects, after a short pause so the server
			// has released this endpoint's slot: an immediate redial can read as a duplicate.
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return nil
			}
			continue
		}
		if err == nil {
			delay = time.Second // a clean session resets the backoff
		} else {
			opts.Log.Printf("connection ended: %v; retrying in %s", err, delay.Round(time.Millisecond))
		}
		jitter := time.Duration((rand.Float64()*0.4 - 0.2) * float64(delay))
		select {
		case <-time.After(delay + jitter):
		case <-ctx.Done():
			return nil
		}
		if delay < 60*time.Second {
			delay *= 2
		}
	}
}

func session(ctx context.Context, id *Identity, target string, opts *Options, commands *ledger, deployments *deployer, running *budget, execRunning *execBudget) error {
	// Commands run under the session's own context, so when this returns the work it started
	// is cancelled rather than left to finish against a host nobody is watching.
	ctx, endSession := context.WithCancel(ctx)
	defer endSession()
	u, _ := url.Parse(target)
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	conn, _, err := websocket.Dial(dctx, target, &websocket.DialOptions{HTTPClient: opts.HTTPClient})
	cancel()
	if err != nil {
		return err
	}
	conn.SetReadLimit(4 << 20)
	defer conn.CloseNow()

	// The handshake has its own deadline: a server that accepts and then says nothing must not
	// hold the agent forever.
	hctx, hcancel := context.WithTimeout(ctx, 15*time.Second)
	defer hcancel()
	// Challenge: the server must present the instance we enrolled with.
	f, err := read(hctx, conn)
	if err != nil {
		return closeReason(err)
	}
	var ch protocol.Challenge
	if f.Type != protocol.TypeChallenge || json.Unmarshal(f.Payload, &ch) != nil || len(ch.Nonce) != 32 {
		return errors.New("bad challenge")
	}
	if ch.InstanceFingerprint != id.InstanceFingerprint {
		return ErrInstanceChanged
	}
	sig := ed25519.Sign(id.signingKey(), protocol.AuthPreimage(id.EndpointID, ch.Nonce, u.Host, protocol.Version))
	if err := write(hctx, conn, protocol.TypeAuth, protocol.Auth{EndpointID: id.EndpointID, Fingerprint: id.fingerprint(), Version: protocol.Version, Signature: sig}); err != nil {
		return err
	}
	f, err = read(hctx, conn)
	if err != nil {
		var ce websocket.CloseError
		if errors.As(err, &ce) {
			switch strings.TrimSpace(ce.Reason) {
			case protocol.CloseKeyRetired:
				// Our key was retired (an acknowledged rotation while we were away). Walk to
				// the next candidate; only with none left is the endpoint really gone.
				if id.tryNext() {
					return errSwitchKey
				}
			case protocol.CloseRevoked:
				// Terminal for the identity's own key: the server answers the same way for a
				// signature mismatch or a store error, and a rotation window must not turn
				// either into a lost identity. Mid-walk the key in hand has only ever been
				// refused, and an unknown key looks exactly like a revoked endpoint, so the
				// remaining candidates are still worth one try each. Nothing is overwritten
				// on the way, so a server fault that refuses them all leaves every key where
				// it was and the walk simply ends.
				if id.recovering() && id.tryNext() {
					return errSwitchKey
				}
			}
		}
		return closeReason(err)
	}
	// The server accepted this key, which is the only proof that the old one is retired: the
	// candidate becomes the identity and the others are dropped.
	if id.recovering() {
		id.commitCandidate()
		_ = opts.save(id)
	}
	// Buffered, but what keeps a worker from waiting forever is the session context rather
	// than the buffer: results already sitting here are not counted against a slot, so more
	// senders than the buffer holds is an ordinary state.
	results := make(chan protocol.Result, maxInFlightCommands)
	// Log chunks are session state: the reader waiting for them is an HTTP request the server
	// holds open on this same socket, so a stream that outlived the session would be reading
	// the host for nobody.
	outbound := make(chan outFrame, logQueueDepth)
	live := newStreams()
	terminals := newExecStreams(ctx, id.EndpointID, ch.Nonce, execRunning, opts, outbound)
	inspections := newInspections(ctx, id.EndpointID, ch.Nonce, opts, outbound)
	var hello protocol.Hello
	if f.Type != protocol.TypeHello || json.Unmarshal(f.Payload, &hello) != nil {
		return errors.New("bad hello")
	}
	if opts.OnState != nil {
		opts.OnState(hello.State)
	}
	heartbeat := time.Duration(hello.HeartbeatSeconds) * time.Second
	if heartbeat <= 0 || heartbeat > 55*time.Second {
		heartbeat = 30 * time.Second
	}
	capabilities := []string{}
	if opts.Inspect != nil {
		capabilities = append(capabilities, protocol.CapabilityContainerInspect)
	}
	if opts.Exec != nil {
		capabilities = append(capabilities, "container.exec")
	}
	if opts.Deploy != nil {
		capabilities = append(capabilities, protocol.CapabilityDeploymentApply, protocol.CapabilityDeploymentPull)
	}
	if opts.Remove != nil {
		capabilities = append(capabilities, protocol.CapabilityDeploymentRemove)
	}
	if err := write(ctx, conn, protocol.TypeHello, protocol.Hello{Capabilities: capabilities, AgentVersion: opts.Version}); err != nil {
		return err
	}
	metricsOut := make(chan protocol.Metrics, 1)
	if hello.State != "pending" {
		if err := sendInventory(ctx, conn, id, opts, metricsOut); err != nil {
			return err
		}
		// Attach before the re-send, so a run finishing in between is delivered here or re-sent.
		defer deployments.attach(ctx, outbound)()
		go deployments.resend(ctx, outbound)
		if id.PendingFingerprint != "" && time.Since(id.PendingSince) > PendingKeyLife {
			// Forget the offer but keep the key material until a new offer replaces it: if a
			// late acknowledgement retires the current key anyway, key_retired can still promote.
			opts.Log.Printf("pending key %s was never acknowledged; offer lapsed", id.PendingFingerprint)
			id.LapsedPrivateKey, id.LapsedRecorded = id.PendingPrivateKey, id.PendingRecorded
			id.PendingPrivateKey, id.PendingFingerprint, id.PendingSince = nil, "", time.Time{}
			id.PendingRecorded = false
			_ = opts.save(id)
		}
		if opts.RotateEvery > 0 && id.PendingFingerprint == "" && time.Since(id.RotatedAt) >= opts.RotateEvery {
			if err := offerRotation(ctx, conn, id, opts); err != nil {
				return err
			}
		}
	} else {
		opts.Log.Printf("enrollment pending approval as %s", id.EndpointID)
	}

	frames := make(chan protocol.Envelope)
	readErr := make(chan error, 1)
	go func() {
		for {
			// The server answers every heartbeat, so silence for two intervals means the path is
			// dead even if TCP has not noticed; reconnecting is the only way to hear about
			// approval or revocation again.
			rctx, rcancel := context.WithTimeout(ctx, 2*heartbeat)
			f, err := read(rctx, conn)
			rcancel()
			if err != nil {
				if rctx.Err() != nil && ctx.Err() == nil {
					err = errors.New("no frame from the control plane for two heartbeat intervals")
				}
				readErr <- err
				return
			}
			select {
			case frames <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	inventoryEvery := opts.InventoryEvery
	if inventoryEvery <= 0 {
		inventoryEvery = 60 * time.Second
	}
	inventory := time.NewTicker(inventoryEvery)
	defer inventory.Stop()
	for {
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "agent_shutdown")
			return nil
		case <-ticker.C:
			if err := write(ctx, conn, protocol.TypeHeartbeat, nil); err != nil {
				return err
			}
		case <-inventory.C:
			if hello.State != "pending" {
				if err := sendInventory(ctx, conn, id, opts, metricsOut); err != nil {
					return err
				}
			}
		case res := <-results:
			if err := write(ctx, conn, protocol.TypeResult, res); err != nil {
				return err
			}
		case f := <-outbound:
			if err := write(ctx, conn, f.Type, f.Payload); err != nil {
				return err
			}
		case m := <-metricsOut:
			if err := write(ctx, conn, protocol.TypeMetrics, m); err != nil {
				return err
			}
		case err := <-readErr:
			// The operator acknowledged our rotated key and the server ended this session on
			// the old one: switch now instead of waiting for the next backoff.
			var ce websocket.CloseError
			if errors.As(err, &ce) && strings.TrimSpace(ce.Reason) == protocol.CloseKeyRetired && id.tryNext() {
				return errSwitchKey
			}
			return closeReason(err)
		case f := <-frames:
			switch f.Type {
			case protocol.TypeApproved:
				opts.Log.Printf("approved; reconnecting with full protocol")
				conn.Close(websocket.StatusNormalClosure, "approved")
				return nil
			case protocol.TypeRotate:
				var ack protocol.Rotated
				_ = json.Unmarshal(f.Payload, &ack)
				if ack.Code != "" || ack.Fingerprint != id.PendingFingerprint {
					opts.Log.Printf("rotation not recorded (%s); keeping the current key", ack.Code)
					id.PendingPrivateKey, id.PendingFingerprint, id.PendingRecorded = nil, "", false
					if ack.Code == "rotation_pending" && len(id.LapsedPrivateKey) == ed25519.PrivateKeySize {
						// The server still holds the offer we gave up on: it is the live one again,
						// and saying so is the server confirming it holds that key.
						id.PendingPrivateKey = id.LapsedPrivateKey
						id.PendingFingerprint = id.pendingFingerprint()
						id.PendingSince = time.Now().UTC()
						id.PendingRecorded = true
						id.LapsedPrivateKey, id.LapsedRecorded = nil, false
					}
					_ = opts.save(id)
				} else {
					// A recorded offer proves any lapsed key is retired server-side.
					id.LapsedPrivateKey, id.LapsedRecorded = nil, false
					id.PendingRecorded = true
					_ = opts.save(id)
					opts.Log.Printf("rotation recorded as %s; waiting for operator acknowledgement", ack.Fingerprint)
				}
			case protocol.TypeRotated:
				var done protocol.Rotated
				_ = json.Unmarshal(f.Payload, &done)
				if done.Fingerprint != "" && id.promotable() && done.Fingerprint == id.pendingFingerprint() {
					id.Promote()
					_ = opts.save(id)
					opts.Log.Printf("rotation acknowledged; reconnecting with the new key")
					conn.Close(websocket.StatusNormalClosure, "rotated")
					return errSwitchKey
				}
			case protocol.TypeCommand:
				var cmd protocol.Command
				if json.Unmarshal(f.Payload, &cmd) != nil || cmd.ID == "" {
					opts.Log.Printf("unreadable command frame")
					break
				}
				// A command already answered is answered again with the same answer, before
				// anything else is considered: the server re-dispatches precisely when it
				// never heard the first result, and refusing it for being busy would report
				// work that did happen as work that did not.
				if prior, ok := commands.lookup(cmd.ID); ok {
					res := protocol.Result{ID: cmd.ID, Outcome: prior.Outcome, Detail: prior.Detail}
					if err := write(ctx, conn, protocol.TypeResult, res); err != nil {
						return err
					}
					break
				}
				// Run off the loop. A pull takes as long as the network does, and a session
				// waiting for one sends no heartbeats, so the endpoint is marked offline and
				// stops accepting the commands an operator needs during an incident. The
				// agent's liveness must not depend on how long a runtime call takes.
				release, taken := running.take(cmd.Action)
				if !taken {
					// Refused rather than queued: an answer now beats an answer later, and a
					// queue lets one caller spend the agent's memory.
					res := protocol.Result{ID: cmd.ID, Outcome: protocol.OutcomeDenied, Detail: "this agent is already running as many commands as it will"}
					if err := write(ctx, conn, protocol.TypeResult, res); err != nil {
						return err
					}
					break
				}
				go func(cmd protocol.Command) {
					defer release()
					deliver(ctx, results, handleCommand(ctx, cmd, id, commands, opts))
				}(cmd)
			case protocol.TypeInspectionOpen, protocol.TypeInspectionCancel:
				if err := inspections.handle(f, hello.State == "active" || hello.State == "approved" || hello.State == "offline"); err != nil {
					return err
				}
			case protocol.TypeExecOpen, protocol.TypeExecInput, protocol.TypeExecResize, protocol.TypeExecCancel:
				if err := terminals.handle(f, hello.State == "active" || hello.State == "approved" || hello.State == "offline"); err != nil {
					reason := "exec stream limit reached; not started"
					if errors.Is(err, errExecUnavailable) {
						reason = errExecUnavailable.Error()
					} else if !errors.Is(err, errExecCapacity) {
						return err
					}
					var req protocol.ExecOpen
					_ = json.Unmarshal(f.Payload, &req)
					if err := write(ctx, conn, protocol.TypeExecClose, protocol.ExecClose{Stream: req.Stream, Reason: reason}); err != nil {
						return err
					}
				}
			case protocol.TypeDeploymentApply, protocol.TypeDeploymentRemove:
				if len(f.Payload) > protocol.MaxDeploymentRequestBytes {
					conn.Close(websocket.StatusPolicyViolation, protocol.CloseProtocol)
					return errors.New("deployment request too large")
				}
				if hello.State == "pending" {
					break
				}
				if f.Type == protocol.TypeDeploymentRemove {
					deployments.handleRemoval(ctx, id.EndpointID, f.Payload, outbound)
				} else {
					deployments.handleApply(ctx, id.EndpointID, f.Payload, outbound)
				}
			case protocol.TypeLogOpen:
				var req protocol.LogRequest
				if json.Unmarshal(f.Payload, &req) != nil || req.Stream == "" {
					opts.Log.Printf("unreadable log request")
					break
				}
				if req.Tail <= 0 || req.Tail > protocol.MaxLogTail {
					req.Tail = protocol.MaxLogTail
				}
				sctx, stop := context.WithCancel(ctx)
				if !live.start(req.Stream, stop) {
					stop()
					refused := protocol.LogClose{Stream: req.Stream, Reason: "this agent is already reading as many logs as it will", Failed: true}
					if err := write(ctx, conn, protocol.TypeLogClose, refused); err != nil {
						return err
					}
					break
				}
				go func(req protocol.LogRequest) {
					defer live.stop(req.Stream)
					readLog(sctx, req, opts, outbound)
				}(req)
			case protocol.TypeLogCancel:
				var cancel protocol.LogCancel
				if json.Unmarshal(f.Payload, &cancel) == nil {
					live.stop(cancel.Stream)
				}
			case protocol.TypeHeartbeat:
			case protocol.TypeError:
				opts.Log.Printf("server error frame: %s", string(f.Payload))
			}
		}
	}
}

func closeReason(err error) error {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		switch strings.TrimSpace(ce.Reason) {
		case protocol.CloseRevoked, protocol.CloseRejected:
			return ErrRevoked
		case protocol.CloseIncompatible:
			return ErrIncompatible
		case protocol.CloseShutdown, protocol.CloseTimeout, protocol.CloseDuplicate:
			return fmt.Errorf("server closed: %s", ce.Reason)
		}
	}
	return err
}

func write(ctx context.Context, conn *websocket.Conn, typ string, payload any) error {
	raw, _ := json.Marshal(payload)
	frame, err := json.Marshal(protocol.Envelope{V: protocol.Version, Type: typ, Payload: raw})
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, frame)
}

func read(ctx context.Context, conn *websocket.Conn) (protocol.Envelope, error) {
	var e protocol.Envelope
	_, raw, err := conn.Read(ctx)
	if err != nil {
		return e, err
	}
	return e, json.Unmarshal(raw, &e)
}

func (o *Options) save(id *Identity) error {
	if o.IdentityDir == "" {
		return nil
	}
	if err := SaveIdentity(o.IdentityDir, id); err != nil {
		o.Log.Printf("could not persist identity: %v", err)
		return err
	}
	return nil
}

// PendingKeyLife is a day longer than the server's seven-day window: the agent must forget an
// offer strictly after the server stops accepting it, so an acknowledgement can never land on
// a key the agent no longer holds. The extra day is clock-skew margin, not a mirror.
const PendingKeyLife = 8 * 24 * time.Hour

// offerRotation mints a key and persists it as pending before it is sent, so a crash after the
// server recorded it cannot lose it. If the key cannot be persisted nothing is sent: announcing
// a key held only in memory would strand the agent once an operator acknowledges it.
func offerRotation(ctx context.Context, conn *websocket.Conn, id *Identity, opts *Options) error {
	pub, priv, err := newKey()
	if err != nil {
		return err
	}
	current := id.signingKey()
	id.PendingPrivateKey = priv
	id.PendingFingerprint = protocol.Fingerprint(pub)
	id.PendingSince = time.Now().UTC()
	// Unconfirmed until the server answers: the save must come first so a key the server may
	// record is never lost, but a key it never saw must not outrank a recorded one.
	id.PendingRecorded = false
	if err := opts.save(id); err != nil {
		id.PendingPrivateKey, id.PendingFingerprint = nil, ""
		return fmt.Errorf("rotation not offered: %w", err)
	}
	sig := ed25519.Sign(current, protocol.Preimage(protocol.ContextRotate, protocol.RawFingerprint(current.Public().(ed25519.PublicKey)), pub))
	return write(ctx, conn, protocol.TypeRotate, protocol.Rotate{PublicKey: pub, Signature: sig})
}

// sendInventory reads the runtime (or reports facts only) under a generation that rises across
// restarts. The persisted counter is the only source of truth: trusting a fast host wall clock
// can create a generation the server rejects for being in the future and persist the wedge.
func sendInventory(ctx context.Context, conn *websocket.Conn, id *Identity, opts *Options, metricsOut chan<- protocol.Metrics) error {
	gen := id.Generation + 1
	var snap *protocol.Snapshot
	if opts.Snapshot != nil {
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		s, err := opts.Snapshot(sctx)
		cancel()
		if err != nil {
			opts.Log.Printf("runtime snapshot failed: %v; reporting facts only", err)
		} else {
			snap = s
		}
	}
	if snap == nil {
		snap = &protocol.Snapshot{ObservedAt: time.Now().UTC(), Containers: []protocol.Container{}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}}
	}
	snap.Generation = gen
	if err := write(ctx, conn, protocol.TypeInventory, snap); err != nil {
		return err
	}
	id.Generation = gen
	_ = opts.save(id)
	if opts.Metrics != nil && metricsOut != nil {
		running := make([]string, 0, len(snap.Containers))
		for _, ct := range snap.Containers {
			if ct.State == "running" {
				running = append(running, ct.ID)
			}
		}
		// Sampling talks to the runtime once per container; it runs off the loop so a slow
		// daemon can never starve heartbeats, and a frame is dropped if the previous one is
		// still waiting to be written.
		go func() {
			mctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			m := opts.Metrics(mctx, running)
			m = protocol.ShrinkMetrics(m)
			if len(m.Samples) == 0 {
				return
			}
			select {
			case metricsOut <- m:
			default:
			}
		}()
	}
	return nil
}
