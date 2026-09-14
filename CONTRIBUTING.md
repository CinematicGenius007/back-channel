# Contributing to backchannel

backchannel is a small, self-hosted relay for text, files, and clipboard data. The
project values boring, inspectable code over framework-heavy abstractions.

## Before you change code

- Read `CLAUDE.md` for the project invariants and security decisions.
- Read the relevant protocol or design section before changing wire behavior.
- Keep the implementation standard-library-only. Do not add a Go module dependency
  without first opening an issue to discuss it.
- Keep frames additive. Existing clients, including v1 guest clients, must continue
  to work unless a change is explicitly versioned.
- Route permission decisions through `can`, `canTarget`, or `canSend` in `code/perms.go`.
- Preserve identical errors for an unknown channel and a channel the caller cannot see.

## Local checks

Run these from `code/`:

```sh
go test ./...
go test -race .
go vet ./...
go build -trimpath -o bch .
./build.sh
```

The build script writes six release binaries to `code/dist/`:

- macOS: arm64 and amd64
- Windows: arm64 and amd64
- Linux: arm64 and amd64

The generated directory is ignored and must not be committed.

## Tests

Add a focused test when behavior changes. Permission changes belong in the table-driven
matrix in `code/perms_test.go`. Hub behavior should use the loopback integration helpers
in `code/hub_test.go`. Crypto changes need both primitive tests and a relayed exchange
test in `code/e2e_test.go`.

For concurrency changes, the race test is required. For crypto or syscall-adjacent
changes, run the cross-build script as well.

## Documentation

Documentation at the repository root is canonical. Update the smallest relevant set:

- `README.md` for first-run and everyday behavior
- `ADMIN-GUIDE.md` for hub operations and permissions
- `PUBLIC-SERVER.md` for internet-facing deployment
- `PROTOCOL.md` for wire or HTTP behavior
- `ENCRYPTION.md` for end-to-end encryption behavior
- `WINDOWS-SETUP.md` for Windows-specific instructions

Write for a person following commands under pressure. State prerequisites, expected
output, failure modes, and security caveats directly. Do not promise features the code
does not provide.

## Pull requests

Include what changed, why, tests run, and any compatibility or migration impact. Do not
include passwords, session tokens, private keys, hub data directories, or generated
binaries in a patch.
