package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// Docker-socket access is host-equivalent; every enrollment command says so, in the same
// response as the one-time token, so it cannot be missed.
const socketDisclosure = "Mounting /var/run/docker.sock gives the KyYard agent, and therefore this control plane, root-equivalent access to that host. Enroll only hosts whose operators accept that."

func (s *Server) instanceFingerprint() string {
	if len(s.config.Security.InstanceKey) != ed25519.SeedSize {
		return ""
	}
	return protocol.Fingerprint(ed25519.NewKeyFromSeed(s.config.Security.InstanceKey).Public().(ed25519.PublicKey))
}

func (s *Server) handleCreateEnrollmentToken(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	var input struct {
		Runtime string `json:"runtime"`
	}
	if err := strictJSON(r, &input); err != nil {
		s.tenantError(w, err)
		return
	}
	image := s.config.Server.AgentImage
	tok, err := s.store.Tenancy().CreateEnrollmentToken(r.Context(), a, input.Runtime, image)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	secret := base64.RawURLEncoding.EncodeToString(tok.Secret)
	out := map[string]any{
		"id": tok.ID, "environment_id": tok.EnvironmentID, "runtime": tok.Runtime, "expires_at": tok.ExpiresAt,
		"token": secret, "disclosure": socketDisclosure,
	}
	// Reuse the exact installed image on the same Docker host. Sharing the server's
	// network namespace keeps HTTP on loopback and creates no Docker network.
	setup := `server=kyyard
server_id=$(docker inspect --type container --format '{{.Id}}' "$server") &&
image=$(docker inspect --type container --format '{{.Image}}' "$server_id") &&
`
	options := `--pull never --network "container:$server_id"`
	imageArg := `"$image"`
	origin := fmt.Sprintf("http://127.0.0.1:%d", s.config.Server.Port)
	out["note"] = "Local Docker normally connects automatically with the standard installation. This command creates an additional agent on the Docker host running KyYard and uses sudo if Docker requires it. If you renamed the server container, change server=kyyard. Compare the printed agent key fingerprint before approving below. After replacing the server container, recreate the agent with the same identity volume (see README)."
	if image != "" && strings.HasPrefix(s.config.Server.AppURL, "https://") {
		out["image"] = image
		setup, options, imageArg, origin = "", "--network bridge", shellQuote(image), s.config.Server.AppURL
		out["note"] = "Run on the additional Docker host to manage; the command uses sudo if Docker requires it. Compare the printed agent key fingerprint before approving below."
	}
	// Detached docker run does not forward piped stdin. Enroll in an attached,
	// short-lived process, then start the persistent agent with its saved identity.
	common := options + " --no-healthcheck --entrypoint /app/kyyard-agent -v /var/run/docker.sock:/var/run/docker.sock -v kyyard-agent-identity:/var/lib/kyyard-agent " + imageArg
	// Check privileges before the token enters stdin. Some Docker installations
	// require sudo for every inspect/run, not just the last command in the chain.
	privileges := `if docker info >/dev/null 2>&1; then
  kyyard_docker() { docker "$@"; }
else
  sudo -v && sudo docker info >/dev/null || exit 1
  kyyard_docker() { sudo docker "$@"; }
fi
`
	setup = strings.ReplaceAll(setup, "$(docker inspect", "$(kyyard_docker inspect")
	out["command"] = privileges + setup + fmt.Sprintf("printf '%%s\\n' '%s' | kyyard_docker run --rm -i --name kyyard-agent-enroll %s --server %s --name \"$(hostname)\" --enroll-only &&\n"+
		"kyyard_docker run -d --name kyyard-agent --restart unless-stopped %s", secret, common, shellQuote(origin), common)

	s.writeJSON(w, http.StatusCreated, out)
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

// handleAgentEnroll is the only agent-facing route in this slice. It has no session: the
// token selects the tenant, the proof binds the key, and every refusal is the same 401.
func (s *Server) handleAgentEnroll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.allowAttempt("enroll:"+s.requestIP(r), 10, time.Minute) {
		s.writeError(w, http.StatusTooManyRequests, "Too many enrollment attempts")
		return
	}
	var input struct {
		Token     string            `json:"token"`
		PublicKey string            `json:"public_key"`
		Proof     string            `json:"proof"`
		Name      string            `json:"name"`
		Facts     map[string]string `json:"facts"`
	}
	if err := strictJSON(r, &input); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid enrollment request")
		return
	}
	token, err1 := base64.RawURLEncoding.DecodeString(input.Token)
	key, err2 := base64.RawURLEncoding.DecodeString(input.PublicKey)
	proof, err3 := base64.RawURLEncoding.DecodeString(input.Proof)
	if err1 != nil || err2 != nil || err3 != nil {
		s.writeError(w, http.StatusUnauthorized, "Enrollment refused")
		return
	}
	e, err := s.store.Tenancy().Enroll(r.Context(), store.EnrollmentRequest{Token: token, PublicKey: key, Proof: proof, Name: input.Name, Facts: input.Facts, IPAddress: s.requestIP(r)})
	switch {
	case errors.Is(err, store.ErrAlreadyExists):
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "Endpoint name already used in this organization", "code": "name_taken"})
		return
	case err != nil:
		s.writeError(w, http.StatusUnauthorized, "Enrollment refused")
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"endpoint_id": e.ID, "state": e.State, "fingerprint": e.Fingerprint, "instance_fingerprint": s.instanceFingerprint()})
}

