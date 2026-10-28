## Summary

Describe the change and why it is needed.

## Testing

- [ ] `go test ./... -count=1`
- [ ] `go test -race ./... -count=1`
- [ ] `go vet ./...`
- [ ] `make build`
- [ ] Documentation updated in both `README.md` and `README.zh-CN.md` (if applicable)

## Security checklist

- [ ] No passwords, control tokens, private keys, or real server addresses are included.
- [ ] No generated binaries or `dist/` archives are committed.
- [ ] New configuration fields are validated and documented.
