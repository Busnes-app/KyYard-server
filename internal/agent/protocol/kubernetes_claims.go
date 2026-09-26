package protocol

import (
	"errors"
	"regexp"
	"strconv"
)

// Claims a cluster deployment mounts: a PersistentVolumeClaim per chosen named volume, created
// once and never updated or deleted by the agent. See docs/agent-protocol.md, Claims.
const (
	MaxKubernetesClaims = 16 // per request
	MaxKubernetesMounts = 8  // per service
	MaxStorageClasses   = 100
	AccessReadWriteOnce = "ReadWriteOnce"
	// DetailRetained is the detail of a removal's skipped volume step: the claim was kept.
	DetailRetained = "retained"
)

// KubernetesClaim is one PersistentVolumeClaim of the instance: its name, the StorageClass
// ("" is the cluster default), the requested size and the access mode.
type KubernetesClaim struct {
	Name         string `json:"name"`
	StorageClass string `json:"storage_class"`
	Size         string `json:"size"`
	AccessMode   string `json:"access_mode"`
}

// KubernetesMount mounts a claim the request names at MountPath.
type KubernetesMount struct {
	Claim     string `json:"claim"`
	MountPath string `json:"mount_path"`
	ReadOnly  bool   `json:"read_only,omitempty"`
}

// StorageClass is a cluster StorageClass as the inventory reports it; Default is the
// is-default-class annotation.
type StorageClass struct {
	Name    string `json:"name"`
	Default bool   `json:"default"`
}

var storageSize = regexp.MustCompile(`^([1-9][0-9]{0,5})(Mi|Gi|Ti)$`)

// StorageSizeBytes reads a claim size: a whole number of Mi, Gi or Ti from 1Mi to 16Ti, the one
// quantity form KyYard writes. ok is false for anything else.
func StorageSizeBytes(s string) (int64, bool) {
	m := storageSize.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	shift := map[string]uint{"Mi": 20, "Gi": 30, "Ti": 40}[m[2]]
	bytes := n << shift
	return bytes, bytes <= 16<<40
}

// ValidStorageClass is "" (the cluster default) or a DNS-1123 subdomain.
func ValidStorageClass(s string) bool { return s == "" || ValidDNSSubdomain(s) }

func (c KubernetesClaim) valid() bool {
	_, size := StorageSizeBytes(c.Size)
	return ValidDNSLabel(c.Name) && ValidStorageClass(c.StorageClass) && size && c.AccessMode == AccessReadWriteOnce
}

// validClaims holds a cluster frame's claims to its services' mounts: at most
// MaxKubernetesClaims distinct claims, each mounted by exactly one service (ReadWriteOnce cannot
// serve two pods), at most MaxKubernetesMounts per service at distinct clean paths.
func (r DeploymentRequest) validClaims() error {
	claims := r.Kubernetes.Claims
	if len(claims) > MaxKubernetesClaims {
		return errors.New("too many claims")
	}
	mountedBy := map[string]string{}
	for _, c := range claims {
		if !c.valid() {
			return errors.New("invalid claim")
		}
		if _, dup := mountedBy[c.Name]; dup {
			return errors.New("duplicate claim")
		}
		mountedBy[c.Name] = ""
	}
	for _, s := range r.Services {
		if len(s.Volumes) > MaxKubernetesMounts {
			return errors.New("too many claim mounts")
		}
		paths := map[string]bool{}
		for _, m := range s.Volumes {
			by, known := mountedBy[m.Claim]
			if !known || (by != "" && by != s.Name) || !cleanAbsolute(m.MountPath) || paths[m.MountPath] {
				return errors.New("invalid claim mount")
			}
			mountedBy[m.Claim], paths[m.MountPath] = s.Name, true
		}
	}
	for _, by := range mountedBy {
		if by == "" {
			return errors.New("a claim no service mounts")
		}
	}
	return nil
}
