# Security policy

## Reporting a vulnerability

Please report security problems privately, using GitHub's **Report a vulnerability** button on this
repository's Security tab. Do not open a public issue for a security problem.

Include what you found, how to reproduce it, and which version (`awxctl version`). You will get an
acknowledgement, and a fix or a decision as soon as it can be looked at.

## What matters most for this tool

awxctl holds a controller API token and can launch automation and change a controller, so these are the
areas where a report is most valuable:

- anything that prints, logs or stores a token, a password, a credential's inputs or a secret-looking
  variable (in output, in an audit record, or in the recorded command line);
- a change that happens without `--apply`, or a guard that can be bypassed (`--expect`, the permission
  tiers, the `allowed_templates` launch allowlist, the refusal of launch options a template would ignore);
- `awxctl upgrade` installing something that was not verified against the release's signed `checksums.txt`;
- the MCP server (`awxctl mcp`) offering a tool or a flag it should not (it is a guardrail for honest
  agent use, see the threat model below);
- a request sent to a host other than the configured controller (redirects and paging links are not
  followed to other hosts).

## Threat model

awxctl runs as your user, reads your config and token, and calls the controller API. Its dry run,
permission tiers, audit records, launch allowlist and the MCP server's tool list are **guardrails against
mistakes**, not a boundary against code that runs as the same user: that code can skip awxctl and use the
token directly (or read the config file and the state directory). Real enforcement is the controller side:
the roles of the token's user and the token's scope. Recommended:

- a separate token per use; for agents that only look, a read-scope token for a user with only the
  auditor role;
- `permissions: readonly` written out on contexts an agent uses. A context with no `permissions` line is
  readonly too, but then `AWXCTL_PERMISSIONS` in the environment (for example in an MCP client's `env`
  block) sets its tier; a tier that is written in the config can only be lowered by the environment;
- an `allowed_templates` list on any context an agent may launch from (over MCP nothing can be launched
  without one, and templates, projects, schedules and credentials are read-only there so the list cannot
  be sidestepped; an agent with a shell and a write-tier context is limited only by its token);
- the config file mode 0600 and the state directory owned by you.

Text that comes from the controller (names, job output, messages) is written by third parties. awxctl
escapes control characters in tables and marks MCP results as data, but whoever reads it, a person or an
agent, must treat it as data and not as instructions.

So a report is most valuable when awxctl does something its own documentation says it will not: prints a
secret, changes something without `--apply`, installs something unverified, or lets one of its guardrails
fail open for an honest caller. "A program running as the user can bypass the guardrails" is known and by
design.

## Supported versions

Fixes go into the latest release.
