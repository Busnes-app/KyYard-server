package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/google/uuid"
)

func mustExec(t *testing.T, st *SQLStore, query string, args ...any) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(), st.rebind(query), args...); err != nil {
		t.Fatal(err)
	}
}

// updatedContainer is the fixture's updated web container as the inventory reports it.
func updatedContainer(updated *Deployment) protocol.Container {
	return webContainer(updatedID, imageX, time.Unix(updated.Result.Services[0].CreatedUnix, 0).UTC())
}

// oldContainer is a container other than the application's running image on the host.
func oldContainer(state string) protocol.Container {
	return protocol.Container{ID: strings.Repeat("5", 64), Name: "old", ImageID: imageD, State: state, CreatedAt: time.Now().UTC(), Mounts: []protocol.Mount{}}
}

// setPlan rewrites the deployment's stored plan.
func setPlan(t *testing.T, st *SQLStore, d *Deployment, edit func(*DeploymentPlan)) {
	t.Helper()
	plan := d.Plan
	plan.Services = slices.Clone(plan.Services)
	edit(&plan)
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, st, `UPDATE deployments SET plan=? WHERE id=?`, string(raw), d.ID)
}

// The target is the validated deployment's own plan: each service returns to the image its
// Replaces identity ran, at the revision the deployment replaced. A policy update re-applies
// revision 1, so that revision is 1 too; the prior deployment row is not consulted.
func TestRollbackTargetReturnsTheReplacedImages(t *testing.T) {
	st, a, app, endpoint, _, _, prior, updated := validationFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	if updated.Plan.Services[0].Replaces.ImageID != imageD {
		t.Fatalf("fixture: %+v", updated.Plan.Services[0].Replaces)
	}
	want := func(label string) {
		t.Helper()
		rb, reason, err := ts.RollbackTarget(ctx, a, app.ID, updated.ID)
		if err != nil || reason != "" || rb == nil || rb.Revision != 1 || len(rb.Images) != 1 || rb.Images["web"] != imageD || rb.Project != "shop" || rb.InstanceID != updated.InstanceID || rb.MappingVersion != updated.MappingVersion {
			t.Fatalf("%s: %+v %q %v", label, rb, reason, err)
		}
	}
	want("target")
	mustExec(t, st, `DELETE FROM deployments WHERE id=?`, prior.ID)
	want("without the prior deployment row")
	// A running container's image is on the host even when the image list omits it.
	putInventory(t, st, endpoint, []protocol.Container{updatedContainer(updated), oldContainer("running")}, []protocol.Image{tagged(imageX, "nginx:1")})
	want("image under a running container")
}

