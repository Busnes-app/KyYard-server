package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/permissions"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

func (s *Server) handleRecreateContainer(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	s.handleDirectCommand(w, r, a, store.ActionRecreate)
}

func (s *Server) handleRunContainer(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	s.handleDirectCommand(w, r, a, store.ActionRun)
}

// handleDirectCommand recreates a container (or runs a new one) from the full configuration the
// operator edited: an explicit deployment frame recorded as a command before it is sent, so a
// frame that reaches the agent and loses its answer is still accounted for.
func (s *Server) handleDirectCommand(w http.ResponseWriter, r *http.Request, a store.TenantAccess, action string) {
	// The frame carries environment values; a service token never sends one.
	if a.ServiceTokenID != "" {
		_ = s.store.Tenancy().DenyService(r.Context(), a, permissions.ContainerConfigure, "bearer")
		s.tenantError(w, store.ErrForbidden)
		return
	}
	endpoint, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	// Shared with the configuration read: one budget for everything that touches values.
	if !s.allowAttempt("configure:"+a.Principal(), 12, time.Minute) {
		s.writeError(w, 429, "Too many configuration requests")
		return
	}
	if err := s.store.Tenancy().CheckEndpointAccess(r.Context(), a, permissions.ContainerConfigure, endpoint); err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.runtimeGate(w, r, a, endpoint, dockerRoute) {
		return
	}
	var body struct {
		Expects *struct {
			ImageID     string `json:"image_id"`
			CreatedUnix int64  `json:"created_unix"`
			State       string `json:"state"`
		} `json:"expects"`
		Spec             protocol.ContainerConfiguration `json:"spec"`
		AcknowledgeBinds []string                        `json:"acknowledge_binds"`
		Confirm          string                          `json:"confirm"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // the frame's own bound applies below
	if strictJSON(r, &body) != nil || (action == store.ActionRecreate) != (body.Expects != nil) {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	spec := body.Spec
	var replaces protocol.InspectionTarget
	if action == store.ActionRecreate {
		replaces = protocol.InspectionTarget{ContainerID: r.PathValue("container"), ImageID: body.Expects.ImageID, CreatedUnix: body.Expects.CreatedUnix}
		if replaces.Validate() != nil {
			s.tenantError(w, store.ErrInvalid)
			return
		}
		managed, err := s.store.Tenancy().ContainerManaged(r.Context(), a, endpoint, replaces.ContainerID)
		if err != nil {
			s.tenantError(w, err)
			return
		}
		if managed {
			s.tenantError(w, store.ErrContainerManaged)
			return
		}
		// The form echoes the read's list; the note catches a form that dropped it.
		if s.configurationIncomplete(a.Principal(), replaces.ContainerID) {
			s.tenantError(w, &store.InvalidSpecError{Blockers: []string{"configuration_incomplete"}})
			return
		}
	}
	// Settings the agent could not express would be lost by the recreate.
	if len(spec.Unsupported) > 0 {
		s.tenantError(w, &store.InvalidSpecError{Blockers: []string{"configuration_incomplete"}})
		return
	}
	if field := specInvalid(spec, replaces); field != "" {
		s.tenantError(w, &store.InvalidSpecError{Blockers: []string{"spec_invalid:" + field}})
		return
	}
	ep, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, endpoint)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if !slices.Contains(ep.Capabilities, protocol.CapabilityDeploymentApply) || !slices.Contains(ep.Capabilities, protocol.CapabilityContainerConfigure) {
		s.writeError(w, http.StatusNotImplemented, "Upgrade the agent to enable container configuration")
		return
	}
	if !s.Connected(endpoint) {
		s.tenantError(w, store.ErrEndpointOffline)
		return
	}
	svc := protocol.DeploymentService{
		Name: "direct", ContainerName: spec.Name, Replaces: replaces, Restart: spec.Restart, Ports: append([]protocol.Port{}, spec.Ports...),
		Env: map[string]string{}, Mounts: append([]protocol.Mount{}, spec.Mounts...),
		Explicit: &protocol.ExplicitService{
			Command: spec.Command, Entrypoint: spec.Entrypoint, User: spec.User, WorkingDir: spec.WorkingDir, Hostname: spec.Hostname, Labels: spec.Labels,
			NetworkMode: spec.NetworkMode, Networks: spec.Networks, Resources: spec.Resources, Healthcheck: spec.Healthcheck, Privileged: spec.Privileged,
			ReadOnlyRootfs: spec.ReadOnlyRootfs, Init: spec.Init, TTY: spec.TTY, StdinOpen: spec.StdinOpen, CapAdd: spec.CapAdd, CapDrop: spec.CapDrop,
			SecurityOpt: spec.SecurityOpt, ExtraHosts: spec.ExtraHosts, DNS: spec.DNS, Devices: spec.Devices, Log: spec.Log, StopSignal: spec.StopSignal,
			StopTimeout: spec.StopTimeout, RestartRetries: spec.RestartRetries, AcknowledgedBinds: append([]string{}, body.AcknowledgeBinds...),
		},
	}
	for _, e := range spec.Env {
		svc.Env[e.Name] = e.Value
	}
	frame := protocol.DeploymentRequest{Services: []protocol.DeploymentService{svc}}
	for _, m := range spec.Mounts {
		if m.Kind == protocol.MountVolume && !slices.Contains(frame.Volumes, m.Source) {
			frame.Volumes = append(frame.Volumes, m.Source)
		}
	}
	dc := store.DirectCommand{Action: action, Confirm: body.Confirm, MaxFrameBytes: maxFrameBytes(ep.Capabilities), Frame: frame}
	if body.Expects != nil {
		dc.State = body.Expects.State
	}
	// Refuse what the store would refuse before spending registry budget on the image.
	if err := s.store.Tenancy().CheckDirectCommand(r.Context(), a, endpoint, dc); err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.directImage(w, r, a, ep, spec, replaces.ImageID, &dc.Frame) {
		return
	}
	cmd, req, err := s.store.Tenancy().CreateDirectCommand(r.Context(), a, endpoint, dc)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if !s.agents.deliver(endpoint, envelope(protocol.TypeDeploymentApply, req)) {
		if err := s.store.Tenancy().FailDirectCommand(context.WithoutCancel(r.Context()), endpoint, cmd.ID, "deployment_not_sent"); err != nil {
			log.Printf("command %s: recording an unsent frame: %v", cmd.ID, err)
		}
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "The command could not be sent to the endpoint", "code": "deployment_not_sent"})
		return
	}
	if err := s.store.Tenancy().MarkCommandDispatched(r.Context(), cmd.ID); err != nil {
		log.Printf("command %s: marking dispatched: %v", cmd.ID, err)
	}
	s.writeJSON(w, http.StatusAccepted, cmd)
}

// directImage points the frame's service at the spec's image ID (or, with none, the recreate
// target's) when the host has that image and it is the one the spec names; a locally built image
// with no digest qualifies by its tag or by a reference that is its ID. Otherwise it pulls: at the kept digest (what the operator
// saw), or with none kept at the reference's current registry digest. It writes the response and
// reports false on refusal.
func (s *Server) directImage(w http.ResponseWriter, r *http.Request, a store.TenantAccess, ep *store.Endpoint, spec protocol.ContainerConfiguration, targetImage string, frame *protocol.DeploymentRequest) bool {
	svc := &frame.Services[0]
	local := spec.ImageID
	if local == "" {
		local = targetImage
	}
	if local != "" {
		inv, err := s.store.Tenancy().ReadInventory(r.Context(), a, ep.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.tenantError(w, err)
			return false
		}
		var snap protocol.Snapshot
		// Only the image the spec names: its kept digest, or with none a tag equal to the
		// reference or the reference being its ID (a container KyYard created names its image
		// that way). A changed reference or a cleared digest pulls instead.
		if inv != nil && json.Unmarshal(inv.Snapshot, &snap) == nil && slices.ContainsFunc(snap.Images, func(im protocol.Image) bool {
			if im.ID != local {
				return false
			}
			if spec.Image.Digest != "" {
				return slices.ContainsFunc(im.Digests, func(d string) bool { return strings.HasSuffix(d, "@"+spec.Image.Digest) })
			}
			return spec.Image.Reference == im.ID || slices.Contains(im.Tags, spec.Image.Reference)
		}) {
			svc.ImageID = local
			return true
		}
	}
	if !slices.Contains(ep.Capabilities, protocol.CapabilityDeploymentPull) {
		s.writeError(w, http.StatusNotImplemented, "Upgrade the host agent to enable deployments that pull images")
		return false
	}
	release, ok := s.acquireRegistrySlot(w, a.OrganizationID)
	if !ok {
		return false
	}
	defer release()
	extendRegistryDeadline(w)
	reference := spec.Image.Reference
	if spec.Image.Digest != "" {
		name, _ := protocol.SplitImageReference(reference)
		reference = name + "@" + spec.Image.Digest
	}
	pull, auth, err := s.store.Tenancy().ResolveDirectImage(r.Context(), a, ep.ID, reference, s.resolver(), s.config.Security.EncryptionKey, s.config.Registry.AllowPrivate)
	if err != nil {
		s.tenantError(w, err)
		return false
	}
	svc.Pull, frame.Registries = pull, auth
	return true
}

// specInvalid names the first field of spec the configuration rules refuse, or "". The spec is
// checked as a read of target would be; a run has no target, so a placeholder stands in.
func specInvalid(spec protocol.ContainerConfiguration, target protocol.InspectionTarget) string {
	if !protocol.ValidContainerID(spec.Name) {
		return "name"
	}
	if target == (protocol.InspectionTarget{}) {
		target = protocol.InspectionTarget{ContainerID: strings.Repeat("0", 64), ImageID: "sha256:" + strings.Repeat("0", 64), CreatedUnix: 1}
	}
	spec.Target, spec.ObservedAt = target, time.Now()
	if spec.ImageID == "" {
		spec.ImageID = target.ImageID
	}
	if err := spec.Validate(target, spec.ObservedAt); err != nil {
		_, field, _ := strings.Cut(err.Error(), ": ")
		return strings.NewReplacer(", ", "_", " ", "_").Replace(field)
	}
	return ""
}
