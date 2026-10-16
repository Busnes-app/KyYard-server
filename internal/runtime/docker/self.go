package docker

import (
	"errors"
	"os"
	"regexp"
)

// ErrSelfProtected refuses an action on the container this agent runs in.
var ErrSelfProtected = errors.New("self_protected")

// ownMount is the source Docker bind-mounts as a container's own /etc/hostname, hosts and
// resolv.conf, which names the container's ID.
var ownMount = regexp.MustCompile(`/containers/([0-9a-f]{64})/(?:hostname|hosts|resolv\.conf) `)

// OwnContainerID is the ID of the Docker container this process runs in, read from
// /proc/self/mountinfo; "" outside one.
func OwnContainerID() string {
	b, _ := os.ReadFile("/proc/self/mountinfo")
	return ownContainerID(b)
}

func ownContainerID(mountinfo []byte) string {
	if m := ownMount.FindSubmatch(mountinfo); m != nil {
		return string(m[1])
	}
	return ""
}

// Protect makes the client refuse exec, configuration reads, lifecycle actions, replacement and
// removal of container id (a full ID; "" protects none). Call it before use.
func (c *Client) Protect(id string) { c.self = id }

func (c *Client) isSelf(id string) bool { return c.self != "" && id == c.self }
