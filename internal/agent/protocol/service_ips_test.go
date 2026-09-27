package protocol

import (
	"testing"
	"time"
)

func TestStaticServiceIPBoundary(t *testing.T) {
	now := time.Now()
	for _, ip := range []string{"10.96.0.40", "fd00::40"} {
		r := goodKubernetesDeployment(now)
		r.Services[0].ClusterIP = ip
		if err := r.Validate(now); err != nil {
			t.Fatal(err)
		}
		r.Services[1].ClusterIP = ip
		if r.Validate(now) == nil {
			t.Fatal("duplicate accepted")
		}
		r.Services[1].ClusterIP = ""
		r.Services[0].Ports = nil
		if r.Validate(now) == nil {
			t.Fatal("IP without Service accepted")
		}
	}
	for _, ip := range []string{"None", "10.0.0.1/24", "010.0.0.1", "127.0.0.1", "0.0.0.0", "224.0.0.1", "::", "::1", "::ffff:10.0.0.1", "fe80::1%eth0", "fd00::AB", " 10.0.0.1", "10.0.0.1:80"} {
		r := goodKubernetesDeployment(now)
		r.Services[0].ClusterIP = ip
		if r.Validate(now) == nil {
			t.Errorf("accepted %q", ip)
		}
	}
	r := goodDeployment(now)
	r.Services[0].ClusterIP = "10.96.0.40"
	if r.Validate(now) == nil {
		t.Fatal("Docker accepted a cluster address")
	}
}