func TestRollbackTargetIneligibility(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, endpoint string, prior, updated *Deployment)
		want  string
	}{
		{"already rolled back", func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, _ string, _, updated *Deployment) {
			if _, err := st.Tenancy().FinishValidation(ctx, updated.ID, VerdictUnhealthy, "web"); err != nil {
				t.Fatal(err)
			}
			if err := st.Tenancy().MarkRollbackPlanned(ctx, updated.ID, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
		}, RollbackAlreadyRolledBack},
		{"another rollback still validating", func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, _ string, prior, _ *Deployment) {
			mustExec(t, st, `UPDATE deployment_validations SET is_rollback=1,phase='observing' WHERE deployment_id=?`, prior.ID)
		}, RollbackInFlight},
		{"another validation's rollback still planned", func(t *testing.T, st *SQLStore, a TenantAccess, app *Application, _ string, prior, _ *Deployment) {
			m, err := st.Tenancy().ReadApplicationMapping(ctx, a, app.ID)
			if err != nil {
				t.Fatal(err)
			}
			back, err := st.Tenancy().PlanDeployment(ctx, a, app.ID, planRequest(m), nil, imageCheckKey, false)
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, st, `UPDATE deployment_validations SET rollback_deployment_id=? WHERE deployment_id=?`, back.ID, prior.ID)
		}, RollbackInFlight},
		{"a service replaced no container", func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, _ string, _, updated *Deployment) {
			setPlan(t, st, updated, func(p *DeploymentPlan) { p.Services[0].Replaces = protocol.InspectionTarget{} })
		}, RollbackNoPriorIdentity},
		{"the plan names no service", func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, _ string, _, updated *Deployment) {
			setPlan(t, st, updated, func(p *DeploymentPlan) { p.Services = []PlannedService{} })
		}, RollbackNoPriorIdentity},
		{"prior definition tampered", func(t *testing.T, st *SQLStore, _ TenantAccess, app *Application, _ string, _, _ *Deployment) {
			mustExec(t, st, `UPDATE application_revisions SET spec=? WHERE application_id=? AND number=1`, `{"kind":"compose.v1","services":[{"name":"web","image":"nginx:tampered"}]}`, app.ID)
		}, RollbackPriorDefinitionInvalid},
		{"services renamed since", func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, _ string, _, updated *Deployment) {
			mustExec(t, st, `UPDATE application_resources SET service_name='api' WHERE instance_id=?`, updated.InstanceID)
		}, RollbackServiceSetChanged},
		{"a later deployment replaced the containers", func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, _ string, _, updated *Deployment) {
			mustExec(t, st, `UPDATE application_resources SET container_id=? WHERE instance_id=?`, strings.Repeat("6", 64), updated.InstanceID)
		}, RollbackServiceSetChanged},
		{"prior image removed", func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, endpoint string, _, updated *Deployment) {
			putInventory(t, st, endpoint, []protocol.Container{updatedContainer(updated)}, []protocol.Image{tagged(imageX, "nginx:1")})
		}, RollbackPriorImagesMissing},
		{"prior image only under a stopped container", func(t *testing.T, st *SQLStore, _ TenantAccess, _ *Application, endpoint string, _, updated *Deployment) {
			putInventory(t, st, endpoint, []protocol.Container{updatedContainer(updated), oldContainer("exited")}, []protocol.Image{tagged(imageX, "nginx:1")})
		}, RollbackPriorImagesMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, a, app, endpoint, _, _, prior, updated := validationFixture(t)
			tc.setup(t, st, a, app, endpoint, prior, updated)
			rb, reason, err := st.Tenancy().RollbackTarget(ctx, a, app.ID, updated.ID)
			if err != nil || rb != nil || reason != tc.want {
				t.Fatalf("%+v %q %v, want %q", rb, reason, err, tc.want)
			}
		})
	}
	// The adopted containers ran under no applied revision: previous_revision is 0.
	t.Run("first apply after adoption", func(t *testing.T) {
		st, a, app, endpoint, _, _ := planFixture(t)
		d := deployFixture(t, st, a, app, endpoint, priorID, []protocol.Image{tagged(imageD, "nginx:1")}, nil)
		if rb, reason, err := st.Tenancy().RollbackTarget(ctx, a, app.ID, d.ID); err != nil || rb != nil || reason != RollbackNoPriorIdentity {
			t.Fatalf("%+v %q %v", rb, reason, err)
		}
	})
}

// The rollback acts as the policy's creator: application.deploy is checked again. A rollback is
// never itself rolled back.
func TestRollbackTargetReauthorizesTheCreator(t *testing.T) {
	st, a, app, _, _, _, prior, updated := validationFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	viewer := policyMember(t, st, a, "viewer", RoleReadOnly)
	if _, _, err := ts.RollbackTarget(ctx, viewer, app.ID, updated.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a read-only member decided a rollback: %v", err)
	}
	if _, _, err := ts.RollbackTarget(ctx, a, app.ID, uuid.NewString()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no such deployment: %v", err)
	}
	mustExec(t, st, `UPDATE deployment_validations SET is_rollback=1 WHERE deployment_id=?`, updated.ID)
	if _, _, err := ts.RollbackTarget(ctx, a, app.ID, updated.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a rollback was offered a rollback: %v", err)
	}
	mustExec(t, st, `DELETE FROM application_instances WHERE id=?`, prior.InstanceID)
	if rb, reason, err := ts.RollbackTarget(ctx, a, app.ID, prior.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("released instance: %+v %q %v", rb, reason, err)
	}
}

