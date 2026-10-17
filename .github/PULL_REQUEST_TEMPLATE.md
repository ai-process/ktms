### Summary of Changes

<!-- Briefly describe the problem solved or feature added. Focus on *why* and architectural decisions. -->

### Related Issues

<!-- E.g., Fixes #123 or Fixes ai-process/docs#45 -->

### Type of Change

- [ ] `feat`: New feature or capability
- [ ] `fix`: Bug fix
- [ ] `perf`: Performance improvement
- [ ] `refactor`: Code change that neither fixes a bug nor adds a feature
- [ ] `docs`: Documentation updates
- [ ] `chore`: Tooling, CI, dependencies, or maintenance

### Contributor Checklist

- [ ] Code passes static analysis: `go vet ./...`
- [ ] Race detector and unit test suite pass: `go test ./... -race`
- [ ] Protobuf sync: ran `make proto` if changes touched `proto/`
- [ ] Config parity verified: any new environment variables match across `internal/config/config.go`, `env.example`, and `README.md`
- [ ] Security hygiene: no vendor API keys, client keys, or encryption keyrings are logged or exposed
- [ ] Error contract adhered to: per-item errors use structured codes and reasons
- [ ] Comments follow the 3-line max rule explaining *why*, never *what*
- [ ] No AI attribution lines or `Co-Authored-By` markers in git commit messages or PR descriptions
- [ ] Documentation updated if runtime behavior or API surface changed
