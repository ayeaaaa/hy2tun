# Contributing

Thanks for considering a contribution.

## Requirements

- Go 1.26.10 or newer.
- Linux for the full test suite; Windows/OpenWrt changes should still compile.
- No Node.js or CDN build step; web assets are embedded.

## Before opening a pull request

Run:

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
make build
```

For documentation changes, keep `README.md` and `README.zh-CN.md` in sync and
check fenced code blocks. Every release must have a matching section in both
`CHANGELOG.md` and `CHANGELOG.zh-CN.md`; the release workflow refuses to publish
if either language is missing.

## Commit style

Use conventional commit prefixes where practical:

- `feat:`
- `fix:`
- `docs:`
- `test:`
- `refactor:`
- `chore:`

Keep commits focused. Do not include generated binaries, `dist/` archives,
test credentials, real server addresses, or private keys.

## Security

Do not report security vulnerabilities in a public issue. Follow
[`SECURITY.md`](SECURITY.md).
