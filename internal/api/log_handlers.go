package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

const (
	// defaultLogTail is what a request gets when it names no history: enough to see what a
	// container is doing now without waiting for a long log to arrive.
	defaultLogTail = 200
	// maxLogLineBytes bounds one line. A container that prints megabytes without a newline
	// must not become a line this server is still assembling; past it the line is split and
	// the split is reported.
	maxLogLineBytes = 64 << 10
	// maxLogSearchBytes bounds the filter. It is a plain substring, matched here rather than
	// at the runtime, because the runtime has no such filter and the bound on what is read
	// must not depend on how many lines happen to match.
	maxLogSearchBytes = 200
	// followKeepalive keeps an idle follow from being dropped by whatever sits between the
	// browser and this server. A quiet container is the common case.
	followKeepalive = 25 * time.Second
)

// handleContainerLogs streams one container's log to the caller: history, or history followed
// by whatever arrives next. The body is never stored and never interpreted -- it is the
// application's own output, forwarded as text.
//
// Every bound is explicit and reported when it bites: the request ends at
// protocol.MaxLogLines or protocol.MaxLogBytes with a line saying so, a reader that cannot
// keep up gets a gap marker naming the bytes dropped rather than a log that looks continuous,
// and an endpoint serves only so many streams at once.
func (s *Server) handleContainerLogs(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	q := r.URL.Query()
	search := q.Get("search")
	if len(search) > maxLogSearchBytes {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	since, err := parseSince(q.Get("since"))
	if err != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	tail := defaultLogTail
	if raw := q.Get("tail"); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 1 || n > protocol.MaxLogTail {
			s.tenantError(w, store.ErrInvalid)
			return
		}
		tail = n
	}
	follow := q.Get("follow") == "1"
	download := q.Get("download") == "1"
	if follow && download {
		// A download is a file, and a file has an end. Asking for both says nothing coherent.
		s.tenantError(w, store.ErrInvalid)
		return
	}

	// Authorization, the audit row, and the resolution of the name the operator used into the
	// container the endpoint last reported, all in one place.
	target, err := s.store.Tenancy().OpenLogTarget(r.Context(), a, id, r.PathValue("container"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	stream, ok := s.logs.open(id)
	if !ok {
		s.writeError(w, http.StatusTooManyRequests, "This endpoint already has as many log streams open as it will serve")
		return
	}
	defer s.logs.release(stream)
	request := protocol.LogRequest{
		Stream: stream.id, Container: target.ContainerID, Tail: tail,
		Since: since, Timestamps: q.Get("timestamps") == "1", Follow: follow,
	}
	if !s.agents.deliver(id, envelope(protocol.TypeLogOpen, request)) {
		s.writeError(w, http.StatusConflict, "The endpoint is not connected")
		return
	}
	// The agent stops reading the host when the reader goes, however the reader goes.
	defer s.agents.deliver(id, envelope(protocol.TypeLogCancel, protocol.LogCancel{Stream: stream.id}))

	out := newLogWriter(w, follow, download, target.Name)
	out.head()
	ticker := time.NewTicker(followKeepalive)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			out.keepalive()
		case chunk := <-stream.chunks:
			out.gap(stream.gap())
			if done := out.write(chunk.Data, search); done {
				return
			}
		case <-stream.done:
			// Drain what arrived before the end: a short log finishes before the reader has
			// looked, and losing it would make a container that printed once look silent.
			for {
				select {
				case chunk := <-stream.chunks:
					out.gap(stream.gap())
					if done := out.write(chunk.Data, search); done {
						return
					}
					continue
				default:
				}
				break
			}
			out.gap(stream.gap())
			out.end(stream.end.Load())
			return
		}
	}
}

// parseSince reads either an instant or an age. An operator types "10m" far more often than an
// RFC 3339 timestamp, and a UI sends the timestamp.
func parseSince(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(raw); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("a negative age")
		}
		return time.Now().UTC().Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// logWriter turns chunks into lines and lines into a response, in whichever shape the caller
// asked for, while counting what it has spent against the request's bounds.
type logWriter struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	follow   bool
	download bool
	name     string
	pending  strings.Builder
	lines    int
	bytes    int
	wroteAny bool
}

