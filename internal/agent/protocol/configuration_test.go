package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

var configNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func configTarget() InspectionTarget {
	return InspectionTarget{ContainerID: strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), CreatedUnix: 1700000000}
}

func validConfiguration() ContainerConfiguration {
	stop := 30
	return ContainerConfiguration{
		Target:     configTarget(),
		ObservedAt: configNow,
		Name:       "web",
		Image:      ImagePull{Reference: "ghcr.io/acme/web:1", Digest: "sha256:" + strings.Repeat("c", 64), Tag: "ghcr.io/acme/web:1"},
		ImageID:    "sha256:" + strings.Repeat("b", 64),
		Command:    []string{"serve", "--port", "80"},
		Entrypoint: []string{"/entry.sh"},
		User:       "1000:1000", WorkingDir: "/app", Hostname: "web",
		Env:            []EnvEntry{{Name: "A", Value: "x=y\nz"}, {Name: "EMPTY"}},
		Labels:         map[string]string{"com.acme": "1"},
		Restart:        "on-failure",
		RestartRetries: 3,
		Ports:          []Port{{Host: 8080, Container: 80, Protocol: "tcp"}, {Container: 9000, Protocol: "udp"}},
		Mounts: []Mount{
			{Kind: MountVolume, Source: "data", Target: "/data"},
			{Kind: MountBind, Source: "/srv/x", Target: "/x", ReadOnly: true},
			{Kind: "tmpfs", Target: "/run"},
		},
		NetworkMode: "bridge",
		Networks:    []NetworkAttachmentSpec{{Name: "bridge"}, {Name: "app", Aliases: []string{"web"}, IP: "172.20.0.5"}},
		Resources:   Resources{NanoCPUs: 1e9, MemoryBytes: 1 << 30, MemorySwapBytes: 2 << 30, PidsLimit: 100},
		Healthcheck: &Healthcheck{Test: []string{"CMD", "true"}, IntervalSeconds: 30, TimeoutSeconds: 5, StartPeriodSeconds: 1.5, Retries: 3},
		Init:        true, TTY: true,
		CapAdd: []string{"NET_ADMIN"}, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"},
		ExtraHosts: []string{"db:10.0.0.2"}, DNS: []string{"1.1.1.1"},
		Devices:     []Device{{Host: "/dev/fuse", Container: "/dev/fuse", Permissions: "rwm"}},
		Log:         LogConfig{Driver: "json-file", Options: map[string]string{"max-size": "10m"}},
		StopSignal:  "SIGTERM",
		StopTimeout: &stop,
		Unsupported: []string{"ulimits", "env_truncated", "list_truncated:cap_add"},
	}
}

func TestConfigurationValid(t *testing.T) {
	c := validConfiguration()
	if err := c.Validate(configTarget(), configNow); err != nil {
		t.Fatal(err)
	}
	if _, ok := any(ConfigurationOpen{}).(InspectionOpen); !ok {
		t.Fatal("open is the inspection grant")
	}
}

