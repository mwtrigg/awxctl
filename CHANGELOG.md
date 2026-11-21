# Changelog

From 1.0.0 on, awxctl follows semantic versioning: commands, flags, exit codes and JSON shapes change
incompatibly only with a new major version. The release notes are taken from this file
(`scripts/release-notes.sh`).

## [Unreleased]

### Added
- `get job-timing --by role`: the time per role, summed over the plays and batches that use it.
- `get job-timing`: `meta.summary` says what the run's time was spent on, each task counted once
  with its wall clock: changed, unchanged (a no-op for this run), skipped and failed; with the
  run's own runtime next to it. As a table the summary is a line on standard error.
- `get job-timing`: `meta.summary.recap_check` compares the task results, host by host, with
  the run's own recap, counted the way Ansible counts (includes, ignored failures, skipped meta
  tasks, loop items that changed): when every host matches, no task result is missing.
- `get job-timing`: `play_run` on task and play rows, and a PLAY column in the task table.

### Fixed
- `get job-timing`: a task of a play that runs its hosts in serial batches was one row for all
  batches, with a wall clock that spanned whatever ran between them. Each batch is now a row of
  its own.

## [1.3.0] - 2026-11-21

### Added
- `get job-events`: every task result has a `duration` (seconds on that host), and
  `--slowest N` returns the slowest task results of a run, slowest first: which host held the
  play.
- `get job-timing`: where a run's time went, one row per task (wall clock, host time, slowest
  host, how many hosts changed or not), per play (`--by play`) or per host (`--by host`).
- `diagnose job --recursive`: for a workflow, follows the nodes that did not succeed, also
  through workflows inside it, and diagnoses the jobs at the end (`leaf_failures`); the verdict
  names the first.

### Fixed
- `wait`: a wait that ran out while a poll was on its way exited 1 with "context deadline
  exceeded" instead of 9 (still running).

### Changed
- Notes about a result (two hosts sharing a name in `get host-jobs`) are part of the JSON, as
  `meta.notes`, instead of a line on standard error, so that a caller reading both streams
  still gets one JSON document. Table, CSV and JSONL output still send them to standard error.
- Dependency: go-pretty 6.9.2.

## [1.2.1] - 2026-11-17

### Changed
- A binary made with `go install github.com/mwtrigg/awxctl/cmd/awxctl@<version>` reports that version instead of `dev`.
- README: installing with `go install`.

## [1.2.0] - 2026-11-17

### Added
- `awxctl demo`: serves a made-up controller on localhost, to try awxctl without AWX or AAP.
- A demo image and animation in the README (`docs/awxctl.gif`, `docs/awxctl.png`), rendered from real output.

### Changed
- Table output (`-o table`), for people; JSON is unchanged:
  - `diagnose job` prints short labelled lines (what failed, where, the error, what differs from the last
    success) instead of one long sentence;
  - a plan shows what would be sent (`SENDS`) and only the columns that have something in them.
- No "url is http://" warning for a controller on this machine (localhost).

## [1.1.0] - 2026-11-16

The low-severity findings of the 2026-11-11 review.

### Changed
- `--wait-timeout` running out exits **9** (`kind: still_running`), not 1: the job has not failed, it is still
  running. `wait`, `launch --wait`, `relaunch --wait`, `sync --wait` and `get job-output --follow`.
- `AWXCTL_URL` no longer redirects a context that keeps its token in the config file to another host (the
  token would have gone there): give `AWXCTL_API_TOKEN` for that host too, or unset the variable.
- An HTTP 500 on a change is "outcome unknown" (exit 7), since the change may have been committed.
- `set workflow-graph`: if an approval node is created but its approval step is not, the run stops there
  (exit 7) instead of linking an empty node.

### Fixed
- A long list read while new objects arrive no longer shows the same object twice.
- `get host-jobs` and `get host-output` cover every host of a name (they stopped at 50).
- `get job-output --follow` on an output above the display limit polls slowly instead of downloading the whole
  output every few seconds.
- Audit records are flushed to disk before the change they describe is sent.

### Build
- The release notes are produced by the job that has no secrets; the signing job no longer runs repository
  scripts. Action versions bumped (attest-build-provenance, sbom-action, goreleaser-action).

## [1.0.0] - 2026-11-16

The first stable release. Nothing changes in behaviour from 0.3.1; from here the command line, the exit
codes and the JSON shapes are a promise.

### Added
- README: installing with `eget`.

### Known limits
- Launching and everything else that starts a job is tested against AWX only; on AAP it is built on the same
  API and checked by `awxctl doctor`, but has not been run there (see Supported controllers in the README).
- Applying a workflow graph on AAP and attaching notification templates have not been run against a real
  controller.
- Event-Driven Ansible, Automation Hub and gateway administration are out of scope.

## [0.3.1] - 2026-11-11

### Added
- `get jobs --host H` and `get unified-jobs --host H`: the runs a host took part in.

### Changed
- `get job-templates --playbook` matches part of the playbook path (it was an exact match).
- A value encrypted with Ansible Vault is shown as `<vaulted>` everywhere, whatever the variable is called
  and also with `--show-secrets` (a vault blob under a name that does not look secret used to print in full).
- `get hosts|groups|inventories` never prints variables, in a list or for one object, with or without `--wide`
  (they can run to pages); one object carries a `variables_hint` that points at `get variables`.

## [0.3.0] - 2026-11-11

From the first real use by an agent (14 calls and 60k characters to diagnose one job).

### Changed
- Lists leave out the large fields (`variables`, `extra_vars`, `extra_data`, `changes`, `permissions`); `--wide`
  adds them and a single object always has them. A list of 20 hosts went from about 80k characters to 6k.
- `diagnose job`: the failed hosts are `hosts_failed` (was `hosts`); `stalled` has a `reason` (timeout, canceled,
  play ended early) and the verdict no longer calls a play abort a hang.
- `get activity` shows the object's type and name (`object`, `object_name`, `related`, `related_name`); the
  `--object` filter is now `--object-type`, which takes several types.

### Added
- `--fields a,b,c` on every command: only those fields, in every format; also an MCP parameter on every tool.
- `--tz` / `AWXCTL_TZ`: times in a zone, and `--since`/`--until` without a zone are read in it.
- `diagnose job`: `errors`, the output lines that report an error outside a task ([ERROR], ERROR!, fatal) with
  the lines that explain them; the tail leaves timing-only lines out.
- `diagnose job --compare-last-success`.
- `get activity --not-object-type`.
- An unknown filter on `get <kind>` lists the filters that kind takes; over MCP a parameter of another kind is
  refused with the valid list, and the `get` tool says which filters go with which kind.

## [0.2.1] - 2026-11-11

### Added
- `AWXCTL_OUTPUT` and a per-context `output:` setting choose the default output format (json, jsonl, table,
  csv). `--output` still wins; the MCP server always uses JSON.

## [0.2.0] - 2026-11-11

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
- A context that is not defined exits 5 (it exited 1).
- Docs: an Install section; what `AWXCTL_PERMISSIONS` does exactly (it sets the tier of a context that has no
  `permissions` line, and can only lower one that has); what counts as secret-looking; what the MCP server
  offers.

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
