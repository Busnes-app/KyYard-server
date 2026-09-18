package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// deadlineWriter is a response writer that records the write deadlines a handler sets, which is
// the thing being asserted: http.NewResponseController finds SetWriteDeadline by interface.
type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

func lines(t *testing.T, follow bool, chunks ...string) []string {
	t.Helper()
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	l := newLogWriter(w, follow, false, "web")
	l.head(historyBudget)
	for _, c := range chunks {
		if l.write(c, "") {
			break
		}
	}
	body := w.Body.String()
	if body == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(body, "\n"), "\n")
}

// A log is forwarded as the container wrote it. CRLF is one break; two line feeds are two
// lines, and the blank one between them is part of what the application printed.
func TestLineBreaksArePreservedExactly(t *testing.T) {
	if got := lines(t, false, "a\n\nb\n"); len(got) != 3 || got[0] != "a" || got[1] != "" || got[2] != "b" {
		t.Fatalf("a blank line was lost: %q", got)
	}
	if got := lines(t, false, "a\r\nb\r\n"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("CRLF was not one break: %q", got)
	}
	// The break can fall across a chunk boundary, because chunks are sized in bytes.
	if got := lines(t, false, "a\r", "\nb\n"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("a CRLF split across chunks: %q", got)
	}
	// A bare CR is still a break: the event-stream grammar says so.
	if got := lines(t, false, "a\rb\n"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("a bare CR: %q", got)
	}
}

// A request whose endpoint has gone quiet is still a request this server means to answer. The
// deadline has to be refreshed on the tick, or a silent stall ends as a cut wire with none of
// the markers the reader was promised -- including for a history request, which writes nothing
// between chunks.
func TestTheDeadlineIsRefreshedEvenWithNothingToSend(t *testing.T) {
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	l := newLogWriter(w, false, false, "web")
	l.head(historyBudget)
	opened := len(w.deadlines)
	if opened == 0 {
		t.Fatal("the response was given no deadline of its own")
	}
	l.keepalive()
	if len(w.deadlines) == opened {
		t.Fatal("a quiet tick let the response deadline lapse")
	}
	// And it stays bounded: the request's own budget is the end of it.
	for _, d := range w.deadlines {
		if d.After(time.Now().Add(historyBudget + time.Second)) {
			t.Fatalf("the deadline was pushed past the request's budget: %s", d)
		}
	}
	l.deadline = time.Now().Add(-time.Second)
	if !l.expired() {
		t.Fatal("a request past its budget did not read as expired")
	}
}

var _ http.ResponseWriter = (*deadlineWriter)(nil)
