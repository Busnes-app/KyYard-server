// Package applications validates desired configuration without reading host files,
// process environment, URLs or runtime inventory (docs/application-schema.md).
package applications

import (
	"crypto/sha256"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Busnes-app/kyyard-server/internal/store"
	"go.yaml.in/yaml/v3"
)

const MaxComposeBytes = 64 * 1024

// Import contains sensitive transient input. Never serialize or log it.
type Import struct {
	Spec   store.ApplicationSpec `json:"-"`
	Values map[string]string     `json:"-"`
}

// Diagnostic deliberately contains no source text, parser error, or scalar value.
type Diagnostic struct {
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Reason string `json:"reason"`
}

func (d *Diagnostic) Error() string {
	return fmt.Sprintf("Compose line %d, column %d: %s", d.Line, d.Column, d.Reason)
}
func refusal(n *yaml.Node, reason string) error { return &Diagnostic{n.Line, n.Column, reason} }

// ParseCompose accepts an explicit initial subset. Unsupported fields are errors,
// never ignored. Values must be explicit strings; interpolation and implicit host
// environment lookups are refused. $$ represents a literal dollar as in Compose.
func ParseCompose(source string) (*Import, error) {
	if len(source) == 0 || len(source) > MaxComposeBytes {
		return nil, &Diagnostic{Reason: "Document must be between 1 and 65536 bytes"}
	}
	dec := yaml.NewDecoder(strings.NewReader(source))
	var doc, extra yaml.Node
	if err := dec.Decode(&doc); err != nil || len(doc.Content) != 1 {
		return nil, &Diagnostic{Reason: "Invalid YAML document"}
	}
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, &Diagnostic{Reason: "Exactly one YAML document is required"}
	}
	count := 0
	if err := checkTree(&doc, 0, &count); err != nil {
		return nil, err
	}
	root, err := mapping(doc.Content[0])
	if err != nil {
		return nil, err
	}
	if err = fields(root, "services", "volumes"); err != nil {
		return nil, err
	}
	servicesNode := root["services"]
	if servicesNode == nil {
		return nil, refusal(doc.Content[0], "services is required")
	}
	services, err := mapping(servicesNode)
	if err != nil {
		return nil, err
	}
	if len(services) == 0 || len(services) > 100 {
		return nil, refusal(servicesNode, "Expected 1–100 services")
	}
	out := &Import{Spec: store.ApplicationSpec{Kind: "compose.v1"}, Values: map[string]string{}}
	declared := map[string]bool{}
	if top := root["volumes"]; top != nil {
		entries, err := mapping(top)
		if err != nil {
			return nil, err
		}
		if len(entries) > 64 {
			return nil, refusal(top, "At most 64 volumes may be declared")
		}
		for i := 0; i < len(top.Content); i += 2 {
			key, n := top.Content[i], top.Content[i+1]
			if !store.ValidDeclaredVolumeName(key.Value) {
				return nil, refusal(key, "Volume names must match [a-zA-Z0-9][a-zA-Z0-9_.-]{1,63}")
			}
			v := store.DeclaredVolume{Name: key.Value}
			if n.Tag != "!!null" {
				config, err := mapping(n)
				if err != nil {
					return nil, err
				}
				if err = fields(config, "external"); err != nil {
					return nil, err
				}
				if n := config["external"]; n != nil {
					if v.External, err = boolean(n); err != nil {
						return nil, err
					}
				}
			}
			declared[v.Name] = true
			out.Spec.Volumes = append(out.Spec.Volumes, v)
		}
		sort.Slice(out.Spec.Volumes, func(i, j int) bool { return out.Spec.Volumes[i].Name < out.Spec.Volumes[j].Name })
	}
	for _, name := range sortedKeys(services) {
		node := services[name]
		config, err := mapping(node)
		if err != nil {
			return nil, err
		}
		if err = fields(config, "image", "environment", "ports", "restart", "volumes"); err != nil {
			return nil, err
		}
		image, err := literal(config["image"])
		if err != nil {
			return nil, err
		}
		service := store.ApplicationService{Name: name, Image: image}
		if restart := config["restart"]; restart != nil {
			service.Restart, err = literal(restart)
			if err != nil {
				return nil, err
			}
		}
		if ports := config["ports"]; ports != nil {
			if ports.Kind != yaml.SequenceNode || len(ports.Content) > 64 {
				return nil, refusal(ports, "ports must be a list of at most 64 long-syntax mappings")
			}
			for _, node := range ports.Content {
				p, err := mapping(node)
				if err != nil {
					return nil, err
				}
				if err = fields(p, "target", "published", "host_ip", "protocol"); err != nil {
					return nil, err
				}
				// Require long syntax to avoid YAML base-60 and ambiguous short port parsing.
				var port store.ApplicationPort
				for field, dest := range map[string]*int{"target": &port.Target, "published": &port.Published} {
					n := p[field]
					if n == nil || n.Kind != yaml.ScalarNode || (n.Tag != "!!int" && n.Tag != "!!str") {
						return nil, refusal(node, "target and published must be decimal ports")
					}
					if len(n.Value) == 0 || (len(n.Value) > 1 && n.Value[0] == '0') || strings.Trim(n.Value, "0123456789") != "" {
						return nil, refusal(n, "Ports must be decimal numbers")
					}
					for _, digit := range n.Value {
						*dest = *dest*10 + int(digit-'0')
						if *dest > 65535 {
							return nil, refusal(n, "Port is out of range")
						}
					}
				}
				port.Protocol = "tcp"
				if n := p["protocol"]; n != nil {
					port.Protocol, err = literal(n)
					if err != nil {
						return nil, err
					}
				}
				if n := p["host_ip"]; n != nil {
					port.HostIP, err = literal(n)
					if err != nil {
						return nil, err
					}
				}
				service.Ports = append(service.Ports, port)
			}
		}
		if n := config["volumes"]; n != nil {
			if service.Volumes, err = volumes(n, declared); err != nil {
				return nil, err
			}
		}
		if env := config["environment"]; env != nil {
			values := map[string]string{}
			switch env.Kind {
			case yaml.MappingNode:
				entries, err := mapping(env)
				if err != nil {
					return nil, err
				}
				for key, n := range entries {
					value, err := literal(n)
					if err != nil {
						return nil, err
					}
					values[key] = value
				}
			case yaml.SequenceNode:
				for _, n := range env.Content {
					entry, err := literal(n)
					if err != nil {
						return nil, err
					}
					key, value, ok := strings.Cut(entry, "=")
					if !ok {
						return nil, refusal(n, "Environment entries require an explicit value")
					}
					if _, exists := values[key]; exists {
						return nil, refusal(n, "Duplicate environment key")
					}
					values[key] = value
				}
			default:
				return nil, refusal(env, "environment must be a mapping or list")
			}
			service.Environment = map[string]store.ApplicationSecretRef{}
			for key, value := range values {
				ref := fmt.Sprintf("env-%x", sha256.Sum256([]byte(name+"\x00"+key)))
				service.Environment[key] = store.ApplicationSecretRef{SecretRef: ref}
				out.Values[ref] = value
			}
		}
		out.Spec.Services = append(out.Spec.Services, service)
	}
	if err := store.ValidateApplicationSpec(out.Spec); err != nil {
		return nil, &Diagnostic{Reason: "Invalid service name, image, restart policy, port, environment key, volume, or specification size"}
	}
	return out, nil
}

