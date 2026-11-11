# Changelog

awxctl is `0.x`: the command line and JSON shapes can still change between releases. The release notes
are taken from this file (`scripts/release-notes.sh`).

## [Unreleased]

### Security
- Over MCP the launch allowlist could be sidestepped: an agent could enable a schedule of a template that is
  not on the list, edit or rename templates, or change a project. Over MCP, templates, projects, schedules,
  credentials, inventory sources, surveys, workflow graphs and role grants are now read-only, a launch cannot
  pick another branch or inventory, and workflow approvals need `mcp --allow-approvals`.
- Relaunching an ad hoc command needs the write tier, like running one.

### Fixed
- `-e` and `--set` values that look like numbers are no longer re-typed: `1.10` stays `1.10`, a long ID keeps
  all its digits. `NAME:=JSON` forces JSON.
- On a template with a survey, extra variables that are not survey questions are refused when the template does
  not prompt for variables; a launch whose fields the controller ignored ends with exit 7, not as applied.

- A successful update of a text field with a number-like value no longer ends as "outcome unknown" (exit 7).
- A change the controller denies exits 8 and says why (it exited 1 with "some changes failed").
- An answer that is too large (256 MB) is an error, never a cut text; job output is no longer cut at 64 MB.
- A list whose next page is on another host name is an error that says so, not a quietly shorter list.
- Ctrl-C and a kill are handled: the audit record is completed (in flight: unknown, later steps: skipped) before
  the process ends; a step that was never sent is skipped, not unknown.
- Audit records with an unknown or unfinished step are never pruned; only awxctl's own record files are ever
  deleted; runs started by the MCP server never prune.
- Secrets in `extra_vars` written as YAML with braces are masked; text that cannot be parsed but names a
  secret is hidden whole. More names count as secret-looking (`pwd`, `*_pw`, `*_key`).
- `set workflow-graph` sends a node's `extra_data` whenever it holds a secret (a changed secret was not sent).
- `diagnose job`: errors the play ignored are not failures (counted as `ignored_errors`), a failed loop is not
  counted twice, a host name is only replaced as a whole word, unreachable hosts group together, hosts are
  counted from the failures, and it says when the controller is still storing the job's events.
- `meta.total` is present when nothing matches; `--limit 0` is refused; a misspelt `mode` is refused; a build
  from a commit after a release is not "upgraded" back to that release; an `http://` url gets a warning.
- MCP results start with a line marking them as data from the controller.

### Changed
- `diagnose job` lists each distinct failure once, with the hosts it hit (hosts that failed the same task
  with the same message share an entry), and its play, role, task, file and line.
- `diagnose job` on a job that timed out or was canceled names the last task that ran and the hosts that
  started it and never returned.

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
