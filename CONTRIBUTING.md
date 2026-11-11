# Contributing

Thanks for looking. A few things keep this tool dependable for both people and agents.

## Build and test

```sh
go vet ./... && go test ./...
make build
```

Tests run against a fake controller (`internal/fakeawx`, both the AWX and the AAP gateway layout); no
controller is needed. If you have a sandbox, try your change there too, on both flavors if you can, but
never against production with a token that can write.

## How changes get in

While a repository is private, pull requests are not needed: changes are signed commits straight to
`main`. Once it is public, pull requests (and Dependabot's) are the way in.

## Ground rules

- **Layers.** `internal/core` talks to the API and returns data; it never prints. `internal/cli` is flags
  and output only. A terminal UI will sit on `core` the same way.
- **Safe by default.** Reads change nothing. Every change is built as a plan (`internal/core/plan.go`)
  and run through `runPlan`, so it is a dry run until `--apply` and gets `--expect`, the permission tier,
  the audit record and verification. A new changing command must be added to `writeCommands` and to the
  dry-run test, which fails otherwise.
- **Flat, predictable output.** Results are flat objects with readable values; errors are
  `{error, kind, code}` with a stable exit code. No raw API pass-through (except the guarded `api`).
- **One description of the commands.** `awxctl llm capabilities` and the MCP tool list are generated from
  the command tree, and the kinds from `internal/core/resources.go`: write good help text and they follow.
- **Mask secrets.** Anything printed or recorded goes through `core.Mask`.
- **No secrets, no environment-specific data.** Never put a token, a hostname or an internal name in code,
  docs, tests or examples.
- **Both flavors.** AWX and AAP differ in where identity and access live (see the README). Assert request
  paths in fake-controller tests, not just results, and run `awxctl doctor` against a new version.

## Dependencies and licenses

awxctl is MIT-licensed. Keep the dependency list small and permissive (MIT, BSD, Apache-2.0). After
changing `go.mod`, regenerate the notices and commit them:

```sh
go run scripts/notices/main.go > THIRD_PARTY_NOTICES.md
```

CI fails if `THIRD_PARTY_NOTICES.md` or `go.mod`/`go.sum` are out of date.

By contributing you agree that your contribution is licensed under the MIT License, the same as the
rest of the project.

## Releasing (maintainers)

Releases are built and signed by `.github/workflows/release.yml` when a `v*` tag is pushed. The release
job runs GoReleaser and the release-notes script from the tagged commit next to the signing key, so
anyone who can push to `main` and create a tag can use the key: keep write access small.
One-time repository setup that the workflow relies on:

- A `release` environment holding the `AWXCTL_SIGNING_KEY` secret (an ed25519 SSH key; the
  public half is compiled in as `internal/cli/release_keys.go` and published as
  `docs/release-signing-key.pub`), restricted to `v*` tags once the repository is public (GitHub
  does not offer environment rules or rulesets on a private repository of a free account; until
  then the tag-on-main check in the workflow is the only control). Keep the secret out of the
  repository-level secrets, so that only a release job can read it. The environment does not
  require a reviewer (the owner chose not to): a `v*` tag on a commit that is on `main` is
  signed and published without anyone approving it.
- A tag ruleset for `v*` that blocks moving and deleting tags (add a `creation` rule with the owner as bypass if only the owner should be able to create them), and a branch ruleset for `main`. The release workflow refuses a tag whose commit is not on `main`, and its `verify` job runs the vet, the tests and the notices check before anything is signed. Those, and the tag ruleset, are the controls: anyone who can push to `main` and create a `v*` tag can cause a signed release, so keep write access to the repository small.
- Cut the final release on its own commit (the one that renames `[Unreleased]` to the version in CHANGELOG.md), not on the commit that already carries a release candidate tag.
- Private vulnerability reporting turned on (see SECURITY.md).

The version is part of every archive name (`awxctl_<version>_<os>_<arch>`) because the signed
`checksums.txt` has to bind each archive to its version; `awxctl upgrade` refuses a release whose
signed checksums do not list the archive named after its tag. GoReleaser, syft and govulncheck
are pinned to exact versions in the workflows; bump them on purpose, not by floating.
