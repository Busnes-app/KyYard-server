// Package agentskill holds the SKILL.md that teaches AI agents (OpenClaw, Hermes Agent) to use
// KyYard's MCP tools. Each instance serves the copy matching its own tools.
package agentskill

import _ "embed"

// KyYard is kyyard/SKILL.md, served at /agent/skills/kyyard/SKILL.md.
//
//go:embed kyyard/SKILL.md
var KyYard []byte
