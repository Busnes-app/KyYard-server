package client

import (
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
	opts := &Options{Operate: func(cmd protocol.Command) (string, string) {
		runs++
		return protocol.OutcomeSucceeded, "stopped"
	}}
	cmd := protocol.Command{ID: "cmd_1", Endpoint: "ep_1", Action: protocol.ActionStop, Container: "c1", Deadline: time.Now().UTC().Add(time.Minute)}

	first := handleCommand(cmd, id, openLedger(dir), opts)
	if first.Outcome != protocol.OutcomeSucceeded || runs != 1 {
		t.Fatalf("first run: %+v after %d executions", first, runs)
	}
	// A fresh ledger, as a restarted agent would open.
	again := handleCommand(cmd, id, openLedger(dir), opts)
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
	opts := &Options{Operate: func(protocol.Command) (string, string) { ran = true; return protocol.OutcomeSucceeded, "" }}
	res := handleCommand(protocol.Command{ID: "cmd_2", Endpoint: "ep_other", Action: protocol.ActionStop}, &Identity{EndpointID: "ep_1"}, openLedger(dir), opts)
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
	opts := &Options{Operate: func(protocol.Command) (string, string) { ran = true; return protocol.OutcomeSucceeded, "" }}
	res := handleCommand(protocol.Command{ID: "cmd_3", Endpoint: "ep_1", Deadline: time.Now().UTC().Add(-time.Minute)}, &Identity{EndpointID: "ep_1"}, openLedger(t.TempDir()), opts)
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
