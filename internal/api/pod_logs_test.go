package api_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
	"github.com/coder/websocket"
)

// reports makes every connectCluster snapshot newer than the last, within one second too.
var reports atomic.Uint64

// connectCluster connects the fleet's cluster agent with the given capabilities and makes it
// active with a one-pod inventory.
func connectCluster(t *testing.T, ctx context.Context, f runtimeFleet, caps ...string) *websocket.Conn {
	t.Helper()
	sock, reason := connect(t, ctx, f.url, f.cluster, f.cluster.priv, protocol.Version)
	if reason != "" {
		t.Fatalf("connect: %s", reason)
	}
	t.Cleanup(func() { sock.conn.CloseNow() })
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHello, protocol.Hello{Capabilities: caps})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeInventory, protocol.Snapshot{Generation: uint64(time.Now().Unix()) - 1000 + reports.Add(1), Kubernetes: &protocol.KubernetesInventory{
		Pods: []protocol.Pod{{Namespace: "shop", Name: "web-7c9", Containers: []protocol.PodContainer{{Name: "web"}, {Name: "sidecar"}}}},
	}})
	writeEnvelope(t, ctx, sock.conn, protocol.TypeHeartbeat, nil)
	if e := readEnvelope(t, ctx, sock.conn); e.Type != protocol.TypeHeartbeat {
		t.Fatalf("setup answered %+v", e)
	}
	waitFor(t, func() bool {
		e, _ := f.st.Tenancy().ReadEndpointRaw(ctx, f.cluster.id)
		return e != nil && e.State == "active"
	})
	return sock.conn
}

// The pod route asks the agent for the named pod and container, never a container ID, streams
// what it answers, and audits the session with the pod it named.
func TestPodLogsStreamFromTheClusterToTheReader(t *testing.T) {
	f := newRuntimeFleet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn := connectCluster(t, ctx, f, protocol.CapabilityKubernetesInventory, protocol.CapabilityPodLogs)
	go func() {
		req := awaitOpen(t, ctx, conn)
		if req.Container != "" || req.Pod == nil || *req.Pod != (protocol.PodTarget{Namespace: "shop", Name: "web-7c9", Container: "web"}) || req.Tail != 10 {
			t.Errorf("the agent was asked for %+v", req)
		}
		writeEnvelope(t, ctx, conn, protocol.TypeLogChunk, protocol.LogChunk{Stream: req.Stream, Data: "ready\n"})
		writeEnvelope(t, ctx, conn, protocol.TypeLogClose, protocol.LogClose{Stream: req.Stream, Reason: "the log ended"})
	}()
	base := "/api/organizations/a/endpoints/" + f.cluster.id + "/pods/shop/web-7c9/logs"
	w := tenantRequest(f.s, f.admin, "GET", base+"?container=web&tail=10", "", false)
	if w.Code != 200 || w.Body.String() != "ready\n" {
		t.Fatalf("logs: %d %q", w.Code, w.Body.String())
	}
	rows, _, err := f.st.Audit().ListAuditRecords(ctx, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var session *store.AuditRecord
	for _, row := range rows {
		if row.Action == "container.logs" {
			session = row
		}
	}
	if session == nil || session.Result != "success" || session.Resource != f.cluster.id+"/pods/shop/web-7c9/web" {
		t.Fatalf("audit %+v", session)
	}

	// The agent's refusal of an unnamed container in a two-container pod reaches the reader.
	go func() {
		req := awaitOpen(t, ctx, conn)
		if req.Pod == nil || req.Pod.Container != "" {
			t.Errorf("the agent was asked for %+v", req)
		}
		writeEnvelope(t, ctx, conn, protocol.TypeLogClose, protocol.LogClose{Stream: req.Stream, Reason: "unknown_container: this pod runs 2 containers; name one", Failed: true})
	}()
	w = tenantRequest(f.s, f.admin, "GET", base, "", false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "the stream ended: unknown_container") {
		t.Fatalf("unnamed container: %d %q", w.Code, w.Body.String())
	}
}

// Names outside Kubernetes syntax are refused at the boundary; the pod route refuses a Docker
// endpoint and a cluster agent without pod.logs; a read-only member may not read it.
func TestPodLogsRefusals(t *testing.T) {
	f := newRuntimeFleet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	viewer := loginAs(t, f.s, f.st, "viewer", "user")
	if err := f.st.Tenancy().SetMembership(ctx, &store.OrganizationMembership{OrganizationID: "a", UserID: "usr_viewer", Role: store.RoleReadOnly, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	pods := "/api/organizations/a/endpoints/" + f.cluster.id + "/pods/"
	conn := connectCluster(t, ctx, f, protocol.CapabilityKubernetesInventory)
	for path, want := range map[string]int{
		pods + "Shop/web/logs":                                                400,
		pods + "shop/web/logs?container=a.b":                                  400,
		pods + "shop/web/logs?tail=0":                                         400,
		pods + "shop/web/logs?follow=1&download=1":                            400,
		pods + "shop/web/logs":                                                501,
		"/api/organizations/a/endpoints/" + f.host.id + "/pods/shop/web/logs": 409,
	} {
		if w := tenantRequest(f.s, f.admin, "GET", path, "", false); w.Code != want {
			t.Errorf("%s: %d %s, want %d", path, w.Code, w.Body.String(), want)
		}
	}
	conn.Close(websocket.StatusNormalClosure, "reconnect")
	waitFor(t, func() bool { return !f.s.Connected(f.cluster.id) })
	connectCluster(t, ctx, f, protocol.CapabilityKubernetesInventory, protocol.CapabilityPodLogs)
	if w := tenantRequest(f.s, viewer, "GET", pods+"shop/web/logs", "", false); w.Code != 403 {
		t.Fatalf("read-only member: %d %s", w.Code, w.Body.String())
	}
}
