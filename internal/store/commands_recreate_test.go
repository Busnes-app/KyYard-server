package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
)

// Migration 38 gives endpoint_commands a result column bounded like deployments.result.
func TestCommandResultColumnMigration(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	var name string
	if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT name FROM schema_migrations WHERE version=?`), 38).Scan(&name); err != nil || name != "endpoint_command_result" {
		t.Fatalf("migration 38: %q %v", name, err)
	}
	endpointID := activeEndpointWith(t, st.Tenancy(), a, nil, nil)
	insert := func(id, result string) error {
		_, err := st.db.ExecContext(ctx, st.rebind(`INSERT INTO endpoint_commands (id,endpoint_id,organization_id,environment_id,actor_id,request_id,action,container_id,expects,deadline,created_at,result) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`),
			id, endpointID, a.OrganizationID, a.EnvironmentID, a.ActorID, "r", ActionRun, "", "{}", time.Now().UTC(), time.Now().UTC(), result)
		return err
	}
	if err := insert("fits", strings.Repeat("x", MaxDeploymentResultStoredBytes)); err != nil {
		t.Fatalf("a result at the bound: %v", err)
	}
	if err := insert("oversize", strings.Repeat("x", MaxDeploymentResultStoredBytes+1)); err == nil {
		t.Fatal("an oversize result was stored")
	}
	// A row with no result reads as having none.
	if err := insert("empty", ""); err != nil {
		t.Fatal(err)
	}
	if cmd, err := st.Tenancy().ReadCommand(ctx, a, endpointID, "empty"); err != nil || cmd.Result != nil {
		t.Fatalf("empty result: %+v %v", cmd, err)
	}
}

// Only the endpoint that ran a direct command settles it, by its deployment and request IDs, and
// a real answer replaces the unknown a dropped socket left.
func TestSettleDirectCommand(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	endpointID := activeEndpointWith(t, st.Tenancy(), a, nil, nil)
	id, request := "0b7f4c1e-8a57-4c1c-9d0e-3b8f0a1c2d3e", "request-1"
	_, err := st.db.ExecContext(ctx, st.rebind(`INSERT INTO endpoint_commands (id,endpoint_id,organization_id,environment_id,actor_id,request_id,action,container_id,expects,deadline,created_at,dispatched_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`),
		id, endpointID, a.OrganizationID, a.EnvironmentID, a.ActorID, request, ActionRun, "", "{}", time.Now().UTC(), time.Now().UTC(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	res := protocol.DeploymentResult{Deployment: id, RequestID: request, Outcome: protocol.OutcomeFailed, Code: protocol.ResultStepFailed,
		Steps: []protocol.DeploymentStep{{Service: "direct", Step: protocol.StepCreate, Outcome: protocol.OutcomeFailed, Code: "runtime_error"}}, Services: []protocol.DeploymentIdentity{}}
	other := res
	other.RequestID = "request-2"
	if err := st.Tenancy().SettleDirectCommand(ctx, endpointID, other); err != ErrNotFound {
		t.Fatalf("another request's result: %v", err)
	}
	if err := st.Tenancy().SettleDirectCommand(ctx, "ep_other", res); err != ErrNotFound {
		t.Fatalf("another endpoint's result: %v", err)
	}
	if _, err := st.Tenancy().AbandonCommands(ctx, endpointID); err != nil {
		t.Fatal(err)
	}
	if err := st.Tenancy().SettleDirectCommand(ctx, endpointID, res); err != nil {
		t.Fatal(err)
	}
	cmd, err := st.Tenancy().ReadCommand(ctx, a, endpointID, id)
	if err != nil || cmd.Outcome != protocol.OutcomeFailed || cmd.Detail != protocol.ResultStepFailed || cmd.Result == nil || len(cmd.Result.Steps) != 1 {
		t.Fatalf("settled: %+v %v", cmd, err)
	}
	// The first real answer wins.
	res.Outcome, res.Code = protocol.OutcomeSucceeded, ""
	res.Steps = []protocol.DeploymentStep{}
	if err := st.Tenancy().SettleDirectCommand(ctx, endpointID, res); err != ErrNotFound {
		t.Fatalf("a second answer: %v", err)
	}
}

// directRun is a minimal valid run frame for image on a host whose inventory lists it.
func directRun(name, image string) DirectCommand {
	return DirectCommand{Action: ActionRun, Confirm: name, MaxFrameBytes: protocol.MaxDeploymentRequestBytes, Frame: protocol.DeploymentRequest{Services: []protocol.DeploymentService{{
		Name: "direct", ContainerName: name, ImageID: image, Ports: []protocol.Port{}, Env: map[string]string{}, Mounts: []protocol.Mount{}, Explicit: &protocol.ExplicitService{},
	}}}}
}

// An unsettled direct command past its deadline no longer blocks the endpoint: creating the next
// one settles it unknown, and a late real answer may still replace that.
func TestCreateDirectCommandSweepsAnExpiredOne(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	image := "sha256:" + strings.Repeat("b", 64)
	endpointID := activeEndpointWith(t, st.Tenancy(), a, nil, []protocol.Image{{ID: image, Tags: []string{}, Digests: []string{}}})
	first, _, err := st.Tenancy().CreateDirectCommand(ctx, a, endpointID, directRun("one", image))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Tenancy().CreateDirectCommand(ctx, a, endpointID, directRun("two", image)); err != ErrCommandInProgress {
		t.Fatalf("while the first is live: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, st.rebind(`UPDATE endpoint_commands SET deadline=? WHERE id=?`), time.Now().UTC().Add(-time.Minute), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Tenancy().CreateDirectCommand(ctx, a, endpointID, directRun("two", image)); err != nil {
		t.Fatalf("after the deadline: %v", err)
	}
	if cmd, err := st.Tenancy().ReadCommand(ctx, a, endpointID, first.ID); err != nil || cmd.Outcome != protocol.OutcomeUnknown {
		t.Fatalf("expired command: %+v %v", cmd, err)
	}
	// The sweep audits the outcome like any other settle: one row under the first command's
	// correlation ID.
	rows, _, err := st.Audit().ListAuditRecords(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var swept []*AuditRecord
	for _, r := range rows {
		if r.CorrelationID == first.RequestID && strings.HasPrefix(r.Details, "code=") {
			swept = append(swept, r)
		}
	}
	if len(swept) != 1 || swept[0].Action != ActionRun || swept[0].Result != "unknown" || swept[0].Details != "code=deadline new=-" || swept[0].Resource != endpointID+"/-" || swept[0].UserID != a.ActorID {
		for _, r := range swept {
			t.Logf("row: %+v", r)
		}
		t.Fatalf("want one deadline outcome row, got %d", len(swept))
	}
}

// One writer settles a direct command: a plain command.result cannot, a dispatch failure goes
// through FailDirectCommand, and a result must speak only of the direct service.
func TestDirectCommandsHaveOneWriter(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	image := "sha256:" + strings.Repeat("b", 64)
	endpointID := activeEndpointWith(t, st.Tenancy(), a, nil, []protocol.Image{{ID: image, Tags: []string{}, Digests: []string{}}})
	cmd, req, err := st.Tenancy().CreateDirectCommand(ctx, a, endpointID, directRun("one", image))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Tenancy().SettleCommand(ctx, endpointID, cmd.ID, protocol.OutcomeSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	read := func() *Command {
		t.Helper()
		c, err := st.Tenancy().ReadCommand(ctx, a, endpointID, cmd.ID)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := read(); c.Outcome != "" {
		t.Fatalf("a command.result settled a direct command: %+v", c)
	}
	res := protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Outcome: protocol.OutcomeSucceeded,
		Steps:    []protocol.DeploymentStep{{Service: "other", Step: protocol.StepCreate, Outcome: protocol.OutcomeSucceeded}},
		Services: []protocol.DeploymentIdentity{}}
	if err := st.Tenancy().SettleDirectCommand(ctx, endpointID, res); err != ErrUnreadableResult {
		t.Fatalf("a step for another service: %v", err)
	}
	identity := protocol.DeploymentIdentity{Service: "direct", ContainerID: strings.Repeat("e", 64), ImageID: image, CreatedUnix: time.Now().Unix()}
	res.Steps = []protocol.DeploymentStep{}
	res.Services = []protocol.DeploymentIdentity{identity, identity}
	if err := st.Tenancy().SettleDirectCommand(ctx, endpointID, res); err != ErrUnreadableResult {
		t.Fatalf("two identities: %v", err)
	}
	res.Services = []protocol.DeploymentIdentity{identity}
	if err := st.Tenancy().SettleDirectCommand(ctx, endpointID, res); err != nil {
		t.Fatal(err)
	}
	if c := read(); c.Outcome != protocol.OutcomeSucceeded || c.ResultContainerID != identity.ContainerID || c.ContainerID != "" {
		t.Fatalf("settled: %+v", c)
	}
	// The new container's Activity lists the run that made it.
	list, err := st.Tenancy().ListCommands(ctx, a, endpointID, identity.ContainerID, 0)
	if err != nil || len(list) != 1 || list[0].ID != cmd.ID {
		t.Fatalf("by new container: %+v %v", list, err)
	}
	next, _, err := st.Tenancy().CreateDirectCommand(ctx, a, endpointID, directRun("two", image))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Tenancy().FailDirectCommand(ctx, endpointID, next.ID, "deployment_not_sent"); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.Tenancy().ReadCommand(ctx, a, endpointID, next.ID); c == nil || c.Outcome != protocol.OutcomeFailed || c.Detail != "deployment_not_sent" {
		t.Fatalf("unsent: %+v", c)
	}
}

// I7: a direct command's outcome is audited under its correlation ID, for its actor.
func TestDirectCommandOutcomesAreAudited(t *testing.T) {
	st, a := tenantAtomicStore(t)
	ctx := context.Background()
	image := "sha256:" + strings.Repeat("b", 64)
	endpointID := activeEndpointWith(t, st.Tenancy(), a, nil, []protocol.Image{{ID: image, Tags: []string{}, Digests: []string{}}})
	outcome := func(correlation string) *AuditRecord {
		t.Helper()
		rows, _, err := st.Audit().ListAuditRecords(ctx, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		var found *AuditRecord
		for _, r := range rows {
			if r.CorrelationID == correlation && strings.HasPrefix(r.Details, "code=") {
				if found != nil {
					t.Fatalf("two outcome rows: %+v %+v", found, r)
				}
				found = r
			}
		}
		if found == nil {
			t.Fatalf("no outcome row for %s", correlation)
		}
		return found
	}
	a.CorrelationID = "direct-one"
	cmd, req, err := st.Tenancy().CreateDirectCommand(ctx, a, endpointID, directRun("one", image))
	if err != nil {
		t.Fatal(err)
	}
	created := strings.Repeat("e", 64)
	res := protocol.DeploymentResult{Deployment: req.Deployment, RequestID: req.RequestID, Outcome: protocol.OutcomeSucceeded, Steps: []protocol.DeploymentStep{},
		Services: []protocol.DeploymentIdentity{{Service: "direct", ContainerID: created, ImageID: image, CreatedUnix: time.Now().Unix()}}}
	if err := st.Tenancy().SettleDirectCommand(ctx, endpointID, res); err != nil {
		t.Fatal(err)
	}
	r := outcome(cmd.RequestID)
	if r.Action != ActionRun || r.UserID != a.ActorID || r.Result != "success" || r.Resource != endpointID+"/"+created || r.Details != "code=- new="+created || r.OrganizationID != a.OrganizationID {
		t.Fatalf("settled row: %+v", r)
	}
	a.CorrelationID = "direct-two"
	next, _, err := st.Tenancy().CreateDirectCommand(ctx, a, endpointID, directRun("two", image))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Tenancy().FailDirectCommand(ctx, endpointID, next.ID, "deployment_not_sent"); err != nil {
		t.Fatal(err)
	}
	if r := outcome(next.RequestID); r.Action != ActionRun || r.Result != "failure" || r.Resource != endpointID+"/-" || r.Details != "code=deployment_not_sent new=-" {
		t.Fatalf("failed row: %+v", r)
	}
	// A repeat changes nothing and writes no second row.
	if err := st.Tenancy().FailDirectCommand(ctx, endpointID, next.ID, "deployment_not_sent"); err != nil {
		t.Fatal(err)
	}
	outcome(next.RequestID)
}

// I6: one bind rule with the agent: same source and target, and no read-only bind made writable.
func TestDirectBlockersBindRule(t *testing.T) {
	old := &protocol.Container{ID: "old", Mounts: []protocol.Mount{{Kind: protocol.MountBind, Source: "/srv", Target: "/data"}, {Kind: protocol.MountBind, Source: "/ro", Target: "/ro", ReadOnly: true}}}
	for name, c := range map[string]struct {
		m  protocol.Mount
		ok bool
	}{
		"same":       {protocol.Mount{Kind: protocol.MountBind, Source: "/srv", Target: "/data"}, true},
		"tightened":  {protocol.Mount{Kind: protocol.MountBind, Source: "/srv", Target: "/data", ReadOnly: true}, true},
		"kept ro":    {protocol.Mount{Kind: protocol.MountBind, Source: "/ro", Target: "/ro", ReadOnly: true}, true},
		"loosened":   {protocol.Mount{Kind: protocol.MountBind, Source: "/ro", Target: "/ro"}, false},
		"moved":      {protocol.Mount{Kind: protocol.MountBind, Source: "/srv", Target: "/elsewhere"}, false},
		"new source": {protocol.Mount{Kind: protocol.MountBind, Source: "/etc", Target: "/data"}, false},
	} {
		svc := &protocol.DeploymentService{ContainerName: "web", Mounts: []protocol.Mount{c.m}, Explicit: &protocol.ExplicitService{}}
		if err := directBlockers(protocol.Snapshot{Containers: []protocol.Container{*old}}, old, svc, false); (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// I8: each host-level setting is refused without the opt-in and accepted with it.
func TestDirectBlockersHostLevel(t *testing.T) {
	bind := func(src string) func(*protocol.DeploymentService) {
		return func(s *protocol.DeploymentService) {
			s.Mounts = []protocol.Mount{{Kind: protocol.MountBind, Source: src, Target: "/mnt"}}
			s.Explicit.AcknowledgedBinds = []string{src}
		}
	}
	for name, c := range map[string]struct {
		set      func(*protocol.DeploymentService)
		hostWide bool
	}{
		"plain":      {func(*protocol.DeploymentService) {}, false},
		"privileged": {func(s *protocol.DeploymentService) { s.Explicit.Privileged = true }, true},
		"devices": {func(s *protocol.DeploymentService) {
			s.Explicit.Devices = []protocol.Device{{Host: "/dev/fuse", Container: "/dev/fuse"}}
		}, true},
		"security_opt": {func(s *protocol.DeploymentService) { s.Explicit.SecurityOpt = []string{"seccomp=unconfined"} }, true},
		"no-new-privileges": {func(s *protocol.DeploymentService) {
			s.Explicit.SecurityOpt = []string{"no-new-privileges", "no-new-privileges:true"}
		}, false},
		"no-new-privileges off": {func(s *protocol.DeploymentService) { s.Explicit.SecurityOpt = []string{"no-new-privileges:false"} }, true},
		"hardening beside another": {func(s *protocol.DeploymentService) {
			s.Explicit.SecurityOpt = []string{"no-new-privileges", "apparmor=unconfined"}
		}, true},
		"default caps": {func(s *protocol.DeploymentService) {
			s.Explicit.CapAdd = []string{"CHOWN", "cap_net_bind_service", "CAP_KILL"}
		}, false},
		"cap beyond":        {func(s *protocol.DeploymentService) { s.Explicit.CapAdd = []string{"NET_ADMIN"} }, true},
		"host network":      {func(s *protocol.DeploymentService) { s.Explicit.NetworkMode = "host" }, true},
		"container network": {func(s *protocol.DeploymentService) { s.Explicit.NetworkMode = "container:db" }, true},
		"bridge":            {func(s *protocol.DeploymentService) { s.Explicit.NetworkMode = "bridge" }, false},
		"host attachment": {func(s *protocol.DeploymentService) {
			s.Explicit.Networks = []protocol.NetworkAttachmentSpec{{Name: "host"}}
		}, true},
		"container attachment": {func(s *protocol.DeploymentService) {
			s.Explicit.NetworkMode, s.Explicit.Networks = "bridge", []protocol.NetworkAttachmentSpec{{Name: "bridge"}, {Name: "container:db"}}
		}, true},
		"user network attachment": {func(s *protocol.DeploymentService) {
			s.Explicit.Networks = []protocol.NetworkAttachmentSpec{{Name: "hostile"}}
		}, false},
		"bind root":                    {bind("/"), true},
		"bind /etc":                    {bind("/etc"), true},
		"bind /etc/localtime":          {bind("/etc/localtime"), false},
		"bind /var":                    {bind("/var"), true},
		"bind /var/run":                {bind("/var/run"), true},
		"bind /run/docker.sock":        {bind("/run/docker.sock"), true},
		"bind /proc/1/root":            {bind("/proc/1/root"), true},
		"bind /root/.ssh":              {bind("/root/.ssh"), true},
		"bind /var/lib/docker/volumes": {bind("/var/lib/docker/volumes"), true},
		"bind /srv/data":               {bind("/srv/data"), false},
		"bind /var/lib/app":            {bind("/var/lib/app"), true},
		"bind /var/lib":                {bind("/var/lib"), true},
		"bind /run":                    {bind("/run"), true},
		"bind /run/containerd":         {bind("/run/containerd/containerd.sock"), true},
		"bind /var/run/crio":           {bind("/var/run/crio/crio.sock"), true},
		"bind /var/spool/cron":         {bind("/var/spool/cron"), true},
		"bind /usr/local/bin":          {bind("/usr/local/bin"), true},
		"bind /lib/modules":            {bind("/lib/modules"), true},
		"bind /lib64":                  {bind("/lib64"), true},
		"bind /bin":                    {bind("/bin"), true},
		"bind /sbin/init":              {bind("/sbin/init"), true},
		"bind /var/log/app":            {bind("/var/log/app"), false},
		"bind /library":                {bind("/library"), false},
		"bind /home/me/app":            {bind("/home/me/app"), false},
		"bind /devices":                {bind("/devices"), false},
	} {
		for _, allow := range []bool{false, true} {
			svc := &protocol.DeploymentService{ContainerName: "web", Explicit: &protocol.ExplicitService{}}
			c.set(svc)
			err := directBlockers(protocol.Snapshot{}, nil, svc, allow)
			if refused := err != nil; refused != (c.hostWide && !allow) {
				t.Errorf("%s allow=%v: %v", name, allow, err)
			}
			if e, ok := err.(*InvalidSpecError); err != nil && (!ok || len(e.Blockers) != 1 || e.Blockers[0] != "privileged_disabled") {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
}
