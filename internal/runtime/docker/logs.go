package docker

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

const (
	// logFetchBudget bounds a request for history, including time spent waiting on the session
	// queue: a large history over a slow uplink can reach it and end as a failed close.
	logFetchBudget = 60 * time.Second
	// logFollowBudget is the absolute life of a following stream. A browser tab left open
	// over a weekend must not hold a reader on the host forever; the operator reopens it.
	logFollowBudget = time.Hour
	// dockerFrameLimit bounds one multiplexed frame. The daemon writes small frames; a header
	// claiming more than this is a stream this code will not trust.
	dockerFrameLimit = 1 << 20
)

// Logs streams one container's log to sink. The caller decides when enough is enough -- a sink
// that returns an error ends the stream -- so the byte and line bounds live with the reader
// who is counting them, not here.
//
// The container is addressed by ID, escaped like every other identifier that becomes part of a
// URL on the host's root-equivalent socket.
func (c *Client) Logs(ctx context.Context, req protocol.LogRequest, sink func([]byte) error) error {
	if !protocol.ValidContainerID(req.Container) {
		return fmt.Errorf("that is not a container identifier")
	}
	budget := logFetchBudget
	if req.Follow {
		budget = logFollowBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	// A container with a TTY writes its log raw; without one the daemon multiplexes stdout and
	// stderr with an 8-byte header per frame. Which it is, is a property of the container.
	var inspected struct {
		Config struct {
			Tty bool `json:"Tty"`
		} `json:"Config"`
	}
	escaped := url.PathEscape(req.Container)
	if err := c.get(ctx, "/containers/"+escaped+"/json", &inspected); err != nil {
		if statusOf(err) == http.StatusNotFound {
			return fmt.Errorf("the container no longer exists")
		}
		return fmt.Errorf("inspecting the container: %w", err)
	}

	q := url.Values{"stdout": {"1"}, "stderr": {"1"}}
	q.Set("tail", strconv.Itoa(req.Tail))
	if req.Timestamps {
		q.Set("timestamps", "1")
	}
	if req.Follow {
		q.Set("follow", "1")
	}
	if !req.Since.IsZero() {
		q.Set("since", strconv.FormatInt(req.Since.Unix(), 10))
	}
	status, body, err := c.stream(ctx, http.MethodGet, "/containers/"+escaped+"/logs?"+q.Encode())
	if body != nil {
		defer body.Close()
	}
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("the runtime refused with status %d", status)
	}
	if inspected.Config.Tty {
		return copyRaw(body, sink)
	}
	return copyDemuxed(body, sink)
}

// copyRaw forwards a TTY container's log, which carries no framing.
func copyRaw(body io.Reader, sink func([]byte) error) error {
	buf := make([]byte, protocol.MaxLogChunkBytes)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if sinkErr := sink(buf[:n]); sinkErr != nil {
				return sinkErr
			}
		}
		if err != nil {
			return endOfStream(err)
		}
	}
}

// copyDemuxed forwards the payloads of Docker's multiplexed stream. stdout and stderr are
// both the container's output as far as an operator reading a log is concerned, so they are
// interleaved in the order the daemon wrote them rather than separated.
//
// The daemon writes a frame per line, so payloads that have already arrived are gathered into
// one chunk of up to protocol.MaxLogChunkBytes. Whatever is gathered is handed on before any
// read that would wait, so a followed line is never held back for the next one.
func copyDemuxed(body io.Reader, sink func([]byte) error) error {
	r := bufio.NewReaderSize(body, protocol.MaxLogChunkBytes)
	header := make([]byte, 8)
	chunk := make([]byte, 0, protocol.MaxLogChunkBytes)
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		err := sink(chunk)
		chunk = chunk[:0]
		return err
	}
	for {
		// Flush before any read that would wait on the daemon.
		if r.Buffered() < len(header) {
			if err := flush(); err != nil {
				return err
			}
		}
		if _, err := io.ReadFull(r, header); err != nil {
			return finish(flush, err)
		}
		size := binary.BigEndian.Uint32(header[4:])
		if size > dockerFrameLimit {
			return fmt.Errorf("the runtime announced a %d byte log frame", size)
		}
		for remaining := int(size); remaining > 0; {
			take := min(remaining, cap(chunk)-len(chunk))
			if take == 0 || r.Buffered() < take {
				if err := flush(); err != nil {
					return err
				}
				take = min(remaining, cap(chunk))
			}
			n, err := io.ReadFull(r, chunk[len(chunk):len(chunk)+take])
			chunk = chunk[:len(chunk)+n]
			remaining -= n
			if err != nil {
				return finish(flush, err)
			}
		}
	}
}

// finish hands on what was gathered before reporting how the stream ended.
func finish(flush func() error, err error) error {
	if ferr := flush(); ferr != nil {
		return ferr
	}
	return endOfStream(err)
}

// endOfStream turns the end of the body into success: a log that ends is not a failure, and
// telling an operator otherwise would make every finished container look broken.
func endOfStream(err error) error {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return nil
	}
	return err
}
