# Changelog

awxctl is `0.x`: the command line and JSON shapes can still change between releases. The release notes
are taken from this file (`scripts/release-notes.sh`).

## [Unreleased]

## [0.1.0] - 2026-11-11

First release.

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
- `stats jobs`, `stats hosts`, `get host-output`, runtime and template on
  `get host-jobs` rows.
- `doctor`: checks a controller for everything awxctl uses.
- `get inventories <name>` lists the inventory's sources.
- `meta.total` in list output; job output above the controller's display limit is
  downloaded; a host name shared by several inventories works in the read
  commands; numbers in API answers are kept exact (very large integers in task
  results no longer break decoding).
- Signed releases (checksums signed with the project's release key, SBOMs), `upgrade` with
  verification, third-party notices in every archive.
