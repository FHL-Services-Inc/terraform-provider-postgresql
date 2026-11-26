# Contributing

Issues and pull requests are welcome. For anything beyond a small fix, open an
issue first so the change can be discussed before you write it.

## Build and check

You need Go (the version is in [`.go-version`](.go-version)).

```sh
make build   # gofmt check, then go install
make test    # unit tests
make vet
make lint    # the same golangci-lint version as CI
```

`make fmt` formats the code.

Acceptance tests run against a real PostgreSQL server: `make testacc` uses
Docker, `make testacc_container` uses Apple's `container` CLI or Docker. The
setup, the superuser and non-superuser (RDS-like) profiles and how to run a
subset are described in [Developing the Provider](README.md#developing-the-provider)
and in the scripts in [`tests/`](tests/).

## Pull requests

- Link the issue the PR fixes or implements.
- Add an entry under `## [Unreleased]` in [`CHANGELOG.md`](CHANGELOG.md), with
  the issue or PR number.
- If you change a resource or data source, update its page in
  [`website/docs/`](website/docs/) in the same PR.

CI runs `make test`, `make vet`, golangci-lint and the acceptance suite on
PostgreSQL 12, 16 and 18, each with the superuser and the non-superuser
profile. PRs that only touch documentation skip the acceptance tests. To test
every supported PostgreSQL version (12 to 18), ask a maintainer to add the
`full-matrix` label to the PR.

## Security issues

Don't open a public issue; see [SECURITY.md](SECURITY.md).
