package api

import (
	"sync"
	"sync/atomic"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/google/uuid"
)

const (
	// logQueueChunks is what one stream holds for a reader that is not keeping up, in chunks
	// of protocol.MaxLogChunkBytes: protocol.LogFollowBuffer in total. Past it the control
	// plane drops and reports the gap. A browser on a slow connection watching a chatty
	// container is the ordinary case, and it must cost a bounded amount of memory here.
	logQueueChunks = protocol.LogFollowBuffer / protocol.MaxLogChunkBytes
	// maxStreamsPerEndpoint is what one host will serve at once, and it is deliberately above
	// maxStreamsPerActor so that one member cannot fill it: a developer holding nothing but
	// container.logs must not be able to deny an administrator the logs of a host during an
	// incident. It is the protocol's number, which the agent enforces too, so the two cannot
	// drift into a lower real ceiling than the one this split assumes.
	maxStreamsPerEndpoint = protocol.MaxLogStreamsPerEndpoint
	// maxStreamsPerActor is what one person may hold on one endpoint. One is enough to watch
	// a container; a second is a tab someone forgot.
	maxStreamsPerActor = 1
)

// logStream is one reader's view of one container's log. Chunks arrive from the endpoint's
// session goroutine and leave through the HTTP handler that opened it.
type logStream struct {
	id         string
	endpointID string
	actorID    string
	chunks     chan protocol.LogChunk
	done       chan struct{}
	doneOnce   sync.Once
	end        atomic.Pointer[protocol.LogClose]
	// dropped counts bytes this control plane could not queue for a slow reader. It is read
	// and cleared by the reader, which turns it into a gap marker.
	dropped atomic.Int64
}

// logRegistry holds the streams this server has open. It is per server rather than per agent
// session: the reader is an HTTP request, and a session that drops must end the stream rather
// than leave the request waiting for an endpoint that is no longer connected.
type logRegistry struct {
	mu      sync.Mutex
	streams map[string]*logStream
	perEnd  map[string]int
	perPair map[string]int // endpoint and actor together
}

func newLogRegistry() *logRegistry {
	return &logRegistry{streams: map[string]*logStream{}, perEnd: map[string]int{}, perPair: map[string]int{}}
}

func pairKey(endpointID, actorID string) string { return endpointID + "\x00" + actorID }

// open reserves a stream, or says which limit stopped it: the host's, or this reader's own.
func (r *logRegistry) open(endpointID, actorID string) (*logStream, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.perPair[pairKey(endpointID, actorID)] >= maxStreamsPerActor {
		return nil, "You already have a log stream open on this endpoint"
	}
	if r.perEnd[endpointID] >= maxStreamsPerEndpoint {
		return nil, "This endpoint already has as many log streams open as it will serve"
	}
	s := &logStream{
		id:         uuid.NewString(),
		endpointID: endpointID,
		actorID:    actorID,
		chunks:     make(chan protocol.LogChunk, logQueueChunks),
		done:       make(chan struct{}),
	}
	r.streams[s.id] = s
	r.perEnd[endpointID]++
	r.perPair[pairKey(endpointID, actorID)]++
	return s, ""
}

// release forgets a stream. The reader calls it when it is done, however it ended.
func (r *logRegistry) release(s *logStream) {
	r.mu.Lock()
	if _, ok := r.streams[s.id]; ok {
		delete(r.streams, s.id)
		r.perEnd[s.endpointID]--
		if r.perEnd[s.endpointID] <= 0 {
			delete(r.perEnd, s.endpointID)
		}
		key := pairKey(s.endpointID, s.actorID)
		r.perPair[key]--
		if r.perPair[key] <= 0 {
			delete(r.perPair, key)
		}
	}
	r.mu.Unlock()
	s.finish(nil)
}

// find returns the stream this endpoint may write to. The endpoint is part of the lookup: a
// stream identifier an agent invented, or one belonging to another endpoint, addresses
// nothing.
func (r *logRegistry) find(endpointID, streamID string) *logStream {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.streams[streamID]; s != nil && s.endpointID == endpointID {
		return s
	}
	return nil
}

// closeActorStreams ends every stream one person is holding, wherever it is. An administrator
// who removes a member or takes a role away has decided they may not see this; the decision
// takes effect in the same request rather than whenever the stream happens to end.
func (r *logRegistry) closeActorStreams(actorID, reason string) {
	r.closeMatching(func(s *logStream) bool { return s.actorID == actorID }, reason)
}

// closeEndpointStreams ends every stream an endpoint has open. A session that has gone cannot
// finish them, and a reader waiting on a disconnected endpoint should be told so.
func (r *logRegistry) closeEndpointStreams(endpointID, reason string) {
	r.closeMatching(func(s *logStream) bool { return s.endpointID == endpointID }, reason)
}

func (r *logRegistry) closeMatching(match func(*logStream) bool, reason string) {
	r.mu.Lock()
	var ending []*logStream
	for _, s := range r.streams {
		if match(s) {
			ending = append(ending, s)
		}
	}
	r.mu.Unlock()
	for _, s := range ending {
		s.finish(&protocol.LogClose{Stream: s.id, Reason: reason, Failed: true})
	}
}

// deliver queues a chunk for the reader, or counts what it had to drop. It never blocks: the
// session loop delivering this chunk also carries heartbeats.
func (s *logStream) deliver(chunk protocol.LogChunk) {
	if chunk.Dropped > 0 {
		s.dropped.Add(chunk.Dropped)
	}
	if chunk.Data == "" {
		return
	}
	select {
	case s.chunks <- chunk:
	default:
		s.dropped.Add(int64(len(chunk.Data)))
	}
}

// finish ends the stream once, recording why. A later end does not overwrite the first: the
// first reason is the one that explains what happened.
func (s *logStream) finish(end *protocol.LogClose) {
	s.doneOnce.Do(func() {
		if end != nil {
			s.end.Store(end)
		}
		close(s.done)
	})
}

// gap returns and clears the bytes dropped so far, which the reader reports inline.
func (s *logStream) gap() int64 { return s.dropped.Swap(0) }