func newLogWriter(w http.ResponseWriter, follow, download bool, name string) *logWriter {
	f, _ := w.(http.Flusher)
	return &logWriter{w: w, flusher: f, follow: follow, download: download, name: name}
}

func (l *logWriter) head() {
	h := l.w.Header()
	h.Set("Cache-Control", "no-store")
	// Whatever sits between the browser and here must not hold a stream back waiting for a
	// buffer to fill; a log arrives when it arrives.
	h.Set("X-Accel-Buffering", "no")
	switch {
	case l.follow:
		h.Set("Content-Type", "text/event-stream")
	case l.download:
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", downloadName(l.name)))
	default:
		h.Set("Content-Type", "text/plain; charset=utf-8")
	}
	l.w.WriteHeader(http.StatusOK)
	l.flush()
}

// write takes a chunk, emits whatever complete lines it completes, and reports whether the
// request has spent its budget.
func (l *logWriter) write(data, search string) (done bool) {
	for len(data) > 0 {
		cut := strings.IndexByte(data, '\n')
		if cut < 0 {
			l.pending.WriteString(data)
			if l.pending.Len() >= maxLogLineBytes {
				line := l.pending.String()
				l.pending.Reset()
				l.notice(fmt.Sprintf("a line longer than %d bytes was split", maxLogLineBytes))
				return l.emit(line, search)
			}
			return false
		}
		line := data[:cut]
		data = data[cut+1:]
		if l.pending.Len() > 0 {
			l.pending.WriteString(line)
			line = l.pending.String()
			l.pending.Reset()
		}
		if l.emit(strings.TrimSuffix(line, "\r"), search) {
			return true
		}
	}
	return false
}

// emit writes one line if it matches the filter, and reports whether the bounds are spent.
// The bounds count every line read rather than only the ones that matched: what a request may
// cost must not depend on how much of the log happens to be interesting.
func (l *logWriter) emit(line, search string) (done bool) {
	l.lines++
	l.bytes += len(line) + 1
	if search == "" || strings.Contains(strings.ToLower(line), strings.ToLower(search)) {
		l.line(line)
	}
	if l.lines >= protocol.MaxLogLines || l.bytes >= protocol.MaxLogBytes {
		l.notice(fmt.Sprintf("this request reached its limit of %d lines or %d bytes; ask for a narrower window to see more", protocol.MaxLogLines, protocol.MaxLogBytes))
		return true
	}
	return false
}

func (l *logWriter) line(text string) {
	l.wroteAny = true
	if l.follow {
		fmt.Fprintf(l.w, "data: %s\n\n", text)
	} else {
		fmt.Fprintf(l.w, "%s\n", text)
	}
	l.flush()
}

// gap reports bytes that were dropped between what came before and what comes next. A reader
// is told about the hole; it is never shown a log that looks continuous when it is not.
func (l *logWriter) gap(dropped int64) {
	if dropped > 0 {
		l.notice(fmt.Sprintf("%d bytes were dropped here because this reader was not keeping up", dropped))
	}
}

// notice is this server speaking rather than the container. It is marked as such in both
// shapes, so nothing the control plane says can be mistaken for something the application
// printed.
func (l *logWriter) notice(text string) {
	if l.follow {
		fmt.Fprintf(l.w, "event: notice\ndata: %s\n\n", text)
	} else {
		fmt.Fprintf(l.w, "--- kyyard: %s ---\n", text)
	}
	l.flush()
}

func (l *logWriter) keepalive() {
	if l.follow {
		fmt.Fprint(l.w, ": keepalive\n\n")
		l.flush()
	}
}

// end closes the response, flushing a last partial line and saying why the stream stopped.
func (l *logWriter) end(closed *protocol.LogClose) {
	if l.pending.Len() > 0 {
		l.line(l.pending.String())
		l.pending.Reset()
	}
	switch {
	case closed != nil && closed.Failed:
		l.notice("the stream ended: " + closed.Reason)
	case l.follow:
		l.notice("the stream ended")
	}
}

func (l *logWriter) flush() {
	if l.flusher != nil {
		l.flusher.Flush()
	}
}

// downloadName is a filename a browser will accept, built from the container name rather than
// from anything a caller typed.
func downloadName(name string) string {
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, name)
	if safe == "" {
		safe = "container"
	}
	return fmt.Sprintf("%s-%s.log", safe, time.Now().UTC().Format("20060102-150405"))
}
