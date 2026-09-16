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

// Errors the loop treats as terminal: reconnecting cannot help.
var (
	ErrRevoked         = errors.New("identity revoked by the control plane")
	ErrInstanceChanged = errors.New("control plane identity changed; re-enroll this host")
	ErrIncompatible    = errors.New("control plane does not speak this protocol version")
)

type Options struct {
	HTTPClient *http.Client
	Version    string
	Log        *log.Logger
	// IdentityDir is where the rising inventory generation is written back; empty skips it.
	IdentityDir string
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
	delay := time.Second
	for {
		err := session(ctx, id, target, &opts)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrRevoked) || errors.Is(err, ErrInstanceChanged) || errors.Is(err, ErrIncompatible) {
			return err
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

func session(ctx context.Context, id *Identity, target string, opts *Options) error {
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
	sig := ed25519.Sign(ed25519.PrivateKey(id.PrivateKey), protocol.AuthPreimage(id.EndpointID, ch.Nonce, u.Host, protocol.Version))
	if err := write(hctx, conn, protocol.TypeAuth, protocol.Auth{EndpointID: id.EndpointID, Version: protocol.Version, Signature: sig}); err != nil {
		return err
	}
	f, err = read(hctx, conn)
	if err != nil {
		return closeReason(err)
	}
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
	if err := write(ctx, conn, protocol.TypeHello, protocol.Hello{Capabilities: []string{}, AgentVersion: opts.Version}); err != nil {
		return err
	}
	if hello.State != "pending" {
		// Generations must rise across restarts, or a restarted agent's first snapshot would be
		// ignored and the endpoint would stay offline. Wall-clock seeding covers a lost file.
		gen := id.Generation + 1
		if now := uint64(time.Now().Unix()); now > gen {
			gen = now
		}
		if err := write(ctx, conn, protocol.TypeInventory, protocol.Inventory{Generation: gen, Facts: Facts("")}); err != nil {
			return err
		}
		id.Generation = gen
		if opts.IdentityDir != "" {
			if err := SaveIdentity(opts.IdentityDir, id); err != nil {
				opts.Log.Printf("could not persist inventory generation: %v", err)
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
	for {
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "agent_shutdown")
			return nil
		case <-ticker.C:
			if err := write(ctx, conn, protocol.TypeHeartbeat, nil); err != nil {
				return err
			}
		case err := <-readErr:
			return closeReason(err)
		case f := <-frames:
			switch f.Type {
			case protocol.TypeApproved:
				opts.Log.Printf("approved; reconnecting with full protocol")
				conn.Close(websocket.StatusNormalClosure, "approved")
				return nil
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
