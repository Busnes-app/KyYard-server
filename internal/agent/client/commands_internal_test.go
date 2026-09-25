package client

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// A command re-sent after the agent restarts must return the answer it already gave, not run
// again. The server re-dispatches precisely when it never heard the first answer, so "I did
// not see a result" and "it did not happen" must not be conflated by the agent either.
func TestALedgerSurvivesRestartAndAnswersOnce(t *testing.T) {
	dir := t.TempDir()
	id := &Identity{EndpointID: "ep_1"}
	runs := 0
	opts := &Options{Operate: func(_ context.Context, cmd protocol.Command) (string, string) {
		runs++
		return protocol.OutcomeSucceeded, "stopped"
	}}
	cmd := protocol.Command{ID: "cmd_1", Endpoint: "ep_1", Action: protocol.ActionStop, Container: "c1", Deadline: time.Now().UTC().Add(time.Minute)}

	first := handleCommand(context.Background(), cmd, id, openLedger(dir), opts)
	if first.Outcome != protocol.OutcomeSucceeded || runs != 1 {
		t.Fatalf("first run: %+v after %d executions", first, runs)
	}
	// A fresh ledger, as a restarted agent would open.
	again := handleCommand(context.Background(), cmd, id, openLedger(dir), opts)
	if again.Outcome != protocol.OutcomeSucceeded || again.Detail != "stopped" {
		t.Fatalf("the stored answer was not returned: %+v", again)
	}
	if runs != 1 {
		t.Fatalf("the command ran %d times across a restart", runs)
	}
}

// A command addressed elsewhere is refused without running, and without being remembered:
// there is nothing to be idempotent about.
func TestACommandForAnotherEndpointIsRefused(t *testing.T) {
	dir := t.TempDir()
	ran := false
	opts := &Options{Operate: func(context.Context, protocol.Command) (string, string) {
		ran = true
		return protocol.OutcomeSucceeded, ""
	}}
	res := handleCommand(context.Background(), protocol.Command{ID: "cmd_2", Endpoint: "ep_other", Action: protocol.ActionStop}, &Identity{EndpointID: "ep_1"}, openLedger(dir), opts)
	if res.Outcome != protocol.OutcomeDenied || ran {
		t.Fatalf("a command for another endpoint was run: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "commands.json")); err == nil {
		t.Fatal("a refusal was written to the ledger")
	}
}

// A deadline already past means the runtime is never touched: the operator's decision has
// aged out, and acting on it late is worse than not acting.
func TestAnExpiredCommandIsNotRun(t *testing.T) {
	ran := false
	opts := &Options{Operate: func(context.Context, protocol.Command) (string, string) {
		ran = true
		return protocol.OutcomeSucceeded, ""
	}}
	res := handleCommand(context.Background(), protocol.Command{ID: "cmd_3", Endpoint: "ep_1", Deadline: time.Now().UTC().Add(-time.Minute)}, &Identity{EndpointID: "ep_1"}, openLedger(t.TempDir()), opts)
	if res.Outcome != protocol.OutcomeTimedOut || ran {
		t.Fatalf("an expired command was run: %+v", res)
	}
}

// The ledger is bounded both ways, so a control plane cannot make the agent's disk grow.
func TestTheLedgerIsBounded(t *testing.T) {
	dir := t.TempDir()
	l := openLedger(dir)
	// Filled directly: recording persists the whole file each time, which is right for an
	// agent that runs a command now and then and wrong for a loop of ten thousand.
	// Timestamped into the past, in order: an entry recorded now must be the newest, or the
	// trim would drop the real one and this would test nothing about the bound.
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < dedupeMax+50; i++ {
		l.done[fmt.Sprintf("cmd_%d", i)] = ledgerEntry{Outcome: protocol.OutcomeSucceeded, Finished: base.Add(time.Duration(i) * time.Millisecond)}
	}
	l.prune()
	if len(l.done) != dedupeMax {
		t.Fatalf("the ledger holds %d entries, past the %d limit", len(l.done), dedupeMax)
	}
	if _, ok := l.done["cmd_0"]; ok {
		t.Fatal("the oldest entry survived the trim; the newest should be what is kept")
	}
	// One real record proves the file is written and read back.
	l.record("cmd_written", protocol.OutcomeSucceeded, "done")
	if e, ok := openLedger(dir).lookup("cmd_written"); !ok || e.Detail != "done" {
		t.Fatal("a recorded answer did not survive reopening the ledger")
	}
	l.done["stale"] = ledgerEntry{Outcome: protocol.OutcomeSucceeded, Finished: time.Now().UTC().Add(-dedupeLife - time.Hour)}
	l.prune()
	if _, ok := l.done["stale"]; ok {
		t.Fatal("an entry past its life was kept")
	}
}

// A slot is agent-wide and lives for the process, so a worker that cannot hand its result over
// must still give the slot back. A session ends for ordinary reasons -- a dropped socket, a
// shutdown -- and a worker blocked forever on a loop that has gone would take a quarter of the
// agent's capacity with it each time, until every command is refused.
func TestASlotIsReturnedWhenTheSessionEndsBeforeTheResult(t *testing.T) {
	running := newBudget()
	results := make(chan protocol.Result, maxInFlightCommands)
	for i := 0; i < maxInFlightCommands; i++ {
		results <- protocol.Result{ID: fmt.Sprintf("earlier_%d", i)}
	}
	ctx, endSession := context.WithCancel(context.Background())
	release, ok := running.take(protocol.ActionImagePull)
	if !ok {
		t.Fatal("a fresh budget had no room")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer release()
		deliver(ctx, results, protocol.Result{ID: "cmd_late", Outcome: protocol.OutcomeSucceeded})
	}()
	endSession()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker is still waiting to deliver a result nobody is reading")
	}
	if len(running.all) != 0 || len(running.images) != 0 {
		t.Fatalf("%d general and %d image slots were never returned", len(running.all), len(running.images))
	}
}

// Image actions draw from a reserve of their own as well as a general slot, so a pull -- which
// waits on a remote registry -- can never occupy the whole set and leave an operator unable to
// stop a container during an incident.
func TestImageActionsCannotTakeEveryCommandSlot(t *testing.T) {
	running := newBudget()
	for i := 0; i < maxInFlightImageCommands; i++ {
		if _, ok := running.take(protocol.ActionImagePull); !ok {
			t.Fatalf("pull %d was refused inside the image reserve", i)
		}
	}
	if _, ok := running.take(protocol.ActionImageRemove); ok {
		t.Fatal("an image action was admitted past the image reserve")
	}
	for i := 0; i < maxInFlightCommands-maxInFlightImageCommands; i++ {
		if _, ok := running.take(protocol.ActionStop); !ok {
			t.Fatalf("container action %d was refused while the general budget had room", i)
		}
	}
	if _, ok := running.take(protocol.ActionStop); ok {
		t.Fatal("a container action was admitted past the general limit")
	}
}