const (
	relativeBind    = "Relative and home-relative bind mounts are unsupported; write the absolute path"
	anonymousVolume = "Anonymous volumes are unsupported; declare a named volume"
	normalizedPath  = "Paths must be absolute and normalized (no trailing slash, surrounding spaces, . or ..)"
)

// volumes parses short (SOURCE:TARGET[:ro|rw]) and long syntax. Relative and home binds
// need a working directory KyYard does not have, so they are refused rather than guessed.
func volumes(n *yaml.Node, declared map[string]bool) ([]store.ApplicationVolume, error) {
	if n.Kind != yaml.SequenceNode || len(n.Content) > 32 {
		return nil, refusal(n, "volumes must be a list of at most 32 entries")
	}
	out := make([]store.ApplicationVolume, 0, len(n.Content))
	targets := map[string]bool{}
	for _, entry := range n.Content {
		v := store.ApplicationVolume{Kind: "named"}
		at, targetAt := entry, entry
		if entry.Kind == yaml.MappingNode {
			m, err := mapping(entry)
			if err != nil {
				return nil, err
			}
			if err = fields(m, "type", "source", "target", "read_only"); err != nil {
				return nil, err
			}
			kind, source, target := m["type"], m["source"], m["target"]
			if kind == nil || target == nil {
				return nil, refusal(entry, "Long-syntax volumes require type and target")
			}
			typ, err := literal(kind)
			if err != nil {
				return nil, err
			}
			switch typ {
			case "volume":
			case "bind":
				v.Kind = "bind"
			default:
				return nil, refusal(kind, "Volume type must be volume or bind")
			}
			if source == nil {
				return nil, refusal(entry, anonymousVolume)
			}
			at, targetAt = source, target
			if v.Source, err = literal(source); err != nil {
				return nil, err
			}
			if v.Target, err = literal(target); err != nil {
				return nil, err
			}
			if ro := m["read_only"]; ro != nil {
				if v.ReadOnly, err = boolean(ro); err != nil {
					return nil, err
				}
			}
			if v.Kind == "bind" && !strings.HasPrefix(v.Source, "/") {
				return nil, refusal(source, relativeBind)
			}
		} else {
			s, err := literal(entry)
			if err != nil {
				return nil, err
			}
			// A leading drive letter is a Windows path, as Compose reads it.
			windows := len(s) > 1 && s[1] == ':' && ('a' <= s[0]|0x20 && s[0]|0x20 <= 'z')
			if windows || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~") {
				return nil, refusal(entry, relativeBind)
			}
			parts := strings.SplitN(s, ":", 3)
			if len(parts) == 1 {
				return nil, refusal(entry, anonymousVolume)
			}
			v.Source, v.Target = parts[0], parts[1]
			if len(parts) == 3 {
				switch parts[2] {
				case "ro":
					v.ReadOnly = true
				case "rw":
				default:
					return nil, refusal(entry, "Volume mode must be ro or rw")
				}
			}
			if strings.HasPrefix(v.Source, "/") {
				v.Kind = "bind"
			}
		}
		switch {
		case v.Source == "" || v.Target == "":
			return nil, refusal(at, "Volume source and target are required")
		case v.Kind == "named" && !declared[v.Source]:
			return nil, refusal(at, "Named volumes must be declared under top-level volumes")
		case v.Kind == "bind" && !store.CleanAbsolutePath(v.Source):
			return nil, refusal(at, normalizedPath)
		case !store.CleanAbsolutePath(v.Target):
			return nil, refusal(targetAt, normalizedPath)
		case v.Target == "/":
			return nil, refusal(targetAt, "A volume cannot be mounted at /")
		case targets[v.Target]:
			return nil, refusal(targetAt, "Duplicate volume target")
		}
		targets[v.Target] = true
		out = append(out, v)
	}
	return out, nil
}