// PinImages takes the given image instead of resolving the tag: no registry, no pull, and the
// image must be on the host now.
func TestPlanDeploymentPinsImages(t *testing.T) {
	st, a, app, endpoint, _, _, _, updated := validationFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	request := func(pins map[string]string) PlanRequest {
		t.Helper()
		m, err := ts.ReadApplicationMapping(ctx, a, app.ID)
		if err != nil {
			t.Fatal(err)
		}
		r := planRequest(m)
		r.PinImages = pins
		return r
	}
	plan := func(pin string) (*Deployment, error) {
		t.Helper()
		return ts.PlanDeployment(ctx, a, app.ID, request(map[string]string{"web": pin}), nil, imageCheckKey, false)
	}
	// nginx:1 resolves to imageX; the pin wins.
	d, err := plan(imageD)
	if err != nil || d.Plan.Services[0].ImageID != imageD || d.Plan.Services[0].PullDigest != "" || d.Plan.Services[0].Reference != "nginx:1" {
		t.Fatalf("pinned plan: %+v %v", d, err)
	}
	current := updatedContainer(updated)
	for _, tc := range []struct {
		name       string
		containers []protocol.Container
		images     []protocol.Image
		pin, want  string
	}{
		{"image gone", []protocol.Container{current}, []protocol.Image{tagged(imageX, "nginx:1")}, imageD, "image_not_reported"},
		{"only a stopped container has it", []protocol.Container{current, oldContainer("exited")}, []protocol.Image{tagged(imageX, "nginx:1")}, imageD, "image_not_reported"},
		{"not an image ID", []protocol.Container{current}, []protocol.Image{tagged(imageD), tagged(imageX, "nginx:1")}, "nginx:1", "image_identity_invalid"},
	} {
		putInventory(t, st, endpoint, tc.containers, tc.images)
		_, err := plan(tc.pin)
		var blocked *PreflightBlockedError
		if !errors.As(err, &blocked) || !slices.Contains(blocked.Blockers, tc.want) {
			t.Errorf("%s: %v, want %s", tc.name, err, tc.want)
		}
	}
	putInventory(t, st, endpoint, []protocol.Container{current, oldContainer("running")}, []protocol.Image{tagged(imageX, "nginx:1")})
	if d, err := plan(imageD); err != nil || d.Plan.Services[0].ImageID != imageD {
		t.Fatalf("pin under a running container: %+v %v", d, err)
	}
	// A pin naming no service would leave the real one resolving its tag; a pin beside a pull
	// would be overridden by it. Both are refused.
	if _, err := ts.PlanDeployment(ctx, a, app.ID, request(map[string]string{"api": imageD}), nil, imageCheckKey, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pin for no service: %v", err)
	}
	r := request(map[string]string{"web": imageD})
	r.Update = []string{"web"}
	if _, err := ts.PlanDeployment(ctx, a, app.ID, r, &fakeResolver{}, imageCheckKey, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pin beside an update: %v", err)
	}
}

// PinImages is server-set: a client body naming it decodes to nothing.
func TestPlanRequestNeverDecodesPinImages(t *testing.T) {
	var r PlanRequest
	if err := json.Unmarshal([]byte(`{"pin_images":{"web":"`+imageD+`"},"PinImages":{"web":"`+imageD+`"}}`), &r); err != nil || r.PinImages != nil {
		t.Fatalf("decoded pins: %+v %v", r.PinImages, err)
	}
}

// Prune keeps a deployment whose automated validation may still roll back: the validation row
// cascades with it, and with it the rollback decision and the policy's pause.
func TestPruneKeepsADeploymentWhoseRollbackIsOpen(t *testing.T) {
	st, _, _, _, _, _, prior, updated := validationFixture(t)
	ctx := context.Background()
	ts := st.Tenancy()
	// Neither row is the current or previous revision's newest apply any more.
	mustExec(t, st, `UPDATE application_instances SET current_revision=0,previous_revision=0 WHERE id=?`, updated.InstanceID)
	mustExec(t, st, `UPDATE deployments SET settled_at=? WHERE id IN (?,?)`, time.Now().UTC().Add(-DeploymentHistoryRetention-time.Hour), prior.ID, updated.ID)
	kept := func(id string) bool {
		t.Helper()
		if _, err := ts.Prune(ctx); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := st.db.QueryRowContext(ctx, st.rebind(`SELECT COUNT(*) FROM deployments WHERE id=?`), id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if kept(prior.ID) {
		t.Fatal("a manual, validated deployment outlived its retention")
	}
	if !kept(updated.ID) {
		t.Fatal("pruned a deployment while its validation was open")
	}
	if _, err := ts.FinishValidation(ctx, updated.ID, VerdictUnhealthy, "web"); err != nil {
		t.Fatal(err)
	}
	if !kept(updated.ID) {
		t.Fatal("pruned a deployment while its rollback decision was owed")
	}
	if err := ts.MarkRollbackPlanned(ctx, updated.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if !kept(updated.ID) {
		t.Fatal("pruned a deployment while its rollback was in flight")
	}
	if err := ts.MarkRollbackOutcome(ctx, updated.ID, RollbackFailed, RollbackNotSent); err != nil {
		t.Fatal(err)
	}
	if kept(updated.ID) {
		t.Fatal("an old, superseded deployment outlived its decided validation")
	}
}
