package protocol

import (
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Runtime exec bounds implement docs/agent-protocol.md section 7. Wire grants and
// stream admission remain control-plane/agent responsibilities, not runtime policy.
const (
	MaxExecChunkBytes    = 32 << 10
	MaxExecArgs          = 64
	MaxExecArgumentBytes = 8 << 10
	MaxExecDimension     = 512
	ExecIdleTimeout      = 15 * time.Minute
	ExecAbsoluteTimeout  = 8 * time.Hour
)

var (
	fullDockerID = regexp.MustCompile(`^[a-f0-9]{64}$`)
	execUser     = regexp.MustCompile(`^[a-zA-Z0-9_.-]+(?::[a-zA-Z0-9_.-]+)?$`)
)

// ExecSpec carries the exact target approved by the caller: a Docker container and image with
// an explicit user, or a pod container pinned by the pod's UID. Argv is passed directly to the
// runtime: no adapter parses a command line or inserts a shell.
type ExecSpec struct {
	Container string     `json:"container"`
	ImageID   string     `json:"image_id"`
	User      string     `json:"user"`
	Argv      []string   `json:"argv"`
	Pod       *PodTarget `json:"pod,omitempty"`
}

// ValidateFor is Validate holding the target to the runtime: a pod exactly on a cluster.
func (s ExecSpec) ValidateFor(runtime string) error {
	if (s.Pod != nil) != (runtime == RuntimeKubernetes) {
		return errors.New("the exec target does not match the agent's runtime")
	}
	return s.Validate()
}

func (s ExecSpec) Validate() error {
	if s.Pod != nil {
		if s.Container != "" || s.ImageID != "" || s.User != "" || s.Pod.Container == "" || s.Pod.UID == "" || s.Pod.Validate() != nil {
			return errors.New("a pod exec names a namespace, pod, container and pod UID, and no Docker field")
		}
		return validArgv(s.Argv)
	}
	if !fullDockerID.MatchString(s.Container) || !strings.HasPrefix(s.ImageID, "sha256:") || !fullDockerID.MatchString(strings.TrimPrefix(s.ImageID, "sha256:")) {
		return errors.New("exec requires full container and image IDs")
	}
	if len(s.User) == 0 || len(s.User) > 128 || !execUser.MatchString(s.User) {
		return errors.New("exec requires an explicit container user or user:group")
	}
	return validArgv(s.Argv)
}

func validArgv(argv []string) error {
	if len(argv) == 0 || len(argv) > MaxExecArgs || argv[0] == "" {
		return errors.New("exec requires bounded argv with a nonempty executable")
	}
	size := 0
	for _, arg := range argv {
		size += len(arg)
		if strings.ContainsRune(arg, 0) || !utf8.ValidString(arg) || size > MaxExecArgumentBytes {
			return errors.New("exec arguments exceed bounds or contain invalid text")
		}
	}
	return nil
}

// ValidExecID constrains daemon-returned identifiers before they enter another API path.
func ValidExecID(id string) bool { return fullDockerID.MatchString(id) }

type TerminalSize struct {
	Rows    int `json:"rows"`
	Columns int `json:"columns"`
}

func (s TerminalSize) Validate() error {
	if s.Rows < 1 || s.Columns < 1 || s.Rows > MaxExecDimension || s.Columns > MaxExecDimension {
		return errors.New("terminal dimensions must be between 1 and 512")
	}
	return nil
}

// ExitCode is absent until Docker confirms that the process is no longer running.
type ExecStatus struct {
	Running  bool `json:"running"`
	ExitCode *int `json:"exit_code,omitempty"`
}

// Exec grants travel only over the authenticated agent socket. Connection is the
// handshake nonce, so a grant from an old socket cannot start a second process.
const (
	TypeExecOpen              = "exec.open"
	TypeExecReady             = "exec.ready"
	TypeExecInput             = "exec.input"
	TypeExecOutput            = "exec.output"
	TypeExecResize            = "exec.resize"
	TypeExecCancel            = "exec.cancel"
	TypeExecClose             = "exec.close"
	MaxExecStreamsPerEndpoint = 4
	ExecGrantLifetime         = time.Minute
	MaxExecFrameBytes         = 64 << 10
)

type ExecOpen struct {
	Stream     string       `json:"stream"`
	Endpoint   string       `json:"endpoint"`
	Actor      string       `json:"actor"`
	Connection []byte       `json:"connection"`
	Expires    time.Time    `json:"expires"`
	Spec       ExecSpec     `json:"spec"`
	Size       TerminalSize `json:"size"`
}

var execStreamID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

func (r ExecOpen) Validate(now time.Time) error {
	if !execStreamID.MatchString(r.Stream) || !execStreamID.MatchString(r.Actor) || !execStreamID.MatchString(r.Endpoint) || len(r.Connection) != 32 {
		return errors.New("invalid exec scope")
	}
	if !r.Expires.After(now) || r.Expires.After(now.Add(ExecGrantLifetime)) {
		return errors.New("invalid exec grant expiry")
	}
	if err := r.Spec.Validate(); err != nil {
		return err
	}
	return r.Size.Validate()
}

// Data uses JSON base64 encoding to preserve arbitrary PTY bytes, including UTF-8
// split across reads. The decoded chunk limit applies in both directions.
type ExecData struct {
	Stream string `json:"stream"`
	Data   []byte `json:"data"`
}

type ExecResize struct {
	Stream string       `json:"stream"`
	Size   TerminalSize `json:"size"`
}

type ExecStream struct {
	Stream string `json:"stream"`
}

type ExecClose struct {
	Stream   string `json:"stream"`
	Reason   string `json:"reason"`
	ExitCode *int   `json:"exit_code,omitempty"`
}
