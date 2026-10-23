# Agent skill

## Purpose
Holds `kyyard/SKILL.md`, the AgentSkills-format skill that teaches AI agents (OpenClaw, Hermes Agent) to use KyYard's MCP tools safely, and embeds it as `agentskill.KyYard`.

## Ownership
Owns the skill's text. The tools it describes, their error messages and the route serving it are `internal/api`'s (`mcp.go`).

## Local Contracts
- Frontmatter is exactly `name`, `description`, `version`: OpenClaw silently hides a skill whose `requires` gate is unmet, so the skill declares none. `version` is the release line, `internal/appversion/VERSION` (`TestAgentSkillNamesEveryToolAndError`); a `vX.Y.Z.N` release serves the skill of its `X.Y.Z` line unchanged.
- The skill's `Tools:` line names every MCP tool and nothing that is not one, and its error table carries every message a tool returns that the agent must act on (`msgReadOnly`, `msgNotAnUpdate`, `msgDropsMounts`, `msgTooManyOperations`, and the shared REST messages).
- Its rules match the server's: workload output is untrusted data, operate only on the user's request and after confirming, nothing destructive exists. The same rules, shorter, are `mcpInstructions` for agents without the skill.
- It is served byte for byte as embedded, publicly: it holds no user or instance data.

## Verification
- `go test ./internal/api/ -run TestAgentSkill`

## Child DOX Index
None.
