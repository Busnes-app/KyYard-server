package client

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// maxInFlightCommands bounds what one agent runs at once. Commands are operator-initiated and
// rare, so several at a time is generous; the limit is what keeps one caller from spending the
// agent's memory or the host's disk with concurrent pulls.
const maxInFlightCommands = 4

// maxInFlightImageCommands is the share of that limit image actions may hold. A pull is
// bounded by a remote registry rather than by the daemon, so without a reserve of its own a
// handful of them -- four ordinary large images, or one caller with only image.pull -- would
// occupy every slot and make an administrator's emergency stop come back denied for minutes.
// The endpoint you cannot manage during an incident is the outcome all of this exists to
// prevent, and moving it from the heartbeat path to the command path would not be fixing it.
const maxInFlightImageCommands = maxInFlightCommands / 2

// budget is what one agent runs at once. It belongs to the agent rather than to a session: a
// command outlives the socket it arrived on, so a per-session budget would grant a fresh set
// with every redial.
type budget struct {
	all    chan struct{}
	images chan struct{}
}

func newBudget() *budget {
	return &budget{
		all:    make(chan struct{}, maxInFlightCommands),
		images: make(chan struct{}, maxInFlightImageCommands),
	}
}

// take reserves capacity for one command. An image action needs both its own reserve and a
// general slot, so container actions always have capacity left. The caller runs release when
// the command is done with; not ok means the agent is full and the command must be refused
// rather than queued.
func (b *budget) take(action string) (release func(), ok bool) {
	image := protocol.IsImageAction(action)
	if image {
		select {
		case b.images <- struct{}{}:
		default:
			return nil, false
		}
	}
	select {
	case b.all <- struct{}{}:
	default:
		if image {
			<-b.images
		}
		return nil, false
	}
	return func() {
		<-b.all
		if image {
			<-b.images
		}
	}, true
}

// Command dedupe limits from docs/agent-protocol.md.
const (
	// dedupeLife is how long a finished command's answer is remembered, so a server that
	// re-dispatches the same ID gets the same answer rather than a second execution.
	dedupeLife = 24 * time.Hour
	// dedupeMax bounds the file: an agent must not be made to grow one without limit by a
	// control plane that keeps sending commands.
	dedupeMax = 10000
)

// ledger remembers what this agent has already done. It is written to the identity volume
// because the point is to survive a restart: a command re-sent after the agent came back must
// not run twice, and "did I already do this" is not a question the runtime can answer.
type ledger struct {
	mu   sync.Mutex
	path string
	done map[string]ledgerEntry
}

type ledgerEntry struct {
	Outcome  string    `json:"outcome"`
	Detail   string    `json:"detail,omitempty"`
	Finished time.Time `json:"finished"`
}

func openLedger(dir string) *ledger {
	l := &ledger{path: filepath.Join(dir, "commands.json"), done: map[string]ledgerEntry{}}
	raw, err := os.ReadFile(l.path)
	if err != nil {
		return l
	}
	var saved map[string]ledgerEntry
	if json.Unmarshal(raw, &saved) == nil {
		l.done = saved
		l.prune()
	}
	return l
}

// lookup returns a previous answer for this command, if there is one.
func (l *ledger) lookup(id string) (ledgerEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.done[id]
	return e, ok
}

// record stores an answer and persists the ledger. A write failure is not fatal: the command
// did run, and refusing to report it would be worse than risking a repeat after a crash.
func (l *ledger) record(id, outcome, detail string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.done[id] = ledgerEntry{Outcome: outcome, Detail: detail, Finished: time.Now().UTC()}
	l.prune()
	if raw, err := json.Marshal(l.done); err == nil {
		tmp := l.path + ".tmp"
		if os.WriteFile(tmp, raw, 0o600) == nil {
			_ = os.Rename(tmp, l.path)
		}
	}
}

// prune drops what is too old, then the oldest of what remains until the file fits. The caller
// holds the lock.
func (l *ledger) prune() {
	cutoff := time.Now().UTC().Add(-dedupeLife)
	for id, e := range l.done {
		if e.Finished.Before(cutoff) {
			delete(l.done, id)
		}
	}
	if len(l.done) <= dedupeMax {
		return
	}
	ids := make([]string, 0, len(l.done))
	for id := range l.done {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return l.done[ids[i]].Finished.Before(l.done[ids[j]].Finished) })
	for _, id := range ids[:len(l.done)-dedupeMax] {
		delete(l.done, id)
	}
}

// deliver hands a result to the session loop, or gives up when the session ends. Waiting for a
// loop that has already returned would hold the in-flight slot for the life of the process --
// the slots are agent-wide by design -- and enough of those and the agent refuses every
// command, which is the unmanageable endpoint this whole arrangement exists to prevent.
// Nothing is lost by giving up: the answer is already in the ledger, so the server's unknown
// and a re-dispatch of the same ID replay it.
func deliver(ctx context.Context, out chan<- protocol.Result, res protocol.Result) {
	select {
	case out <- res:
	case <-ctx.Done():
	}
}

// handleCommand answers one command: a repeat gets the stored answer, a command for another
// tenant or endpoint is refused without being run, and anything else is executed and recorded.
func handleCommand(ctx context.Context, cmd protocol.Command, id *Identity, l *ledger, opts *Options) protocol.Result {
	if prior, ok := l.lookup(cmd.ID); ok {
		// Already done. The server may be re-dispatching because it never heard the answer.
		return protocol.Result{ID: cmd.ID, Outcome: prior.Outcome, Detail: prior.Detail}
	}
	if cmd.Endpoint != id.EndpointID {
		// A command addressed to someone else is refused and not remembered: there is nothing
		// to be idempotent about.
		return protocol.Result{ID: cmd.ID, Outcome: protocol.OutcomeDenied, Detail: "this command is addressed to another endpoint"}
	}
	if !cmd.Deadline.IsZero() && time.Now().UTC().After(cmd.Deadline) {
		return protocol.Result{ID: cmd.ID, Outcome: protocol.OutcomeTimedOut, Detail: "the deadline had passed before the command was read"}
	}
	if opts.Operate == nil {
		return protocol.Result{ID: cmd.ID, Outcome: protocol.OutcomeDenied, Detail: "this agent has no runtime to operate"}
	}
	outcome, detail := opts.Operate(ctx, cmd)
	l.record(cmd.ID, outcome, detail)
	return protocol.Result{ID: cmd.ID, Outcome: outcome, Detail: detail}
}
