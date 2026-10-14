package docker

import (
	"strings"
	"testing"
)

func TestOwnContainerIDFromMountinfo(t *testing.T) {
	id := strings.Repeat("ab", 32)
	for name, c := range map[string]struct{ mountinfo, want string }{
		"docker": {"1 0 0:1 / / rw - overlay overlay rw\n" +
			"2 1 8:1 /var/lib/docker/containers/" + id + "/resolv.conf /etc/resolv.conf rw - ext4 /dev/sda1 rw\n", id},
		"own filesystem": {"3 1 8:2 /containers/" + id + "/hostname /etc/hostname rw - xfs /dev/sdb1 rw\n", id},
		"host":           {"1 0 0:1 / / rw - ext4 /dev/sda1 rw\n", ""},
		"short id":       {"2 1 8:1 /var/lib/docker/containers/abc/hostname /etc/hostname rw - ext4 /dev/sda1 rw\n", ""},
	} {
		if got := ownContainerID([]byte(c.mountinfo)); got != c.want {
			t.Errorf("%s: %q", name, got)
		}
	}
}
