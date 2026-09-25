package protocol

import "time"

// Log frame types. A log stream is a conversation inside the one socket: the server opens it,
// the agent sends chunks until it ends or is cancelled, and either side can end it.
const (
	TypeLogOpen   = "log.open"   // server → agent: start reading one container's log
	TypeLogChunk  = "log.chunk"  // agent → server: the next piece of it
	TypeLogClose  = "log.close"  // agent → server: this stream is over, and why
	TypeLogCancel = "log.cancel" // server → agent: the reader went away, stop reading
)

// Log bounds, from docs/retention-policy.md. Log bodies are never stored, so these bound
// memory and work rather than disk: what an operator asked for must not be able to cost the
// control plane or the agent more than this, however much the container has written.
const (
	// MaxLogChunkBytes is one chunk on the wire. Small enough that a slow reader costs little
	// to hold and that a chunk fits well inside the control-frame limit.
	MaxLogChunkBytes = 32 << 10
	// MaxLogFrameBytes bounds a log chunk frame. It is larger than the control-frame limit
	// because JSON escaping can multiply a byte by six, and a log carries whatever the
	// application printed; the log text inside it is still bounded by MaxLogChunkBytes.
	MaxLogFrameBytes = 256 << 10
	// MaxLogLines and MaxLogBytes bound one request: whichever comes first ends it, and the
	// answer says it was truncated rather than pretending it was the whole log.
	MaxLogLines = 10000
	MaxLogBytes = 4 << 20
	// MaxLogTail is the most history one request may ask the runtime for.
	MaxLogTail = MaxLogLines
	// MaxLogStreamsPerEndpoint is how many log streams one endpoint serves at once. Both
	// sides read it from here: if the agent's limit were the lower of the two it would be the
	// real ceiling, and the control plane's per-reader split -- which exists so that members
	// holding nothing but container.logs cannot deny an administrator the logs of a host --
	// would hand out places the agent then refuses.
	MaxLogStreamsPerEndpoint = 4
	// LogFollowBuffer is what one following stream may hold for a reader that is not keeping
	// up. Past it, bytes are dropped and the gap is reported: a reader watching a chatty
	// container must not be able to grow the control plane's memory without limit.
	LogFollowBuffer = 1 << 20
)

// LogRequest opens one stream. Container is the ID the control plane resolved, never a name:
// a name is a label the runtime reassigns, and a stream must not follow it to another
// container mid-read. A Kubernetes endpoint is asked for a Pod instead (ValidateFor).
type LogRequest struct {
	Stream     string     `json:"stream"`
	Container  string     `json:"container"`
	Pod        *PodTarget `json:"pod,omitempty"`
	Tail       int        `json:"tail"`
	Since      time.Time  `json:"since,omitempty"`
	Timestamps bool       `json:"timestamps"`
	Follow     bool       `json:"follow"`
}

// LogChunk is log text as the container wrote it, with the count of bytes the agent had to
// drop before it. Dropped is the gap marker: the reader is told what it did not get rather
// than shown a seamless log with a hole in it.
type LogChunk struct {
	Stream  string `json:"stream"`
	Data    string `json:"data"`
	Dropped int64  `json:"dropped,omitempty"`
}

// LogClose ends a stream. Reason is short and operator-facing.
type LogClose struct {
	Stream string `json:"stream"`
	Reason string `json:"reason"`
	// Failed says the stream ended because something went wrong rather than because the log
	// ended or the reader left, so the reader can tell an empty log from a broken one.
	Failed bool `json:"failed,omitempty"`
}

// LogCancel stops a stream the reader has abandoned.
type LogCancel struct {
	Stream string `json:"stream"`
}
