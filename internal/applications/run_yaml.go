package applications

import (
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/agent/protocol"
	"go.yaml.in/yaml/v3"
	k8syaml "sigs.k8s.io/yaml"
)

// RunYAML is transient, privileged configuration, never persisted by the preview endpoint.
type RunYAML struct {
	Containers []protocol.ContainerConfiguration `json:"containers"`
	Workloads  []protocol.WorkloadConfiguration  `json:"workloads"`
}

// ParseRunYAML converts the supported Compose or Deployment subset to the existing run frames.
// Nothing is dispatched, and unknown fields are refused rather than lost in conversion.
func ParseRunYAML(source, runtime string) (*RunYAML, error) {
	if runtime == "docker" {
		return composeRun(source)
	}
	if runtime != "kubernetes" || len(source) == 0 || len(source) > MaxComposeBytes {
		return nil, &Diagnostic{Reason: "Expected at most 65536 bytes of Kubernetes Deployment YAML"}
	}
	out := &RunYAML{Containers: []protocol.ContainerConfiguration{}, Workloads: []protocol.WorkloadConfiguration{}}
	decoder := yaml.NewDecoder(strings.NewReader(source))
	count := 0
	seen := map[string]bool{}
	for {
		var node yaml.Node
		err := decoder.Decode(&node)
		if err == io.EOF {
			break
		}
		if err != nil || len(node.Content) != 1 {
			return nil, &Diagnostic{Reason: "Invalid Kubernetes YAML"}
		}
		if len(out.Workloads) >= 16 {
			return nil, &Diagnostic{Reason: "At most 16 Deployments per preview"}
		}
		if err = checkTree(&node, 0, &count); err != nil {
			return nil, err
		}
		// Keep a native Deployment rather than dropping template fields in a conversion.
		data, _ := yaml.Marshal(&node)
		raw, err := k8syaml.YAMLToJSONStrict(data)
		if err != nil {
			return nil, &Diagnostic{Reason: "Invalid Kubernetes YAML"}
		}
		var identity struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if json.Unmarshal(raw, &identity) != nil {
			return nil, &Diagnostic{Reason: "Invalid Deployment identity"}
		}
		target := protocol.WorkloadRef{Namespace: identity.Metadata.Namespace, Kind: protocol.WorkloadDeployment, Name: identity.Metadata.Name}
		if seen[target.String()] {
			return nil, &Diagnostic{Reason: "Duplicate Deployment identity"}
		}
		seen[target.String()] = true
		deployment, err := protocol.RunDeployment(raw, target)
		if err != nil {
			return nil, &Diagnostic{Reason: "Expected a new apps/v1 Deployment with matching selector labels, a granted namespace and no service-account token mount"}
		}
		// The preview shows the canonical token policy and is what the confirmation submits.
		raw, _ = json.Marshal(deployment)
		w := protocol.WorkloadConfiguration{Target: target, ObservedAt: time.Now().UTC(), Replicas: deployment.Spec.Replicas, Strategy: string(deployment.Spec.Strategy.Type), Containers: []protocol.WorkloadContainer{}, InitContainers: []protocol.WorkloadContainer{}, EnvFrom: []string{}, Unsupported: []string{}, RunManifest: raw}
		if w.Strategy == "" {
			w.Strategy = "RollingUpdate"
		}
		for _, c := range deployment.Spec.Template.Spec.Containers {
			w.Containers = append(w.Containers, protocol.WorkloadContainer{Name: c.Name, Image: c.Image, Command: []string{}, Args: []string{}, Env: []protocol.WorkloadEnv{}})
		}

		checked := w
		checked.ObservedAt = time.Time{}
		now := time.Now()
		frame := protocol.WorkloadApply{Request: "11111111-2222-4333-8444-555555555555", Endpoint: "preview", IssuedAt: now, Deadline: now.Add(time.Minute), Target: target, Spec: checked, Create: true}
		if frame.Validate(now) != nil {
			return nil, &Diagnostic{Reason: "Invalid Deployment run settings"}
		}
		out.Workloads = append(out.Workloads, w)
	}
	if len(out.Workloads) == 0 {
		return nil, &Diagnostic{Reason: "At least one Deployment is required"}
	}
	return out, nil
}

func composeRun(source string) (*RunYAML, error) {
	normalized, extras, err := normalizeRunCompose(source)
	if err != nil {
		return nil, err
	}
	imported, err := ParseCompose(normalized)
	if err != nil {
		return nil, err
	}
	if len(imported.Spec.Services) > 16 {
		return nil, &Diagnostic{Reason: "At most 16 services per run preview"}
	}
	// Named-volume creation belongs to managed application plans; direct runs mount only
	// existing volumes and therefore accept external declarations only.
	for _, v := range imported.Spec.Volumes {
		if !v.External {
			return nil, &Diagnostic{Reason: "Direct runs require external named volumes that already exist"}
		}
	}
	out := &RunYAML{Containers: []protocol.ContainerConfiguration{}, Workloads: []protocol.WorkloadConfiguration{}}
	seen := map[string]bool{}
	for _, service := range imported.Spec.Services {
		c := protocol.ContainerConfiguration{Name: service.Name, Image: protocol.ImagePull{Reference: service.Image}, Command: []string{}, Entrypoint: []string{}, Env: []protocol.EnvEntry{}, Labels: map[string]string{}, Ports: []protocol.Port{}, Mounts: []protocol.Mount{}, Networks: []protocol.NetworkAttachmentSpec{}, CapAdd: []string{}, CapDrop: []string{}, SecurityOpt: []string{}, ExtraHosts: []string{}, DNS: []string{}, Devices: []protocol.Device{}, Log: protocol.LogConfig{Options: map[string]string{}}, Unsupported: []string{}, Restart: service.Restart}
		extra := extras[service.Name]
		if extra.name != "" {
			c.Name = extra.name
		}
		if extra.command != nil {
			c.Command = extra.command
		}
		if extra.entrypoint != nil {
			c.Entrypoint = extra.entrypoint
		}
		if c.Restart == "" {
			c.Restart = "no"
		}
		keys := make([]string, 0, len(service.Environment))
		for key := range service.Environment {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			c.Env = append(c.Env, protocol.EnvEntry{Name: key, Value: imported.Values[service.Environment[key].SecretRef]})
		}
		for _, port := range service.Ports {
			c.Ports = append(c.Ports, protocol.Port{HostIP: port.HostIP, Host: port.Published, Container: port.Target, Protocol: port.Protocol})
		}
		for _, volume := range service.Volumes {
			kind := volume.Kind
			if kind == "named" {
				kind = "volume"
			}
			c.Mounts = append(c.Mounts, protocol.Mount{Kind: kind, Source: volume.Source, Target: volume.Target, ReadOnly: volume.ReadOnly})
		}
		if seen[c.Name] {
			return nil, &Diagnostic{Reason: "Duplicate container name"}
		}
		seen[c.Name] = true
		out.Containers = append(out.Containers, c)
	}
	return out, nil
}
