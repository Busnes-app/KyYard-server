package client

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// A reader that is not keeping up must cost the agent a bounded amount of memory, and the
// reader must be told what it missed: a log with a silent hole in it is worse than one that
// says where the hole is.
func TestASlowReaderGetsAGapRatherThanUnboundedMemory(t *testing.T) {
	const queued = 2
	out := make(chan outFrame, queued)
	// Nothing is read until the container has finished writing, which is what a reader that
	// has stalled looks like from here.
	sunk := make(chan struct{})
	opts := &Options{Logs: func(ctx context.Context, req protocol.LogRequest, sink func([]byte) error) error {
		for i := 0; i < 20; i++ {
			if err := sink([]byte(strings.Repeat("x", 100))); err != nil {
				return err
			}
		}
		close(sunk)
		return nil
	}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		readLog(context.Background(), protocol.LogRequest{Stream: "s1"}, opts, out)
	}()
	select {
	case <-sunk:
	case <-time.After(10 * time.Second):
		t.Fatal("the reader blocked on a queue nobody was draining")
	}

	var chunks []protocol.LogChunk
	var closed *protocol.LogClose
	for closed == nil {
		select {
		case f := <-out:
			switch payload := f.Payload.(type) {
			case protocol.LogChunk:
				chunks = append(chunks, payload)
			case protocol.LogClose:
				closed = &payload
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the reader never finished")
		}
	}
	<-done
	// What was held is the queue; everything else was dropped and counted.
	if len(chunks) > queued+1 {
		t.Fatalf("%d chunks were held for a reader that was not reading", len(chunks))
	}
	var dropped int64
	for _, c := range chunks {
		dropped += c.Dropped
	}
	if dropped == 0 {
		t.Fatal("bytes were dropped for a slow reader without saying so")
	}
	if closed.Failed {
		t.Fatalf("a log that ended normally was reported as a failure: %+v", closed)
	}
}

// A cancelled stream stops reading the host and says nothing more: the reader has gone, and
// there is nobody to tell.
func TestACancelledStreamStopsReading(t *testing.T) {
	out := make(chan outFrame, 8)
	reading := make(chan struct{})
	opts := &Options{Logs: func(ctx context.Context, req protocol.LogRequest, sink func([]byte) error) error {
		close(reading)
		<-ctx.Done()
		return ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		readLog(ctx, protocol.LogRequest{Stream: "s1"}, opts, out)
	}()
	<-reading
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the reader ignored the cancellation")
	}
	select {
	case f := <-out:
		t.Fatalf("a cancelled stream still sent %s", f.Type)
	default:
	}
}

// An agent with no runtime says so rather than leaving a request open forever.
func TestAnAgentWithoutARuntimeClosesTheStream(t *testing.T) {
	out := make(chan outFrame, 2)
	readLog(context.Background(), protocol.LogRequest{Stream: "s1"}, &Options{}, out)
	f := <-out
	closed, ok := f.Payload.(protocol.LogClose)
	if f.Type != protocol.TypeLogClose || !ok || !closed.Failed {
		t.Fatalf("an agent with no runtime answered %s %+v", f.Type, f.Payload)
	}
}

// Streams are counted, and the count is what stops one caller from holding readers open on a
// host. The third is refused rather than queued.
func TestOnlySoManyStreamsAreOpenAtOnce(t *testing.T) {
	live := newStreams()
	for i := 0; i < maxLogStreams; i++ {
		if !live.start(string(rune('a'+i)), func() {}) {
			t.Fatalf("stream %d was refused inside the limit", i)
		}
	}
	if live.start("one-too-many", func() {}) {
		t.Fatal("a stream was admitted past the limit")
	}
	stopped := false
	live.stop("a")
	if !live.start("a-again", func() { stopped = true }) {
		t.Fatal("a closed stream did not free its place")
	}
	live.stop("a-again")
	if !stopped {
		t.Fatal("stopping a stream did not cancel its reader")
	}
}
