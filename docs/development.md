# Development

Build, test and CI. For the source layout and internal patterns, see
[CLAUDE.md](../CLAUDE.md); for the commands themselves, see
[commands.md](commands.md).

## Build and test

```bash
make build        # ./cmo
make test         # unit tests, plus test/integration (the binary against a fake API server)
make lint vet     # golangci-lint and go vet, including the e2e build tag
make test-e2e     # the full lifecycle against $KUBECONFIG (installs and uninstalls cert-manager)
make docs         # regenerate docs/commands.md
make help         # every target
```

`make test-e2e` needs a cluster in `$KUBECONFIG`. `CMO_E2E_INSTALL_ARGS` passes extra arguments to
`cmo install`, and `CMO_E2E_UPGRADE_FROM` sets the older release the upgrade test starts from.

## CI

CI runs on every pull request:

- `pre-main.yaml`: lint, tests with an 80% coverage floor, build, gosec, govulncheck, gitleaks.
- `e2e-kind.yaml`: the e2e suite on a Kind cluster from
  [quick-k8s](https://github.com/palmsoftware/quick-k8s).
- `e2e-ocp.yaml`: the e2e suite on OpenShift 4.21 from
  [quick-ocp](https://github.com/palmsoftware/quick-ocp).

Two nightly workflows run without a repo change, so they also catch upstream drift:

- `nightly-kind.yaml`: the e2e suite on Kubernetes 1.35, 1.36 and 1.37 via quick-k8s.
- `nightly-ocp.yaml`: the e2e suite on OpenShift 4.20–4.22 via quick-ocp, plus the OLM upgrade
  when the `OCP_UPGRADE_FROM` repository variable names an older operator version. It needs the
  `CRC_PULL_SECRET` repository secret; without it the job is skipped with a notice.