func endpointID(r *http.Request) (string, error) {
	id := r.PathValue("endpoint")
	if id == "" || len(id) > 64 {
		return "", store.ErrInvalid
	}
	return id, nil
}

func (s *Server) handleTenantEndpoints(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	offset, limit, err := tenantPage(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	rows, err := s.store.Tenancy().ListEndpoints(r.Context(), a, offset, limit)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleTenantEndpoint(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	e, err := s.store.Tenancy().ReadEndpoint(r.Context(), a, id)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleApproveEndpoint(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	var input struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := strictJSON(r, &input); err != nil {
		s.tenantError(w, err)
		return
	}
	if _, err := hex.DecodeString(input.Fingerprint); err != nil || len(input.Fingerprint) != 64 {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	if err := s.store.Tenancy().ApproveEndpoint(r.Context(), a, id, input.Fingerprint); err != nil {
		s.tenantError(w, err)
		return
	}
	s.agents.notify(id, envelope(protocol.TypeApproved, nil))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) endpointTransition(op func(context.Context, store.TenantAccess, string) error) func(http.ResponseWriter, *http.Request, store.TenantAccess) {
	return func(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
		id, err := endpointID(r)
		if err != nil {
			s.tenantError(w, err)
			return
		}
		if err := op(r.Context(), a, id); err != nil {
			s.tenantError(w, err)
			return
		}
		// Reject and revoke are terminal: a live socket ends in the same request.
		s.agents.closeEndpoint(id, protocol.CloseRevoked)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleRenameEndpoint(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	name, err := tenantName(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	if err := s.store.Tenancy().RenameEndpoint(r.Context(), a, id, name); err != nil {
		s.tenantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAcknowledgeKey is the human half of rotation: the named pending key becomes the only
// approved key, and a live agent is told so it switches without waiting for a reconnect.
func (s *Server) handleAcknowledgeKey(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	fp := r.PathValue("fingerprint")
	if _, err := hex.DecodeString(fp); err != nil || len(fp) != 64 {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	if err := s.store.Tenancy().AcknowledgeEndpointKey(r.Context(), a, id, fp); err != nil {
		s.tenantError(w, err)
		return
	}
	s.agents.notify(id, envelope(protocol.TypeRotated, protocol.Rotated{Fingerprint: fp}))
	// The acknowledged key is now the only one; a session on the retired key ends here, in
	// the same request, whether or not its holder honours the notice.
	s.agents.closeEndpoint(id, protocol.CloseKeyRetired)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAcknowledgeEvent(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	eventID, err := strconv.ParseInt(r.PathValue("event"), 10, 64)
	if err != nil || eventID <= 0 {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	if err := s.store.Tenancy().AcknowledgeEndpointEvent(r.Context(), a, id, eventID); err != nil {
		s.tenantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEndpointInventory(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	inv, err := s.store.Tenancy().ReadInventory(r.Context(), a, id)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, inv)
}

func (s *Server) handleLatestSamples(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	rows, err := s.store.Tenancy().LatestSamples(r.Context(), a, id)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rows)
}

// handleContainerRollups serves the hourly summary, which outlives the raw window: samples are
// kept six hours, their summaries a week. It is a separate route rather than a wider window on
// the raw one, so a caller always knows which resolution it asked for and got.
func (s *Server) handleContainerRollups(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	container := r.PathValue("container")
	if container == "" || len(container) > 128 {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	rows, err := s.store.Tenancy().ReadRollups(r.Context(), a, id, container, time.Duration(hours)*time.Hour)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleContainerSamples(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	container := r.PathValue("container")
	if container == "" || len(container) > 128 {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	minutes, _ := strconv.Atoi(r.URL.Query().Get("minutes"))
	rows, err := s.store.Tenancy().ReadSamples(r.Context(), a, id, container, time.Duration(minutes)*time.Minute)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rows)
}

// handleDispatchCommand records the intent, sends it, and answers with the record either way.
// The row is written before the frame goes out, so a command that reaches the agent and then
// loses its answer is still something the control plane can account for.
func (s *Server) handleDispatchCommand(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	var body struct {
		Action string `json:"action"`
		// A command names a container or an image, never both; the action says which field
		// applies and the store validates it against that grammar.
		Container string `json:"container"`
		Reference string `json:"reference"`
		// Confirm must repeat the container name for a destructive action. Requiring it here
		// rather than in the interface means every caller has to mean it, scripts included.
		Confirm string `json:"confirm"`
		Expects struct {
			ImageDigest string `json:"image_digest"`
			State       string `json:"state"`
		} `json:"expects"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		s.tenantError(w, store.ErrInvalid)
		return
	}
	expects := protocol.Expectation{ImageDigest: body.Expects.ImageDigest, State: body.Expects.State}
	if body.Container != "" && body.Reference != "" {
		// A command names one or the other. Preferring one silently would make the request
		// mean something the caller did not write.
		s.tenantError(w, store.ErrInvalid)
		return
	}
	target := body.Container
	if body.Reference != "" {
		target = body.Reference
	}
	cmd, err := s.store.Tenancy().CreateCommand(r.Context(), a, id, body.Action, target, body.Confirm, expects)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	frame := envelope(protocol.TypeCommand, protocol.Command{
		ID: cmd.ID, RequestID: cmd.RequestID, Org: cmd.OrganizationID, Env: cmd.EnvironmentID,
		Endpoint: cmd.EndpointID, Deadline: cmd.Deadline, Action: cmd.Action,
		Container: cmd.ContainerID, Reference: cmd.Reference, Expects: cmd.Expects,
	})
	if !s.agents.deliver(id, frame) {
		// Never sent, so nothing needs reconciling: say so plainly rather than leaving a row
		// in flight that an operator would have to chase.
		_ = s.store.Tenancy().SettleCommand(r.Context(), id, cmd.ID, protocol.OutcomeFailed, "the endpoint was not connected")
		cmd.Outcome = protocol.OutcomeFailed
		cmd.Detail = "the endpoint was not connected"
		s.writeJSON(w, http.StatusConflict, cmd)
		return
	}
	if err := s.store.Tenancy().MarkCommandDispatched(r.Context(), cmd.ID); err != nil {
		log.Printf("command %s: marking dispatched: %v", cmd.ID, err)
	}
	s.writeJSON(w, http.StatusAccepted, cmd)
}

func (s *Server) handleReadCommand(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	cmd, err := s.store.Tenancy().ReadCommand(r.Context(), a, id, r.PathValue("command"))
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, cmd)
}

func (s *Server) handleListCommands(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.store.Tenancy().ListCommands(r.Context(), a, id, limit)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rows)
}

// handleRemovalPreview says what destroying a container would mean, so a confirmation is a
// decision rather than a reflex. It answers from the stored inventory and says how old that is:
// the agent is not asked, because a preview must not be a way to make an endpoint do work, and
// the command itself re-checks the state at the moment it acts.
func (s *Server) handleRemovalPreview(w http.ResponseWriter, r *http.Request, a store.TenantAccess) {
	id, err := endpointID(r)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	container := r.PathValue("container")
	inv, err := s.store.Tenancy().ReadInventory(r.Context(), a, id)
	if err != nil {
		s.tenantError(w, err)
		return
	}
	var snap protocol.Snapshot
	if err := json.Unmarshal(inv.Snapshot, &snap); err != nil {
		s.tenantError(w, err)
		return
	}
	for _, c := range snap.Containers {
		if c.ID != container && c.Name != container {
			continue
		}
		preview := map[string]any{
			"organization": a.OrganizationID,
			"endpoint":     id,
			// Echoed as the caller addressed it, so a client can feed this response straight
			// back: the identifier it sent, and the name the server knows, are both here.
			"container":    container,
			"container_id": c.ID,
			"name":         c.Name,
			"image":        c.Image,
			"state":        c.State,
			"observed_at":  inv.ObservedAt,
			"received_at":  inv.ReceivedAt,
			// What an operator is actually deciding about.
			"consequences": consequencesOfRemoval(c),
			"confirm_with": c.Name,
		}
		s.writeJSON(w, http.StatusOK, preview)
		return
	}
	s.tenantError(w, store.ErrNotFound)
}

// consequencesOfRemoval names what goes and what stays. Saying what survives matters as much
// as saying what does not: an operator who believes the data is going too will hesitate over
// the wrong thing, and one who believes it is safe when it is not will lose it.
func consequencesOfRemoval(c protocol.Container) []string {
	out := []string{"the container and its writable layer are destroyed"}
	if c.State == "running" || c.State == "restarting" || c.State == "paused" {
		out = append(out, "it is "+c.State+", so removal is refused until it is stopped")
	}
	if len(c.Ports) > 0 {
		out = append(out, "published ports stop answering")
	}
	if c.ComposeProject != "" {
		out = append(out, "it belongs to compose project "+c.ComposeProject+", which may recreate it")
	}
	out = append(out, "named volumes and images are left alone; destroying data is a separate action")
	return out
}
