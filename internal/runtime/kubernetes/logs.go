package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// logFetchBudget and logFollowBudget match the Docker adapter's: history is a read the
	// kubelet already has; a follow ends after an hour and the operator reopens it.
	logFetchBudget  = 60 * time.Second
	logFollowBudget = time.Hour
	// unknownContainer starts every refusal that names no container the pod runs.
	unknownContainer = "unknown_container"
)

// Logs streams one pod container's log to sink, at most protocol.MaxLogLines lines or
// protocol.MaxLogBytes bytes, whichever comes first. The container comes from the pod spec: an
// unnamed one is the pod's only container, and a pod with several is refused rather than
// guessed at.
func (c *Client) Logs(ctx context.Context, req protocol.LogRequest, sink func([]byte) error) error {
	if err := req.ValidateFor(protocol.RuntimeKubernetes); err != nil {
		return err
	}
	budget := logFetchBudget
	if req.Follow {
		budget = logFollowBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	target := *req.Pod
	p, err := c.cs.CoreV1().Pods(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return errors.New("the pod no longer exists")
	}
	if err != nil {
		return errors.New("the cluster did not answer for the pod")
	}
	container, err := podContainer(p.Spec.Containers, target.Container)
	if err != nil {
		return err
	}
	limitBytes := int64(protocol.MaxLogBytes)
	opts := &corev1.PodLogOptions{Container: container, Follow: req.Follow, Timestamps: req.Timestamps, LimitBytes: &limitBytes}
	if req.Tail > 0 {
		tail := int64(req.Tail)
		opts.TailLines = &tail
	}
	if !req.Since.IsZero() {
		since := metav1.NewTime(req.Since)
		opts.SinceTime = &since
	}
	body, err := c.openLog(ctx, target.Namespace, target.Name, opts)
	if err != nil {
		return errors.New("the cluster refused the log request")
	}
	defer body.Close()
	// A read blocked on a quiet follow ends when the reader goes, not at the next line.
	stop := context.AfterFunc(ctx, func() { body.Close() })
	defer stop()
	return copyBounded(ctx, body, sink)
}

func podContainer(containers []corev1.Container, want string) (string, error) {
	if want == "" {
		if len(containers) == 1 {
			return containers[0].Name, nil
		}
		return "", fmt.Errorf("%s: this pod runs %d containers; name one", unknownContainer, len(containers))
	}
	for _, c := range containers {
		if c.Name == want {
			return want, nil
		}
	}
	return "", fmt.Errorf("%s: the pod runs no container named %s", unknownContainer, want)
}

// copyBounded forwards the log in reads of at most one chunk and stops at the request's
// ceiling. A stream that ends, or is ended by the reader, is not a failure.
func copyBounded(ctx context.Context, body io.Reader, sink func([]byte) error) error {
	buf := make([]byte, protocol.MaxLogChunkBytes)
	lines, total := 0, 0
	for {
		n, err := body.Read(buf)
		if n > 0 {
			chunk := buf[:min(n, protocol.MaxLogBytes-total)]
			for i, b := range chunk {
				if b == '\n' {
					if lines++; lines == protocol.MaxLogLines {
						chunk = chunk[:i+1]
						break
					}
				}
			}
			total += len(chunk)
			if serr := sink(chunk); serr != nil {
				return serr
			}
			if total >= protocol.MaxLogBytes || lines >= protocol.MaxLogLines {
				return nil
			}
		}
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			return errors.New("the log stream broke")
		}
	}
}
