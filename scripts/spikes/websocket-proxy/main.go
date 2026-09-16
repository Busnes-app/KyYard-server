// Disposable compatibility spike for docs/agent-protocol.md section 2: does an outbound
// WebSocket with 30-second heartbeats survive Caddy and nginx defaults, reconnect after a
// proxy restart, and carry a 4 MiB frame? Run through run.sh; results go in the doc.
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/coder/websocket"
)

const (
	idleFor  = 90 * time.Second // longer than nginx's 60 s proxy_read_timeout default
	bigFrame = 4 << 20
)

func main() {
	mode := flag.String("mode", "server", "server | client")
	addr := flag.String("addr", "127.0.0.1:8090", "server listen address")
	url := flag.String("url", "", "client: ws:// URL through a proxy")
	name := flag.String("name", "proxy", "client: label for the report")
	restartAt := flag.Duration("restart-after", 0, "client: expect a proxy restart this long into the idle phase (0 = none)")
	beat := flag.Duration("heartbeat", 30*time.Second, "client: heartbeat interval; 75s proves the proxy timeout bites without one")
	flag.Parse()
	if *mode == "server" {
		runServer(*addr)
		return
	}
	if err := runClient(*url, *name, *restartAt, *beat); err != nil {
		fmt.Printf("RESULT %s FAIL %v\n", *name, err)
		os.Exit(1)
	}
}

func runServer(addr string) {
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"127.0.0.1:8081", "127.0.0.1:8082"}})
		if err != nil {
			log.Printf("accept: %v", err)
			return
		}
		c.SetReadLimit(bigFrame + 1024)
		defer c.CloseNow()
		for {
			typ, data, err := c.Read(r.Context())
			if err != nil {
				log.Printf("server: connection ended: %v", err)
				return
			}
			if err := c.Write(r.Context(), typ, data); err != nil {
				return
			}
		}
	})
	log.Printf("server listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// dial connects with exponential backoff and returns how many attempts it took.
func dial(ctx context.Context, url string) (*websocket.Conn, int, error) {
	delay := time.Second
	for attempt := 1; ; attempt++ {
		c, _, err := websocket.Dial(ctx, url, nil)
		if err == nil {
			c.SetReadLimit(bigFrame + 1024)
			return c, attempt, nil
		}
		if attempt >= 8 {
			return nil, attempt, err
		}
		time.Sleep(delay)
		if delay < 60*time.Second {
			delay *= 2
		}
	}
}

func echo(ctx context.Context, c *websocket.Conn, payload []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, payload); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	_, got, err := c.Read(ctx)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if len(got) != len(payload) {
		return fmt.Errorf("echo length %d != %d", len(got), len(payload))
	}
	return nil
}

func runClient(url, name string, restartAt, heartbeat time.Duration) error {
	ctx := context.Background()
	c, attempts, err := dial(ctx, url)
	if err != nil {
		return fmt.Errorf("initial dial: %w", err)
	}
	fmt.Printf("%s: connected (attempts=%d)\n", name, attempts)

	// Idle phase: only heartbeats cross the proxy.
	deadline := time.Now().Add(idleFor)
	beats := 0
	for time.Now().Before(deadline) {
		time.Sleep(heartbeat)
		if err := echo(ctx, c, []byte("heartbeat")); err != nil {
			if restartAt > 0 {
				// A proxy restart is expected to drop us; reconnect and continue.
				fmt.Printf("%s: dropped during idle after %d heartbeats (%v); reconnecting\n", name, beats, err)
				c.CloseNow()
				c, attempts, err = dial(ctx, url)
				if err != nil {
					return fmt.Errorf("reconnect after restart: %w", err)
				}
				fmt.Printf("%s: reconnected (attempts=%d)\n", name, attempts)
				restartAt = 0
				continue
			}
			return fmt.Errorf("heartbeat %d failed during idle: %w", beats+1, err)
		}
		beats++
	}
	fmt.Printf("%s: idle survived %v with %d heartbeats\n", name, idleFor, beats)

	big := make([]byte, bigFrame)
	if _, err := rand.Read(big); err != nil {
		return err
	}
	start := time.Now()
	if err := echo(ctx, c, big); err != nil {
		return fmt.Errorf("4 MiB frame: %w", err)
	}
	fmt.Printf("%s: 4 MiB frame echoed in %v\n", name, time.Since(start).Round(time.Millisecond))
	c.Close(websocket.StatusNormalClosure, "done")
	fmt.Printf("RESULT %s PASS\n", name)
	return nil
}
