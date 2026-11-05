# Contributing

Issues and pull requests are welcome. For anything larger than a small fix, open an issue first so we can agree on the approach.

## Setup

```sh
go test ./...
go run ./algodtop
```

Go 1.27 builds it. CI runs `go vet` and `go test -race` on Linux, macOS and Windows, builds every release platform, and checks that sources are ASCII. `main` only takes pull requests that pass CI, and every merge to `main` is a release.

## Conventions

- Use the Go standard library and the dependencies approved in DESIGN.md section 15. Anything else needs an issue first.
- DESIGN.md explains every decision. A change to behavior updates it in the same pull request.
- `internal/fakealgod` stands in for algod in tests. A change to what algod.top reads from algod needs a matching change there.
- `go test ./internal/tui -update` rewrites the terminal UI's golden files. Check their diff before you commit it.
- Match the style of the surrounding code: gofmt, short comments that say why, and ASCII only.
