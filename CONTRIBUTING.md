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

By contributing, you agree that your contribution is licensed under the MIT License in `LICENSE`.
