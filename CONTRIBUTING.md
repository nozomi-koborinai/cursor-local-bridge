# Contributing

Thanks for helping. This file covers what you need to send a change.

## Set up

You need Go (the version in `go.mod`), git, and [golangci-lint v2](https://golangci-lint.run/). The tests do not need the Cursor CLI or a Cursor login. The E2E tests use a fake agent built from `internal/fakeagent`.

## Run the checks

Run the same checks as CI before you open a pull request:

```sh
golangci-lint fmt ./...
golangci-lint run ./...
go test -race -count=1 ./...
go test -race -count=1 -tags e2e ./e2e/...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

`golangci-lint fmt` applies gofumpt and goimports.

## Find your way around

- `internal/lifecycle` is the run state machine. It does no IO. Change run behavior here first, and add a row to its table test.
- `internal/daemon` owns each run in one goroutine and performs the effects that `lifecycle.Apply` returns.
- `internal/backend/acp` drives `agent acp`. `internal/backend/stream` drives `agent -p --output-format stream-json`.
- `internal/store` is the SQLite source of truth. Add a schema change as a new numbered file in `internal/store/migrations`. Do not edit a migration that has shipped.

## Send a change

- Keep a pull request to one purpose.
- Write commit subjects in the Conventional Commits form, for example `fix(daemon): close the agent when start fails`.
- Add or update a test that fails without your change.
- Write code, comments, and docs in English.

## Release a version

A tag publishes a release. After the release commit is on `main`, push a tag from it:

```sh
git switch main && git pull
git tag -a v0.1.0 -m v0.1.0
git push origin v0.1.0
```

The tag starts `.github/workflows/release.yml`. It runs every CI job, then GoReleaser publishes the GitHub Release with the archives and `checksums.txt`. When the repository secret `HOMEBREW_TAP_TOKEN` is set, GoReleaser also pushes `Casks/clb.rb` to `nozomi-koborinai/homebrew-tap`. Without the secret, the release still succeeds and the tap does not change. A tag with a prerelease suffix, such as `v0.2.0-rc.1`, publishes a prerelease and skips the tap.

To check the release config before you tag, run `goreleaser check` and `goreleaser release --snapshot --clean`. The workflows pin GoReleaser to one version. Change `release.yml` and the `release-config` job in `ci.yml` together.

By contributing, you agree that your contribution is licensed under the MIT License in `LICENSE`.
