package client_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/client"
	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func TestInventoryRefreshAndMutationPublishBeforeResult(t *testing.T) {
	host, st, jar, id, dir := approvedAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var stopped atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- client.Run(ctx, id, client.Options{HTTPClient: host.Client(), IdentityDir: dir, InventoryEvery: time.Hour,
			Snapshot: func(context.Context) (*protocol.Snapshot, error) {
				state := "running"
				if stopped.Load() {
					state = "exited"
				}
				return &protocol.Snapshot{ObservedAt: time.Now(), Containers: []protocol.Container{{ID: "c1", Name: "web", State: state, Ports: []protocol.Port{}, Labels: map[string]string{}, Networks: []string{}}}, Images: []protocol.Image{}, Networks: []protocol.Network{}, Volumes: []protocol.Volume{}}, nil
			}, Operate: func(context.Context, protocol.Command) (string, string) {
				stopped.Store(true)
				return protocol.OutcomeSucceeded, ""
			},
		})
	}()
	defer func() { cancel(); <-done }()
	waitState(t, ctx, st.Tenancy(), id.EndpointID, "active")
	a := store.TenantAccess{ActorID: "usr_admin", OrganizationID: "a"}
	before, err := st.Tenancy().ReadInventory(ctx, a, id.EndpointID)
	if err != nil {
		t.Fatal(err)
	}
	base := host.URL + "/api/organizations/a/endpoints/" + id.EndpointID
	var refreshed store.Inventory
	if err := json.Unmarshal(post(t, base+"/inventory/refresh", jar, ""), &refreshed); err != nil || refreshed.Generation <= before.Generation {
		t.Fatalf("refresh did not await a new report: %+v %v", refreshed, err)
	}
	var cmd store.Command
	if err := json.Unmarshal(post(t, base+"/commands", jar, `{"action":"container.stop","container":"c1","expects":{"state":"running"}}`), &cmd); err != nil {
		t.Fatal(err)
	}
	for ctx.Err() == nil {
		result, err := st.Tenancy().ReadCommand(ctx, a, id.EndpointID, cmd.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.Outcome != "" {
			inv, err := st.Tenancy().ReadInventory(ctx, a, id.EndpointID)
			var snap protocol.Snapshot
			if err != nil || json.Unmarshal(inv.Snapshot, &snap) != nil || len(snap.Containers) != 1 || snap.Containers[0].State != "exited" || inv.Generation <= refreshed.Generation || result.Outcome != protocol.OutcomeSucceeded {
				t.Fatalf("result preceded inventory: %+v %+v %v", result, inv, err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("command never settled")
}