func boolean(n *yaml.Node) (bool, error) {
	var b bool
	if n.Kind != yaml.ScalarNode || n.Tag != "!!bool" || n.Decode(&b) != nil {
		return false, refusal(n, "An explicit true or false is required")
	}
	return b, nil
}
func sortedKeys(m map[string]*yaml.Node) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func literal(n *yaml.Node) (string, error) {
	if n == nil {
		return "", &Diagnostic{Reason: "An explicit image is required"}
	}
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return "", refusal(n, "An explicit string is required; quote numeric and boolean values")
	}
	var b strings.Builder
	for i := 0; i < len(n.Value); i++ {
		if n.Value[i] == '$' {
			if i+1 >= len(n.Value) || n.Value[i+1] != '$' {
				return "", refusal(n, "Interpolation is unsupported; supply resolved values, escaping literal dollars as $$")
			}
			i++
		}
		b.WriteByte(n.Value[i])
	}
	return b.String(), nil
}
func mapping(n *yaml.Node) (map[string]*yaml.Node, error) {
	if n.Kind != yaml.MappingNode {
		return nil, refusal(n, "Expected a mapping")
	}
	out := map[string]*yaml.Node{}
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind != yaml.ScalarNode || k.Tag != "!!str" {
			return nil, refusal(k, "Keys must be strings")
		}
		if _, ok := out[k.Value]; ok {
			return nil, refusal(k, "Duplicate key")
		}
		out[k.Value] = v
	}
	return out, nil
}
func fields(m map[string]*yaml.Node, allowed ...string) error {
	for key, n := range m {
		ok := false
		for _, a := range allowed {
			if key == a {
				ok = true
				break
			}
		}
		if !ok {
			return refusal(n, "Unsupported field; see the supported import fields")
		}
	}
	return nil
}
func checkTree(n *yaml.Node, depth int, count *int) error {
	*count++
	if depth > 16 || *count > 8192 {
		return refusal(n, "YAML nesting or node limit exceeded")
	}
	if n.Kind == yaml.AliasNode || n.Anchor != "" || n.Tag == "!!merge" || (n.Style&yaml.TaggedStyle) != 0 {
		return refusal(n, "Aliases, anchors, merge keys and explicit tags are unsupported")
	}
	for _, c := range n.Content {
		if err := checkTree(c, depth+1, count); err != nil {
			return err
		}
	}
	return nil
}
