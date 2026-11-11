# Changelog

## [Unreleased]

First working command set (no release yet).

- Connection: contexts, AWX / AAP gateway detection, `auth status`.
- Reads: `get <kind>` for 31 kinds, job output, events, per-host results, host
  history, workflow nodes, launch info, `diagnose job`, `wait`.
- Changes, a dry run unless `--apply`: launch, relaunch, cancel, sync, approve, deny,
  enable, disable, create, update, delete, attach, detach, ad hoc commands, raw `api`.
- Permission tiers, launch allowlist, `--expect`, audit records with masked secrets.
- `llm capabilities` and an MCP server (`mcp`).
- AAP gateway: the API index is read to find the services; users, teams,
  organizations, tokens and role assignments are read and written on the platform
  gateway when there is one.
- Tokens (`token create`, `get tokens`, `delete tokens`), role grants (`grant`,
  `revoke`, `get access`).
- Host facts, variables with `--merged`, project files, schedule previews.
- `copy`, survey editing (`get survey`, `set survey`), notification templates on
  `attach`/`detach`.
- Workflow editing from a file (`get workflow-graph`, `set workflow-graph`).
- Users, teams and organizations are read-only.
