package api

import (
	"net/http"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// routeKind is which runtimes a route serves.
type routeKind int

const (
	// dockerRoute acts on containers, images, exec, inspection, commands or adoption: Docker only.
	dockerRoute routeKind = iota
	// applicationRoute maps, plans, applies or removes an application on either runtime; the
	// store holds each instance to its endpoint's runtime.
	applicationRoute
	// kubernetesRoute acts on a cluster's workloads or pods: Kubernetes only.
	kubernetesRoute
)

// runtimeGate refuses, with 409 runtime_unsupported, a Docker route aimed at an endpoint that is
// not Docker and a Kubernetes route aimed at one that is not a cluster. It runs after the
// action's own permission (adoptionGate, CheckEndpointAccess) and before any capability check,
// so a member without the permission is told that, and one with it is told the runtime rather
// than asked for an agent upgrade that would not help. It writes the response and reports false
// on refusal.
func (s *Server) runtimeGate(w http.ResponseWriter, r *http.Request, a store.TenantAccess, endpointID string, kind routeKind) bool {
	_, ok := s.runtimeEndpoint(w, r, a, endpointID, kind)
	return ok
}

// runtimeEndpoint is runtimeGate returning the endpoint it read.
func (s *Server) runtimeEndpoint(w http.ResponseWriter, r *http.Request, a store.TenantAccess, endpointID string, kind routeKind) (*store.Endpoint, bool) {
	e, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, endpointID)
	if err != nil {
		s.tenantError(w, err)
		return nil, false
	}
	if (kind == dockerRoute && e.Runtime != protocol.RuntimeDocker) || (kind == kubernetesRoute && e.Runtime != protocol.RuntimeKubernetes) {
		s.tenantError(w, store.ErrRuntimeUnsupported)
		return nil, false
	}
	return e, true
}

// adoptionGate authorizes application.adopt, the permission of adoption and service mapping, on
// the application, then runs the runtime gate for the endpoint the request aims at.
func (s *Server) adoptionGate(w http.ResponseWriter, r *http.Request, a store.TenantAccess, app, endpointID string, kind routeKind) bool {
	if err := s.store.Tenancy().CheckApplicationAccess(r.Context(), a, permissions.ApplicationAdopt, app); err != nil {
		s.tenantError(w, err)
		return false
	}
	return s.runtimeGate(w, r, a, endpointID, kind)
}

// runtimeCapability holds a plan or an instance to its endpoint's runtime (a namespace on a
// cluster, none on Docker) and names the capability the action needs there.
func runtimeCapability(ep *store.Endpoint, namespace, docker, cluster string) (string, error) {
	kube := ep.Runtime == protocol.RuntimeKubernetes
	if kube != (namespace != "") {
		return "", store.ErrRuntimeUnsupported
	}
	if kube {
		return cluster, nil
	}
	return docker, nil
}
