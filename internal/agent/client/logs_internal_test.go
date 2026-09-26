package client

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// A reader that is not keeping up slows the read instead of losing the log: the sink waits for
// the queue, so every byte arrives in order, and memory stays bounded by the queue itself.
func TestASlowReaderSlowsTheReadAndLosesNothing(t *testing.T) {
	out := make(chan outFrame, 2)
	var want strings.Builder
	opts := &Options{Logs: func(ctx context.Context, req protocol.LogRequest, sink func([]byte) error) error {
		for i := 0; i < 20; i++ {
			line := strings.Repeat(string(rune('a'+i)), 99) + "\n"
			want.WriteString(line)
			if err := sink([]byte(line)); err != nil {
				return err
			}
		}
		return nil
	}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		readLog(context.Background(), protocol.LogRequest{Stream: "s1"}, opts, out)
	}()

	var got strings.Builder
	var closed *protocol.LogClose
	for closed == nil {
		time.Sleep(5 * time.Millisecond) // a reader draining slower than the container writes
		select {
		case f := <-out:
			switch payload := f.Payload.(type) {
			case protocol.LogChunk:
				if payload.Dropped != 0 {
					t.Fatalf("the agent dropped %d bytes", payload.Dropped)
				}
				got.WriteString(payload.Data)
			case protocol.LogClose:
				closed = &payload
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the reader never finished")
		}
	}
	<-done
	if got.String() != want.String() {
		t.Fatalf("%d of %d bytes arrived, or out of order", got.Len(), want.Len())
	}
	if closed.Failed {
		t.Fatalf("a log that ended normally was reported as a failure: %+v", closed)
	}
}

// A read waiting on a full queue ends when the stream is cancelled rather than holding the
// host's log open for a reader that has gone.
func TestACancelledStreamUnblocksAWaitingRead(t *testing.T) {
	out := make(chan outFrame) // nobody drains
	ended := make(chan error, 1)
	opts := &Options{Logs: func(ctx context.Context, req protocol.LogRequest, sink func([]byte) error) error {
		err := sink([]byte("waiting\n"))
		ended <- err
		return err
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		readLog(ctx, protocol.LogRequest{Stream: "s1"}, opts, out)
	}()
	select {
	case err := <-ended:
		t.Fatalf("the sink returned %v with nobody draining the queue", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("a cancelled sink reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the sink ignored the cancellation")
	}
	<-done
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
	f := nextFrame(t, out)
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

// The agent's limit is the protocol's, not a number of this package's own. The control plane
// hands out places against the same one and splits them per reader so that members holding
// nothing but container.logs cannot deny an administrator a host's logs; an agent refusing
// earlier would quietly make that split meaningless.
func TestTheAgentServesAsManyStreamsAsTheProtocolSays(t *testing.T) {
	if maxLogStreams != protocol.MaxLogStreamsPerEndpoint {
		t.Fatalf("the agent serves %d streams while the control plane hands out %d", maxLogStreams, protocol.MaxLogStreamsPerEndpoint)
	}
}

// A log is bytes, not text: a container writing anything but ASCII produces reads that end
// mid-character, and coercing those to valid UTF-8 makes the value grow. The bound the agent
// promises is on what travels, so it has to be applied after the coercion -- an oversized
// chunk is a protocol violation, and the control plane answers one by ending the stream.
func TestChunksStayInsideTheBoundAfterCoercion(t *testing.T) {
	out := make(chan outFrame, 64)
	// Alternating text and invalid bytes, which is what a read that ends mid-character looks
	// like: each invalid run becomes a three-byte replacement, so the coerced value is twice
	// the size of the read.
	block := make([]byte, protocol.MaxLogChunkBytes)
	for i := range block {
		if i%2 == 0 {
			block[i] = 'a'
			continue
		}
		block[i] = 0xff
	}
	opts := &Options{Logs: func(ctx context.Context, req protocol.LogRequest, sink func([]byte) error) error {
		return sink(block)
	}}
	readLog(context.Background(), protocol.LogRequest{Stream: "s1"}, opts, out)
	close(out)

	total := 0
	for f := range out {
		chunk, ok := f.Payload.(protocol.LogChunk)
		if !ok {
			continue
		}
		if len(chunk.Data) > protocol.MaxLogChunkBytes {
			t.Fatalf("a %d byte chunk was sent, past the %d the protocol allows", len(chunk.Data), protocol.MaxLogChunkBytes)
		}
		if !utf8.ValidString(chunk.Data) {
			t.Fatalf("a chunk was cut through a character: %q", chunk.Data)
		}
		total += len(chunk.Data)
	}
	if want := protocol.MaxLogChunkBytes / 2 * 4; total != want {
		t.Fatalf("%d bytes of coerced text arrived, want %d", total, want)
	}
}
