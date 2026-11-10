# awxctl

A command-line tool for the AWX and Ansible Automation Platform (AAP) controller API,
built for agents and operators: one static binary, JSON by default, structured errors,
safe by default.

**Status: early development.** Today it can check a connection (`awxctl auth status`).
Reads, job actions, changes, an MCP server and a terminal UI follow.

## Supported controllers

- AWX (API under `/api/v2`)
- AAP 2.5 and newer through the platform gateway (controller API under `/api/controller/v2`)

awxctl detects which one it is talking to. Set `flavor: awx` or `flavor: aap` to pin it.

## Configuration

Flags override `AWXCTL_*` environment variables, which override the config file
`~/.config/awxctl/config.yaml` (keep it mode 0600):

```yaml
current_context: lab
contexts:
  lab:
    url: https://awx.example.com
    api_token: "<token>"
    permissions: readonly   # readonly | operator | write
    # ca_file: /path/to/ca.pem
    # flavor: auto          # auto | awx | aap
```

| Variable | Meaning |
|---|---|
| `AWXCTL_CONTEXT` | context to use |
| `AWXCTL_URL` | controller URL |
| `AWXCTL_API_TOKEN` | API token |
| `AWXCTL_USERNAME`, `AWXCTL_PASSWORD` | basic authentication, when no token is set |
| `AWXCTL_CA_FILE` | CA certificate (PEM) for a private or self-signed controller |
| `AWXCTL_INSECURE` | `1` disables TLS verification |
| `AWXCTL_FLAVOR` | `awx`, `aap` or `auto` |
| `AWXCTL_PERMISSIONS` | lower the context's tier (it can never raise it) |

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | error |
| 2 | not found |
| 3 | authentication failed |
| 4 | cannot reach the controller |
| 5 | bad flags, arguments or configuration |
| 6 | controller version not supported |
| 7 | a change was sent but its outcome is unknown: check before retrying |
| 8 | not permitted (permission tier, or denied by the controller) |

Errors are printed to stderr as `{"error": "...", "kind": "...", "code": N}`.

## Security model

- The permission tiers, the dry-run default and the allowlists in awxctl are guardrails
  against mistakes. They are not a security boundary: anything that can run awxctl as
  your user can also read the config file and call the API directly.
- Real enforcement is the token. Give each use a token for a controller user that has
  only the roles it needs; use a read-only user (system auditor) for reporting and agents
  that only look.
- awxctl never prints the token, never follows redirects (a redirect could carry the
  token to another host), and never retries a request that changes something.

## Build

```
make build
make test
```

## License

MIT
