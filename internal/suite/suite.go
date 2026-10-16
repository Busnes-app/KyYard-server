// Package suite is the client for suite runner protocol v1 (KyQuickStart
// docs/superpowers/specs/2026-10-16-suite-runner-design.md, "Runner protocol v1"): HTTP/1.1 over
// the runner's Unix socket, JSON both ways, unknown fields refused.
package suite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"time"

	"github.com/Busnes-app/ky-primitives/keyfile"
)

const (
	// MaxRequestBytes is the protocol's request cap.
	MaxRequestBytes = 64 << 10
	// maxResponseBytes bounds every answer; the plan's stack.yaml is the largest.
	maxResponseBytes = 1 << 20
	// requestTimeout bounds one exchange. Operations run in the background, so none waits long.
	requestTimeout = 10 * time.Second
	// maxSocketPath is Linux's sun_path less its terminator.
	maxSocketPath = 107
)

var (
	// ErrUnreachable is a failed exchange: the runner may or may not have seen the request.
	ErrUnreachable = errors.New("suite runner unreachable")
	// ErrInvalidResponse is an answer outside the protocol.
	ErrInvalidResponse = errors.New("suite runner answered outside protocol v1")
	// ErrNotFound is a 404: the runner does not know the operation.
	ErrNotFound = errors.New("suite runner does not know the operation")
	// ErrUnsafeSocket is a socket directory another user could control.
	ErrUnsafeSocket = errors.New("suite runner socket directory is not private")
)

// Error is the runner's refusal, or a failed operation's cause.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return "suite runner refused: " + e.Code }

type Runner struct {
	ReleaseSet   string `json:"release_set"`
	RunnerID     string `json:"runner_id"`
	PlanRevision int64  `json:"plan_revision"`
	PlanOwner    string `json:"plan_owner"`
}

type Target struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type Health struct {
	OK      bool      `json:"ok"`
	Checked time.Time `json:"checked"`
}

type Result struct {
	Step   string    `json:"step"`
	Status string    `json:"status"`
	Time   time.Time `json:"time"`
}

type App struct {
	Name       string  `json:"name"`
	Category   string  `json:"category"`
	Version    string  `json:"version"`
	Target     string  `json:"target"`
	State      string  `json:"state"`
	Health     *Health `json:"health"`
	LastResult *Result `json:"last_result"`
}

type Status struct {
	Runner    Runner   `json:"runner"`
	Operation *string  `json:"operation"`
	Targets   []Target `json:"targets"`
	Apps      []App    `json:"apps"`
}

type Upgrade struct {
	MinFrom      string   `json:"min_from"`
	NoSkip       []string `json:"no_skip"`
	MigratesData bool     `json:"migrates_data"`
}

type CatalogApp struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Version     string   `json:"version"`
	DependsOn   []string `json:"depends_on"`
	Installed   bool     `json:"installed"`
	Upgrade     Upgrade  `json:"upgrade"`
}

type Catalog struct {
	ReleaseSet string       `json:"release_set"`
	Apps       []CatalogApp `json:"apps"`
}

type Plan struct {
	Revision int64  `json:"revision"`
	Stack    string `json:"stack"`
}

// Request starts an operation. Target is sent only for install; Apps is always a list.
type Request struct {
	ID                  string   `json:"id"`
	Kind                string   `json:"kind"`
	Apps                []string `json:"apps"`
	Target              string   `json:"target,omitempty"`
	AcknowledgeNoBackup bool     `json:"acknowledge_no_backup"`
	Actor               string   `json:"actor"`
}

type Step struct {
	ID          string    `json:"id"`
	Status      string    `json:"status"`
	Time        time.Time `json:"time"`
	Diagnostics string    `json:"diagnostics"`
}

type Operation struct {
	ID       string     `json:"id"`
	Kind     string     `json:"kind"`
	Apps     []string   `json:"apps"`
	Actor    string     `json:"actor"`
	State    string     `json:"state"`
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished"`
	Steps    []Step     `json:"steps"`
	Error    *Error     `json:"error"`
}

// Kinds and States are the protocol's closed vocabularies.
var (
	Kinds  = []string{"install", "uninstall", "unmanage", "upgrade"}
	States = []string{"running", "succeeded", "failed", "unknown"}
)

// CheckSocketPath refuses a path that could bind outside the directory checked at dial time.
func CheckSocketPath(socket string) error {
	if !filepath.IsAbs(socket) || socket != filepath.Clean(socket) {
		return errors.New("must be an absolute, canonical path")
	}
	if len(socket) > maxSocketPath {
		return fmt.Errorf("exceeds the %d-byte Unix socket limit", maxSocketPath)
	}
	return nil
}

type Client struct {
	socket string
	http   *http.Client
}

// New takes a path CheckSocketPath accepted.
func New(socket string) *Client {
	c := &Client{socket: socket}
	c.http = &http.Client{
		Transport:     &http.Transport{Proxy: nil, DialContext: c.dial, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c
}

// dial rechecks the directory on every connection: access is the file permission, so a
// directory another user could swap would let them stand in for the runner.
func (c *Client) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	dir := filepath.Dir(c.socket)
	if keyfile.RequireTrustedAncestry(dir) != nil || keyfile.RequirePrivateDirectory(dir) != nil {
		return nil, ErrUnsafeSocket
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", c.socket)
}

func (c *Client) Status(ctx context.Context) (*Status, error) {
	var out Status
	return &out, c.do(ctx, http.MethodGet, "/v1/status", nil, http.StatusOK, &out)
}

func (c *Client) Catalog(ctx context.Context) (*Catalog, error) {
	var out Catalog
	return &out, c.do(ctx, http.MethodGet, "/v1/catalog", nil, http.StatusOK, &out)
}

func (c *Client) Plan(ctx context.Context) (*Plan, error) {
	var out Plan
	return &out, c.do(ctx, http.MethodGet, "/v1/plan", nil, http.StatusOK, &out)
}

// Start asks the runner to begin r. Repeating the same request starts nothing new.
func (c *Client) Start(ctx context.Context, r Request) error {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/operations", r, http.StatusAccepted, &out); err != nil {
		return err
	}
	if out.ID != r.ID {
		return ErrInvalidResponse
	}
	return nil
}

func (c *Client) Operation(ctx context.Context, id string) (*Operation, error) {
	var out Operation
	if err := c.do(ctx, http.MethodGet, "/v1/operations/"+id, nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	if out.ID != id || !slices.Contains(Kinds, out.Kind) || !slices.Contains(States, out.State) {
		return nil, ErrInvalidResponse
	}
	return &out, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, want int, out any) error {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil || len(raw) > MaxRequestBytes {
			return errors.New("suite request exceeds the protocol limit")
		}
		payload = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://runner"+path, payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, ErrUnsafeSocket) {
			return ErrUnsafeSocket
		}
		return ErrUnreachable
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		return ErrUnreachable
	}
	if len(raw) > maxResponseBytes {
		return ErrInvalidResponse
	}
	switch res.StatusCode {
	case want:
		return decode(raw, out)
	case http.StatusNotFound:
		return ErrNotFound
	}
	var refusal struct {
		Error *Error `json:"error"`
	}
	if decode(raw, &refusal) != nil || refusal.Error == nil || refusal.Error.Code == "" || res.StatusCode < 400 {
		return ErrInvalidResponse
	}
	refusal.Error.Status = res.StatusCode
	return refusal.Error
}

func decode(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return ErrInvalidResponse
	}
	return nil
}