func TestConfigurationRefusals(t *testing.T) {
	long := strings.Repeat("v", MaxDeploymentEnvValueBytes+1)
	cases := map[string]func(*ContainerConfiguration){
		"env count": func(c *ContainerConfiguration) {
			c.Env = nil
			for i := range 129 {
				c.Env = append(c.Env, EnvEntry{Name: fmt.Sprintf("K%d", i)})
			}
		},
		"env value":     func(c *ContainerConfiguration) { c.Env = []EnvEntry{{Name: "A", Value: long}} },
		"env name":      func(c *ContainerConfiguration) { c.Env = []EnvEntry{{Name: "1A"}} },
		"env duplicate": func(c *ContainerConfiguration) { c.Env = []EnvEntry{{Name: "A"}, {Name: "A"}} },
		"argv":          func(c *ContainerConfiguration) { c.Command = make([]string, 65) },
		"argv entry":    func(c *ContainerConfiguration) { c.Entrypoint = []string{strings.Repeat("a", MaxArgvEntryBytes+1)} },
		"cap_add":       func(c *ContainerConfiguration) { c.CapAdd = make([]string, 33) },
		"list entry":    func(c *ContainerConfiguration) { c.DNS = []string{strings.Repeat("a", MaxListEntryBytes+1)} },
		"networks": func(c *ContainerConfiguration) {
			c.Networks = make([]NetworkAttachmentSpec, 17)
			for i := range c.Networks {
				c.Networks[i].Name = fmt.Sprintf("n%d", i)
			}
		},
		"network ip": func(c *ContainerConfiguration) { c.Networks[1].IP = "nope" },
		"labels": func(c *ContainerConfiguration) {
			c.Labels = map[string]string{}
			for i := range 33 {
				c.Labels[fmt.Sprintf("l%d", i)] = "v"
			}
		},
		"log options": func(c *ContainerConfiguration) {
			c.Log.Options = map[string]string{}
			for i := range 17 {
				c.Log.Options[fmt.Sprintf("o%d", i)] = "v"
			}
		},
		"target":          func(c *ContainerConfiguration) { c.Target.ContainerID = strings.Repeat("d", 64) },
		"skew future":     func(c *ContainerConfiguration) { c.ObservedAt = configNow.Add(MaxClockSkew + time.Second) },
		"skew past":       func(c *ContainerConfiguration) { c.ObservedAt = configNow.Add(-MaxClockSkew - time.Second) },
		"restart":         func(c *ContainerConfiguration) { c.Restart = "sometimes" },
		"unsupported":     func(c *ContainerConfiguration) { c.Unsupported = []string{"bogus"} },
		"truncated field": func(c *ContainerConfiguration) { c.Unsupported = []string{"list_truncated:Cap-Add"} },
		"unsupported dup": func(c *ContainerConfiguration) { c.Unsupported = []string{"ulimits", "ulimits"} },
		"nul name":        func(c *ContainerConfiguration) { c.Name = "we\x00b" },
		"nul env":         func(c *ContainerConfiguration) { c.Env[0].Value = "a\x00b" },
		"nul label":       func(c *ContainerConfiguration) { c.Labels["k"] = "a\x00" },
		"nul argv":        func(c *ContainerConfiguration) { c.Command[0] = "a\x00" },
		"nul mount":       func(c *ContainerConfiguration) { c.Mounts[0].Source = "d\x00" },
		"mount kind":      func(c *ContainerConfiguration) { c.Mounts[0].Kind = "other" },
		"tmpfs source":    func(c *ContainerConfiguration) { c.Mounts[2].Source = "/x" },
		"mount target":    func(c *ContainerConfiguration) { c.Mounts[1].Target = "/data" },
		"port":            func(c *ContainerConfiguration) { c.Ports[0].Container = 0 },
		"port dup":        func(c *ContainerConfiguration) { c.Ports = append(c.Ports, c.Ports[0]) },
		"port proto":      func(c *ContainerConfiguration) { c.Ports[0].Protocol = "sctp" },
		"device perms":    func(c *ContainerConfiguration) { c.Devices[0].Permissions = "x" },
		"resources":       func(c *ContainerConfiguration) { c.Resources.MemoryBytes = -1 },
		"healthcheck":     func(c *ContainerConfiguration) { c.Healthcheck.Retries = -1 },
		"image id":        func(c *ContainerConfiguration) { c.ImageID = "sha256:short" },
		"frame size": func(c *ContainerConfiguration) {
			c.Command, c.Entrypoint = nil, nil
			for range MaxArgv {
				c.Command = append(c.Command, strings.Repeat("a", MaxArgvEntryBytes))
				c.Entrypoint = append(c.Entrypoint, strings.Repeat("a", MaxArgvEntryBytes))
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := validConfiguration()
			mutate(&c)
			if err := c.Validate(configTarget(), configNow); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestConfigurationEnvValueKeepsNewlineAndEquals(t *testing.T) {
	c := validConfiguration()
	c.Env = []EnvEntry{{Name: "K", Value: "a=b\nc=d"}}
	if err := c.Validate(configTarget(), configNow); err != nil {
		t.Fatal(err)
	}
}

func TestConfigurationResult(t *testing.T) {
	c := validConfiguration()
	if (ConfigurationResult{Request: "r1", Status: "ok"}).Validate() == nil {
		t.Fatal("ok without a result")
	}
	if err := (ConfigurationResult{Request: "r1", Status: "ok", Result: &c}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"busy", "unavailable"} {
		if err := (ConfigurationResult{Request: "r1", Status: s}).Validate(); err != nil {
			t.Fatal(s, err)
		}
		if (ConfigurationResult{Request: "r1", Status: s, Result: &c}).Validate() == nil {
			t.Fatal(s, "with a result")
		}
	}
	if (ConfigurationResult{Request: "r1", Status: "other"}).Validate() == nil || (ConfigurationResult{Status: "busy"}).Validate() == nil {
		t.Fatal("bad status or request")
	}
}

func TestConfigurationMaximalFitsFrame(t *testing.T) {
	c := validConfiguration()
	c.Env = nil
	for i := range MaxDeploymentEnvEntries {
		c.Env = append(c.Env, EnvEntry{Name: fmt.Sprintf("K%d", i), Value: strings.Repeat("v", 480)})
	}
	arg := strings.Repeat("a", 512)
	c.Command, c.Entrypoint = nil, nil
	for range MaxArgv {
		c.Command = append(c.Command, arg)
		c.Entrypoint = append(c.Entrypoint, arg)
	}
	c.Labels = map[string]string{}
	for i := range MaxLabels {
		c.Labels[fmt.Sprintf("label-%02d-%s", i, strings.Repeat("k", 230))] = strings.Repeat("v", MaxLabelBytes)
	}
	entry := strings.Repeat("e", 256)
	c.CapAdd, c.CapDrop, c.SecurityOpt, c.ExtraHosts, c.DNS = fill(entry), fill(entry), fill(entry), fill(entry), fill(entry)
	if err := c.Validate(configTarget(), configNow); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(ConfigurationResult{Request: "r", Status: "ok", Result: &c})
	if err != nil || len(b) >= MaxConfigurationFrameBytes {
		t.Fatal(len(b), err)
	}
}

func fill(s string) []string {
	out := make([]string, MaxListEntries)
	for i := range out {
		out[i] = s
	}
	return out
}
