# awxctl

A command-line tool for the AWX and Ansible Automation Platform (AAP) controller API,
built for agents and operators: one static binary, JSON by default, structured errors,
and safe by default (nothing changes without `--apply`).

**Status: early (0.x).** The command set below works and is tested against AWX 24.6 and
AAP 2.7; commands and JSON shapes can still change between releases. Releases are signed.

```
awxctl diagnose job 1234                         # why did it fail, in one call
awxctl get jobs --failed --since 24h --group-by template
awxctl get job-output 1234 --grep 'fatal|FAILED' --context 3
awxctl get host-jobs web1 --failed
awxctl get host-output web1 --template "Patch servers"   # only this host's part of the last run
awxctl stats jobs --since 7d                     # runs, failure rate, runtimes per template
awxctl launch "Patch servers" --limit web1       # dry run: shows what would be sent
awxctl launch "Patch servers" --limit web1 --apply --wait
```

## Install

**From a release** (Linux, macOS, Windows; amd64 and arm64): download the archive for your
platform from the [Releases](../../releases) page, check it (see
[Releases and upgrade](#releases-and-upgrade)), and put `awxctl` on your `PATH`. Each archive
also holds the license and the third-party notices.

**From source** (Go 1.26 or newer):

```sh
git clone https://github.com/mwtrigg/awxctl
cd awxctl
make build            # or: CGO_ENABLED=0 go build -o awxctl ./cmd/awxctl
```

Then create the config file below and run `awxctl auth status`.

## Supported controllers

| Product | Version | Controller API | Identity and access | Status |
|---|---|---|---|---|
| AWX | 24.6 | `/api/v2` | controller | tested |
| AAP | 2.7 (controller 4.8) | `/api/controller/v2` | platform gateway, `/api/gateway/v1` | tested, except running jobs |
| AAP | 2.5, 2.6 | `/api/controller/v2` | platform gateway | same layout as 2.7; not tested |
| AAP | 2.4 and older | `/api/v2` | controller | treated like AWX; not tested |

"Tested" means the commands were run against a real instance of that version. The
AAP instance used had no subscription, so launching and everything else that starts
a job is tested on AWX only. A version that is not in the table may well work:
`awxctl doctor` reads every API path awxctl depends on (GET requests only) and
reports what is missing or different, and says whether the version is a tested one. Run it first on a new
version; the table is updated when a version has been checked.

awxctl reads the API index (`/api/`) to find out which one it is talking to and where
its services are, so a moved prefix is followed, not guessed. Set `flavor: awx` or
`flavor: aap` to pin it.

On AAP 2.5 and newer, identity and access moved from the controller to the platform
gateway (`/api/gateway/v1`): users, teams, organizations, tokens, role definitions and
role assignments. The controller's copies still answer but are deprecated. awxctl
reads and writes those kinds on the gateway when there is one, and on the controller
for AWX; everything else (jobs, templates, inventories, projects, ...) is the
controller's on both. Users, teams and organizations are read-only in awxctl.
Event-Driven Ansible and Automation Hub are not covered.

## Configuration

Flags override `AWXCTL_*` environment variables, which override the config file
`~/.config/awxctl/config.yaml` (keep it mode 0600):

```yaml
current_context: lab
contexts:
  lab:
    url: https://awx.example.com
    api_token: "<token>"
    permissions: readonly          # readonly | operator | write. Write this line: without it the
                                   # tier is readonly, but AWXCTL_PERMISSIONS in the environment can set it
    # ca_file: /path/to/ca.pem     # private or self-signed controller
    # flavor: auto                 # auto | awx | aap
    # mode: dry-run                # dry-run (default) | apply
    # allowed_templates: ["Patch servers", "id:42"]   # what may be launched
    # default_limit: 100           # rows a list returns unless --limit says otherwise
    # output: json                 # default output format: json | jsonl | table | csv
    # state_dir: ~/.local/state/awxctl   # audit records; state_keep: 200 newest
```

| Variable | Meaning |
|---|---|
| `AWXCTL_CONTEXT` | context to use |
| `AWXCTL_URL`, `AWXCTL_API_TOKEN` | controller URL and token |
| `AWXCTL_USERNAME`, `AWXCTL_PASSWORD` | basic authentication, when no token is set |
| `AWXCTL_CA_FILE` | CA certificate (PEM) |
| `AWXCTL_INSECURE` | `1` disables TLS verification |
| `AWXCTL_FLAVOR` | `awx`, `aap` or `auto` |
| `AWXCTL_PERMISSIONS` | the tier of a context that has no `permissions` line; on a context that has one it can lower the tier but never raise it |
| `AWXCTL_MODE` | `dry-run` or `apply` |
| `AWXCTL_OUTPUT` | default output format: `json`, `jsonl`, `table` or `csv` (a context can also set `output:`) |
| `AWXCTL_STATE_DIR`, `AWXCTL_STATE_KEEP`, `AWXCTL_BEFORE_STATE` | audit records: where, how many, on/off |

`awxctl auth status` shows what a context reaches; `awxctl context list` and
`awxctl context use <name>` manage contexts.

## Reading

`awxctl get <kind>` lists, `awxctl get <kind> <name-or-id>` shows one object with every
field. Kinds: `jobs`, `unified-jobs` (every run of any type), `workflow-jobs`,
`project-updates`, `inventory-updates`, `ad-hoc-commands`, `system-jobs`,
`workflow-approvals`, `job-templates`, `workflow-templates`, `inventories`, `hosts`,
`groups`, `inventory-sources`, `projects`, `credentials` (never their secrets),
`credential-types`, `execution-environments`, `schedules`, `organizations`, `users`,
`teams`, `tokens` (never their values), `role-definitions`, `role-user-assignments`,
`role-team-assignments`,
`notification-templates`, `notifications`, `labels`, `activity`, `instances`,
`instance-groups`.

| Flag | Meaning |
|---|---|
| `--filter KEY=VALUE` | any API filter, passed through (repeatable): `status=failed`, `name__icontains=web` |
| `--name TEXT`, `--search TEXT` | name contains; free-text search |
| `--since`, `--until` | `30m`, `24h`, `7d`, a date or RFC 3339 (job kinds, activity) |
| `--failed` | only failed runs |
| `--limit N` | most rows (default 100; `-1` = all); `meta.limit_reached` says when there are more |
| `--order FIELD` | sort, `-` prefix for descending |
| `--fields a,b,c` | keep only these fields of each row (every command, every format) |
| `--tz ZONE` | show times in a zone (`America/New_York`, `local`; default UTC); also the zone of a `--since` written without one. `AWXCTL_TZ` sets it |
| `--wide` | add the large fields a list leaves out (`extra_vars`, `changes`); one object always shows those. Variables of hosts, groups and inventories are never part of `get <kind>`: use `get variables` |
| `--group-by FIELD` | count rows per value instead of listing them |
| per kind | e.g. `--template`, `--inventory`, `--status`, `--project`, `--organization`; `get jobs --host H` (runs a host took part in); `get job-templates --playbook patch.yml` (templates whose playbook path contains it) |

Objects are addressed by name or ID. `id:12` and `name:12` force one reading; a name
that matches several objects is an error that lists their IDs. Hosts and groups take
`--inventory` to say which inventory the name is in.

About jobs:

| Command | Answers |
|---|---|
| `diagnose job <id>` | why it failed, best effort: verdict, each distinct failure with its role, task, file and hosts; where a job stopped and why (timeout, cancel, play ended early); error lines from the output; workflow nodes. `--compare-last-success` adds what differs from the last good run of the template (revision, limit, inventory, variables by name, host count, runtime) |
| `get job-output <id>` | the output, narrowed with `--tail`, `--head`, `--grep`/`--context`, saved with `--save`, streamed with `--follow --text` |
| `get job-events <id>` | task results: `--failed`, `--changed`, `--host`, `--task` |
| `get job-hosts <id>` | per-host counts (ok, changed, failures, unreachable) |
| `get host-jobs <host>` | the jobs that ran against a host, with the host's result and the job's runtime (`--template`, `--failed`, `--since`) |
| `get host-output <host>` | one host's part of a job's output: the last job, the last of `--template`, or `--job` |
| `stats jobs` | runs, failure rate and runtimes (min, average, median, p95, max) per template, day, status, ... |
| `stats hosts --inventory <inv>` | per host: jobs, failures, unreachable, last job |
| `get workflow-nodes <id>` | the jobs a workflow started (`--template` for a workflow template) |
| `get launch-info <template>` | what a template prompts for, its defaults and survey questions |
| `wait <id>` | block until it ends; exit 0 only if successful |

About everything else:

| Command | Answers |
|---|---|
| `get inventories <name>` | one inventory: host and group counts, its kind, and `sources`: where its hosts come from (type, project, file, last sync) |
| `get host-facts <host>` | the facts last gathered for a host (`--fact ansible_distribution`) |
| `get variables hosts\|groups\|inventories <name>` | its variables; `--merged` shows what a host ends up with and where each value comes from |
| `get project-files <project>` | the playbooks and inventory files of a project |
| `get schedule-runs <schedule>` | the next runs (`--rrule` previews a rule before you save it) |
| `get survey <template>` | the survey, in the form `set survey` takes (`--save` to edit it) |
| `get workflow-graph <workflow>` | nodes and links, in the form `set workflow-graph` takes |
| `get access <kind> <name>` | who has which role on an object |
| `get activity` | who changed what: time, actor, operation, object type and name. `--object-type inventory,host`, `--not-object-type job` (job launches fill it otherwise), `--group-by object\|operation\|actor` |

Output is `{"data": [...], "meta": {...}}`; `-o table|jsonl|csv` change the format and
`--field NAME` prints one field per line. The default format is JSON, for agents and scripts.
For tables at your own terminal, set `AWXCTL_OUTPUT=table` in your shell profile (or `output:
table` on a context); `-o` still wins, and the MCP server always asks for JSON. To keep agents
that inherit your profile on JSON, set it only for interactive use, for example
`[ -z "$CLAUDECODE" ] && export AWXCTL_OUTPUT=table`. In a list, `meta.total` is how many objects
match on the server and `meta.count` how many rows came back, so "how many failed
jobs this week" is `awxctl get jobs --failed --since 7d --limit 1` and reading
`meta.total`.

Things that are normal on a real controller and handled:

- The same host name in several inventories: `get host-jobs` and `get host-output`
  cover all of them (each row says which inventory); commands that change a host
  ask for `--inventory`.
- Job output above the controller's display limit (1 MB by default) is downloaded
  whole and then narrowed locally, so `--tail` and `--grep` still work.
- `get unified-jobs` includes every project and inventory sync, which can far
  outnumber playbook jobs; `get jobs` is playbook jobs only.
- Listing jobs is slow on a busy controller (about 50 rows a second): prefer
  filters and small limits to `--limit -1`.

## Changing things

Every changing command is a **dry run unless `--apply`** (alias `--yes`) is given, or
the context says `mode: apply` (then `--dry-run` forces a dry run). The dry run prints
exactly what would be sent. `--expect N` refuses to run unless exactly N objects would
change.

| Command | Tier | Does |
|---|---|---|
| `launch <template>` | operator | launch a job template (`--workflow`: a workflow template); `--limit`, `--tags`, `--skip-tags`, `-e NAME=VALUE`, `--extra-vars-file`, `--inventory`, `--check`, `--wait`, `--follow` |
| `relaunch <job-id>` | operator | run again; `--hosts failed` only on the hosts that failed (an ad hoc command: write) |
| `cancel <job-id>...` | operator | cancel running jobs |
| `sync projects\|inventory-sources <name>...` | operator | start a sync |
| `approve`, `deny <approval-id>...` | operator | answer workflow approvals |
| `enable`, `disable hosts\|schedules <name>...` | operator | switch hosts or schedules on and off |
| `create <kind> [name] --set FIELD=VALUE` | write | create an object; `--file`, `--set-file FIELD=PATH` |
| `update <kind> <name>... --set FIELD=VALUE` | write | change fields; only those that differ are sent |
| `delete <kind> <name>...` | write | delete; the dry run shows what goes with it |
| `attach`, `detach <kind> <name>... --to KIND=NAME` | write | hosts and groups of a group, credentials of a job template, a notification template (`--on started\|success\|error`) |
| `copy <kind> <name> --name NEW` | write | copy a template, inventory, project, credential, ... |
| `set survey <template> --file \| --remove` | write | replace or remove a survey |
| `set workflow-graph <workflow> --file` | write | make a workflow's nodes and links match a file |
| `grant`, `revoke <role> --user\|--team [--on KIND=NAME]` | write | give or take a role |
| `token create --description --scope [--user]` | write | create an API token (delete with `delete tokens <id>`) |
| `run command --inventory --limit --credential --module` | write | an ad hoc command |
| `api post\|patch\|put\|delete <path>` | write | any API path (`api get` reads) |

Workflows are edited as a whole, from a file, instead of node by node:

```yaml
nodes:
  - id: build
    template: Build image
    on_success: [approve]
    on_failure: [notify]
  - id: approve
    approval:
      name: Release this build
      timeout: 3600
    on_success: [deploy]
  - id: deploy
    template: Deploy
  - id: notify
    template: Send alert
```

`awxctl get workflow-graph <workflow> --save graph.yml` writes the current graph in
this form. Edit it, then `awxctl set workflow-graph <workflow> --file graph.yml` works
out what to create, change, link and delete (nodes are matched by `id`) and shows
every step before `--apply` does any of them. Cycles and unknown names are refused.

A new token's value is shown once, in the result of `token create --apply` (or
written to a file with `--save`), and is never in the dry run or the audit record.

Launch safety:

- An option the template does not prompt for is **refused**. The controller would
  ignore it silently, and an ignored `--limit` means a run against every host.
- `--check` is Ansible check mode: a real job. `--dry-run` sends nothing.
- `allowed_templates` in the context limits what `launch` and `relaunch` accept (entries are names or
  `id:<n>`; an ID cannot be taken over by renaming another template).
- On a template with a survey, `-e` names that are not survey questions are refused unless the template
  also prompts for variables. If the controller still reports ignored fields after a launch, the command
  ends with exit 7 and the job's ID, not as a success.
- `-e` and `--set` values are text unless they are `true`, `false`, `null`, a plain number, or a JSON
  list or object: `-e version=1.10` sends the text `1.10`. `NAME:=JSON` forces JSON (`-e count:=3`,
  `-e version:='"1.10"'`).
- Passwords a template asks for at launch are not supported.

Audit records: an applied change first writes a record (what is sent, the state
before, secrets masked) to `~/.local/state/awxctl`, then updates it with each outcome.
`awxctl state list` and `awxctl state show <file>` read them. There is no undo
command; the record is what you would restore from by hand. If the record cannot be
written, nothing is sent. A change whose outcome is unknown (timeout, 502) stops the
run with exit 7: check before retrying, awxctl never retries a change.

Secrets: values of secret-looking fields are shown as `******` in all output and
records, including inside `variables` and `extra_vars` text. "Secret-looking" goes by the name:
it contains `pass`, `pwd`, `secret`, `token`, `private`, `vault`, `authorize`, `api_key` or
`become_`, or has `key` or `pw` as a whole word between underscores (`ssh_key`, `db_pw`), or is
`inputs`. A secret under another name is not recognised. A value encrypted with Ansible Vault is
shown as `<vaulted>` whatever its name (it is no secret to show, but dozens of lines of nothing). Give secrets with `--set-file FIELD=PATH` or `--extra-vars-file` (`-` reads
stdin), not on the command line.

## For agents

- `awxctl llm capabilities` prints everything as one JSON document: conventions,
  commands with flags, kinds with fields and filters, exit codes.
- `awxctl mcp` serves the same commands as an MCP server over stdio:

  ```json
  {"mcpServers": {"awx": {"command": "awxctl", "args": ["-c", "lab", "mcp"]}}}
  ```

  Read-only by default. With `--allow-writes` the changing tools appear, and each is
  still a dry run unless called with `apply: true`.

  Over MCP a template can only be launched or relaunched if it is in
  `allowed_templates`, and only as it is defined (no other branch or inventory). So
  that the list holds, everything that decides what runs is read-only over MCP: job
  and workflow templates, projects, schedules, credentials, inventory sources,
  surveys, workflow graphs and role grants. Hosts, groups, inventories and labels can
  be changed. Workflow approvals can be answered only if the server was started with
  `--allow-approvals`. Never offered: ad hoc commands, `api`, tokens, contexts,
  `upgrade`, and flags that read or write local files. Each call runs awxctl as a
  child process that enforces the same rules.

  The allowlist is a guardrail for MCP and for the `launch` and `relaunch` commands.
  An agent that can run awxctl in a shell with a write-tier context can edit a
  template like any operator: what such an agent may do is set by its token.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | error, or a job that did not end successful (`--wait`, `wait`) |
| 2 | not found |
| 3 | authentication failed |
| 4 | cannot reach the controller |
| 5 | bad flags, arguments or configuration (also a context that is not defined) |
| 6 | controller version not supported |
| 7 | a change was sent but its outcome is unknown: check before retrying |
| 8 | not permitted (permission tier, allowlist, or denied by the controller) |

Errors are printed to stderr as `{"error": "...", "kind": "...", "code": N}`.

## Security model

- The permission tiers, the dry-run default, the allowlist and the MCP restrictions are
  guardrails against mistakes. They are not a security boundary: anything that can run
  programs as your user can read the config file and call the API directly.
- Real enforcement is the token. Give each use a token for a controller user that has
  only the roles it needs; use a read-only user (system auditor) for reporting and for
  agents that only look.
- Text that comes from the controller (names, job output, messages) is written by
  third parties. awxctl escapes control characters in tables and marks MCP results as
  data, but an agent must still treat it as data, not instructions.
- awxctl never prints the token, never follows redirects (a redirect could carry the
  token to another host), and only follows paging links on the same host.
- Job extra variables are visible to anyone who can read the job in the controller;
  masking in awxctl's own output does not change that.

## Releases and upgrade

Releases are built by GitHub Actions from a `v*` tag. Every release has a `checksums.txt` and a
signature of it, `checksums.txt.sig`, made with the project's release key
([`docs/release-signing-key.pub`](docs/release-signing-key.pub), fingerprint
`SHA256:JcxegRoINrLo8XEppZs2uirJPaVY20R2BN0jtugTZTY`). The key file is in the repository, not
in the archives: fetch it from there, and compare its fingerprint (`ssh-keygen -lf`) with the one
above. To check a download by hand:

```sh
echo "awxctl-release $(cat docs/release-signing-key.pub)" > allowed_signers
ssh-keygen -Y verify -f allowed_signers -I awxctl-release -n awxctl-release -s checksums.txt.sig < checksums.txt
sha256sum -c --ignore-missing checksums.txt
```

`awxctl upgrade` does the same before it replaces itself: it downloads the latest release, verifies
the signature against the key compiled into the binary and the archive against the signed checksums,
never installs an older version, starts the new binary once and puts the old one back if it does not
run. `awxctl upgrade --check` only looks. `upgrade` sends no credentials: it works when the releases
can be read without logging in. A build made from source after a release is never "upgraded" back to it.

The license and the notices of the libraries awxctl is built with are in `LICENSE` and
`THIRD_PARTY_NOTICES.md`, and travel in every release archive.

## Build

```
make build
make test
```

## License

MIT
