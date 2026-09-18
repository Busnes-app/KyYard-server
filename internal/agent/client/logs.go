package client

import (
	"context"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

const (
	// maxLogStreams bounds how many logs one agent reads at once. It is the protocol's number
	// rather than one of this package's own: the control plane hands out places against the
	// same limit, and an agent that refused earlier would make that split meaningless.
	maxLogStreams = protocol.MaxLogStreamsPerEndpoint
	// logQueueDepth is how many chunks wait for the session loop before the agent starts
	// dropping and reporting the gap. This is the agent's half of the bound: the control
	// plane has its own, because a slow browser must not become memory on either side.
	logQueueDepth = 8
)

// streams tracks the log streams one session has open, so a cancel can reach the reader and a
// dropped socket can stop all of them. It is per session on purpose: the reader waiting for
// these chunks is an HTTP request held open by this same socket, so a stream that outlived the
// session would be reading a host for nobody. Commands are the opposite case -- they change
// the host, so they finish -- which is why their budget is agent-wide and this one is not.
type streams struct {
	mu   sync.Mutex
	open map[string]context.CancelFunc
}

func newStreams() *streams { return &streams{open: map[string]context.CancelFunc{}} }

// start registers a stream, or reports that the agent is already reading as many as it will.
func (s *streams) start(id string, cancel context.CancelFunc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.open) >= maxLogStreams || s.open[id] != nil {
		return false
	}
	s.open[id] = cancel
	return true
}

func (s *streams) stop(id string) {
	s.mu.Lock()
	cancel := s.open[id]
	delete(s.open, id)
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// outFrame is a frame a worker wants the session loop to write. Only the loop writes to the
// socket, so everything a goroutine produces travels this way.
type outFrame struct {
	Type    string
	Payload any
}

// readLog streams one container's log to the session loop until it ends, the reader cancels,
// or the runtime stops. It never blocks on a loop that is not draining: a chunk that cannot be
// queued is dropped and counted, and the count travels with the next chunk so the reader is
// told about the hole rather than shown a log that looks continuous.
func readLog(ctx context.Context, req protocol.LogRequest, opts *Options, out chan<- outFrame) {
	close := protocol.LogClose{Stream: req.Stream, Reason: "the log ended"}
	if opts.Logs == nil {
		close.Reason, close.Failed = "this agent has no runtime to read logs from", true
		deliverFrame(ctx, out, outFrame{protocol.TypeLogClose, close})
		return
	}
	var dropped int64
	sink := func(b []byte) error {
		// Coerced to valid UTF-8 and no further: a log is the application's own bytes, and an
		// agent that reformatted them would be lying about what the container printed. The
		// control plane treats it as text and never interprets it.
		//
		// Coercion grows the text -- every invalid byte becomes a three-byte replacement, and
		// a multi-byte character straddling a read boundary is enough to produce one -- so the
		// bound is applied after it, to the value that actually travels. An oversized chunk is
		// a protocol violation, and reading a container that logs anything but ASCII must not
		// be able to commit one.
		text := strings.ToValidUTF8(string(b), "�")
		for len(text) > 0 {
			piece := text[:cutAtRune(text, protocol.MaxLogChunkBytes)]
			text = text[len(piece):]
			select {
			case out <- (outFrame{protocol.TypeLogChunk, protocol.LogChunk{Stream: req.Stream, Data: piece, Dropped: dropped}}):
				dropped = 0
			case <-ctx.Done():
				return ctx.Err()
			default:
				// The loop is behind. Dropping is the bounded answer; the next chunk says how
				// much was lost.
				dropped += int64(len(piece))
			}
		}
		return nil
	}
	if err := opts.Logs(ctx, req, sink); err != nil && ctx.Err() == nil {
		close.Reason, close.Failed = protocol.CleanText(err.Error(), protocol.MaxResultDetailBytes), true
	}
	if ctx.Err() != nil {
		// Cancelled: the reader is gone, so there is no one to tell.
		return
	}
	if dropped > 0 {
		// Report the last gap even if nothing followed it, or a stream that ended while the
		// loop was behind would look complete.
		deliverFrame(ctx, out, outFrame{protocol.TypeLogChunk, protocol.LogChunk{Stream: req.Stream, Dropped: dropped}})
	}
	deliverFrame(ctx, out, outFrame{protocol.TypeLogClose, close})
}

// cutAtRune is the largest cut at or below max that does not split a character in half. A
// chunk is text by the time it gets here, and half a character is not text.
func cutAtRune(s string, max int) int {
	if len(s) <= max {
		return len(s)
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut == 0 {
		// Unreachable for valid UTF-8, where no character is longer than four bytes, but a
		// cut of zero would spin forever and that is not a risk worth leaving open.
		return max
	}
	return cut
}

// deliverFrame hands a frame to the session loop, or gives up when the session ends, for the
// same reason deliver does with a result.
func deliverFrame(ctx context.Context, out chan<- outFrame, f outFrame) {
	select {
	case out <- f:
	case <-ctx.Done():
	}
}
