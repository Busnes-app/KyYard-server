package applications

import (
	"io"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

type runExtras struct {
	name                string
	command, entrypoint []string
}

// Run-only conveniences keep saved application imports unchanged. Decode shell commands only
// as explicit argv lists; guessing shell tokenization would change what the operator wrote.
func normalizeRunCompose(source string) (string, map[string]runExtras, error) {
	if len(source) == 0 || len(source) > MaxComposeBytes {
		return "", nil, &Diagnostic{Reason: "Document must be between 1 and 65536 bytes"}
	}
	decoder := yaml.NewDecoder(strings.NewReader(source))
	var document, extra yaml.Node
	if decoder.Decode(&document) != nil || len(document.Content) != 1 || decoder.Decode(&extra) != io.EOF {
		return "", nil, &Diagnostic{Reason: "Exactly one valid YAML document is required"}
	}
	count := 0
	if err := checkTree(&document, 0, &count); err != nil {
		return "", nil, err
	}
	root, err := mapping(document.Content[0])
	if err != nil {
		return "", nil, err
	}
	servicesNode := root["services"]
	if servicesNode == nil {
		return "", nil, &Diagnostic{Reason: "services is required"}
	}
	services, err := mapping(servicesNode)
	if err != nil {
		return "", nil, err
	}
	extras := map[string]runExtras{}
	for name, service := range services {
		values, err := mapping(service)
		if err != nil {
			return "", nil, err
		}
		e := runExtras{}
		if n := values["container_name"]; n != nil {
			e.name, err = literal(n)
			if err != nil {
				return "", nil, err
			}
		}
		for key, dest := range map[string]*[]string{"command": &e.command, "entrypoint": &e.entrypoint} {
			if n := values[key]; n != nil {
				if n.Kind != yaml.SequenceNode || len(n.Content) > 64 {
					return "", nil, refusal(n, "Run commands must be argv lists of at most 64 strings")
				}
				for _, arg := range n.Content {
					value, err := literal(arg)
					if err != nil {
						return "", nil, err
					}
					*dest = append(*dest, value)
				}
			}
		}
		if ports := values["ports"]; ports != nil && ports.Kind == yaml.SequenceNode {
			for i, n := range ports.Content {
				if n.Kind != yaml.ScalarNode {
					continue
				}
				if n.Tag != "!!str" {
					return "", nil, refusal(n, "Quote short port mappings")
				}
				mapping, err := shortRunPort(n)
				if err != nil {
					return "", nil, err
				}
				ports.Content[i] = mapping
			}
		}
		kept := []*yaml.Node{}
		for i := 0; i < len(service.Content); i += 2 {
			key := service.Content[i].Value
			if key != "container_name" && key != "command" && key != "entrypoint" {
				kept = append(kept, service.Content[i], service.Content[i+1])
			}
		}
		service.Content = kept
		extras[name] = e
	}
	data, err := yaml.Marshal(&document)
	if err != nil {
		return "", nil, &Diagnostic{Reason: "Invalid YAML document"}
	}
	return string(data), extras, nil
}
func shortRunPort(n *yaml.Node) (*yaml.Node, error) {
	value, err := literal(n)
	if err != nil {
		return nil, err
	}
	value, protocol, hasProtocol := strings.Cut(value, "/")
	if !hasProtocol {
		protocol = "tcp"
	}
	if protocol != "tcp" && protocol != "udp" {
		return nil, refusal(n, "Short ports use tcp or udp")
	}
	targetAt := strings.LastIndex(value, ":")
	if targetAt < 0 {
		return nil, refusal(n, "Short ports require published:target")
	}
	target, host := value[targetAt+1:], value[:targetAt]
	published, ip := host, ""
	if at := strings.LastIndex(host, ":"); at >= 0 {
		ip, published = host[:at], host[at+1:]
		ip = strings.TrimPrefix(strings.TrimSuffix(ip, "]"), "[")
	}
	for _, part := range []string{target, published} {
		number, err := strconv.Atoi(part)
		if err != nil || number < 1 || number > 65535 || strconv.Itoa(number) != part {
			return nil, refusal(n, "Short ports require decimal ports from 1 to 65535")
		}
	}
	result := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Line: n.Line, Column: n.Column}
	for _, pair := range [][2]string{{"target", target}, {"published", published}, {"protocol", protocol}, {"host_ip", ip}} {
		if pair[0] == "host_ip" && pair[1] == "" {
			continue
		}
		result.Content = append(result.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: pair[0]}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: pair[1]})
	}
	return result, nil
}
