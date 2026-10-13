package kubernetes

import (
	"context"
	"testing"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

func TestInstallerOwnershipRefusesWorkloadMutations(t *testing.T) {
	for _, templateOnly := range []bool{false, true} {
		for _, action := range []string{protocol.ActionWorkloadScale, protocol.ActionWorkloadRestart, protocol.ActionWorkloadDelete} {
			d := webDeployment()
			if templateOnly {
				d.Spec.Template.Labels[installerOwnerLabel] = "kyquickstart"
			} else {
				d.Labels[installerOwnerLabel] = "kyquickstart"
			}
			c, cs := workloadCluster(t, false, true, d)
			cmd := command(action, "shop/deployment/web")
			if action == protocol.ActionWorkloadScale {
				cmd.Expects.Replicas = ptr(int32(0))
			}
			outcome, detail := c.Operate(context.Background(), cmd)
			if outcome != protocol.OutcomeDenied || detail != "application_managed" {
				t.Fatalf("template=%t %s: %s %q", templateOnly, action, outcome, detail)
			}
			if got := mutations(cs); len(got) != 0 {
				t.Fatalf("template=%t %s mutated: %v", templateOnly, action, got)
			}
		}
	}
}

func TestInstallerOwnershipRefusesPodExec(t *testing.T) {
	pod := execPod(nil)
	pod.Labels = map[string]string{installerOwnerLabel: "kyquickstart"}
	c, got := execClient(t, newFake(nil), pod)
	if _, err := c.OpenExec(context.Background(), execSpec()); err == nil {
		t.Fatal("installer-owned pod accepted exec")
	}
	if len(got.Command) != 0 {
		t.Fatal("installer-owned pod reached executor")
	}
}
