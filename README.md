# cmo — Cert Manager Operator CLI

`cmo` installs [cert-manager](https://cert-manager.io) on a cluster, removes it again, and creates,
lists, inspects, renews and deletes its issuers and certificates. Each of those is one command,
with no YAML to write.

- **OpenShift:** installs the Red Hat cert-manager Operator through OLM. You can pin an operator
  version; cmo then approves only that version's InstallPlan.
- **Any other Kubernetes** (Kind, EKS, GKE, ...): server-side applies the upstream
  `cert-manager.yaml` release manifest. No Helm needed.

cmo detects which kind of cluster it is talking to. It replaces the bash scripts in
[cert-manager-scripts](https://github.com/sebrandon1/cert-manager-scripts) for everyday use.

## Quick start

Download `cmo` from the [releases](https://github.com/sebrandon1/cmo-cli/releases) (linux and
darwin, amd64 and arm64), or build it with `make install`. `go install
github.com/sebrandon1/cmo-cli@latest` works too, but names the binary `cmo-cli`.

```bash
cmo install                                         # install cert-manager and wait until it's ready
cmo issuer create ca my-ca --generate               # a root CA to sign with
cmo cert create web --issuer my-ca --dns web.example.com
cmo cert inspect web                                # subject, SANs, validity, key, serial
cmo status                                          # is everything healthy?
cmo doctor                                          # ...and if not, why
```

To clean up:

```bash
cmo cert delete --all-managed -A --yes     # every certificate cmo created, and its secret
cmo issuer delete --all-managed --yes      # every issuer cmo created
cmo uninstall --purge --yes                # cert-manager, its namespace and its CRDs
```

## Commands

| Command | What it does |
|---|---|
| `cmo install` | Install cert-manager: OLM on OpenShift, the release manifest elsewhere |
| `cmo uninstall` | Remove cert-manager; `--purge` also removes the CRDs and every cert-manager object |
| `cmo status` | Show the install method, version, deployments and resource counts |
| `cmo doctor` | Check access, the OLM catalog, deployments, the webhook, and every issuer and certificate that isn't Ready |
| `cmo issuer create selfsigned\|ca\|acme NAME` | Create an issuer (ClusterIssuer by default, Issuer with `-n`) |
| `cmo issuer list\|get\|delete` | List, show and delete issuers |
| `cmo cert create NAME --issuer X --dns ...` | Request a certificate and wait for it to be issued |
| `cmo cert list\|get\|delete` | List, show (with why it isn't ready) and delete certificates |
| `cmo cert renew NAME` | Reissue a certificate now |
| `cmo cert inspect NAME` | Decode the issued certificate |
| `cmo version`, `cmo completion` | Version and shell completion |

**[docs/commands.md](docs/commands.md)** has every command's flags.

### Things that work everywhere

- `-o table|json|yaml`, plus `-o wide` on the list commands. Data goes to stdout and progress to
  stderr, so `cmo status -o json | jq` always works.
- `-v` traces each API request the way kubectl would name it (`→ get certificates.cert-manager.io web -n default`).
- `--dry-run` on `install` and the `create` commands prints the YAML and changes nothing.
- `--timeout` controls how long a command waits. `--no-wait` returns as soon as the request is made.
- Destructive commands ask first. `--yes` skips the question, and without a terminal cmo refuses
  instead of guessing.
- Everything cmo creates is labeled `app.kubernetes.io/managed-by=cmo-cli`. `--all-managed` uses
  this label to delete cmo's objects and nothing else.

## Examples

```bash
# Pin an operator release on OpenShift (Manual approval; channel stable-v1.18)
cmo install --version v1.18.0

# A specific upstream release, or an air-gapped manifest, on Kubernetes
cmo install --version v1.20.2
cmo install --manifest ./cert-manager.yaml

# A namespaced self-signed Issuer, and an ECDSA certificate from it
cmo issuer create selfsigned local -n team-a
cmo cert create api -n team-a --issuer local --dns api.team-a.svc --key-algorithm ecdsa --key-size 384

# Bring your own CA key pair (a kubernetes.io/tls Secret in the cert-manager namespace)
cmo issuer create ca corp --secret corp-ca-keypair

# Let's Encrypt staging over HTTP-01 through nginx
cmo issuer create acme le-staging --server letsencrypt-staging --email me@example.com \
    --http01-ingress-class nginx

# A short-lived certificate, then renew it by hand
cmo cert create short --issuer my-ca --dns short.example.com --duration 2h --renew-before 1h
cmo cert renew short
```

## Requirements

- A kubeconfig for the cluster. cmo looks at `--kubeconfig`, then `$KUBECONFIG`, then
  `~/.kube/config`. `--context` picks a context.
- `install` and `uninstall` need cluster-admin. The issuer and certificate commands need rights to
  cert-manager's resources.
- No other tools: cmo doesn't call `oc`, `kubectl`, `helm` or `openssl`.

## Development

```bash
make build        # ./cmo
make test         # unit tests, plus test/integration (the binary against a fake API server)
make lint vet     # golangci-lint and go vet, including the e2e build tag
make test-e2e     # the full lifecycle against $KUBECONFIG (installs and uninstalls cert-manager)
make docs         # regenerate docs/commands.md
make help         # every target
```

CI runs on every pull request:

- `pre-main.yaml`: lint, tests with an 80% coverage floor, build, gosec, govulncheck, gitleaks.
- `e2e-kind.yaml`: the e2e suite on a Kind cluster from
  [quick-k8s](https://github.com/palmsoftware/quick-k8s).
- `e2e-ocp.yaml`: the e2e suite on OpenShift from
  [quick-ocp](https://github.com/palmsoftware/quick-ocp). PRs test 4.21; the nightly run tests
  4.20–4.22. It needs the `CRC_PULL_SECRET` repository secret; without it the job is skipped
  with a notice.

## Roadmap

These are features of cert-manager-scripts that are not ported yet:

- Pebble and fake-DNS harnesses for HTTP-01 and DNS-01 tests
- API server certificate rollout on OpenShift
- monitoring (ServiceMonitor, alerts)
- the IBU and OADP scenarios

Also planned: istio-csr, trust-manager, and Vault and Venafi issuers.

## License

Apache-2.0
