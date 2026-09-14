# Agent instructions for backchannel

This file is the short operational brief for coding agents. `CLAUDE.md` remains the
full project context and takes precedence when details are needed.

## Project shape

- Go module: `code/`
- Package: one flat `package main`
- Dependencies: Go standard library only
- Supported release targets: macOS arm64/amd64, Windows arm64/amd64, Linux arm64/amd64
- Canonical docs: repository root

## Required invariants

1. Do not add third-party modules without explicit approval.
2. Do not scatter authorization checks. Use the helpers in `code/perms.go` and update
   its tests for every permission change.
3. Keep unknown-channel and non-member errors byte-identical.
4. Never persist or replay `clip`, `keyreq`, or `keyshare` frames.
5. Never hash a password while holding `h.mu`.
6. Keep protocol changes additive and preserve v1 guest compatibility.
7. Treat ordinary channels as readable by the hub operator; do not describe transport
   TLS as end-to-end encryption.

## Workflow

1. Inspect the relevant code and docs before editing.
2. Make the smallest correct change with `apply_patch`.
3. Update documentation when observable behavior changes.
4. Run `go test ./...`, `go vet ./...`, and `go build -trimpath -o /tmp/bch .` from `code/`.
5. Run `go test -race .` for concurrency changes and `./build.sh` for crypto, terminal,
   filesystem, or platform changes.
6. Report changed files, checks run, and any residual risk.

## Documentation style

Use plain language, concrete commands, and honest limitations. Avoid filler, invented
features, and vague claims such as “secure by default” without naming the boundary.
