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

## Install

Download `cmo` from the [releases](https://github.com/sebrandon1/cmo-cli/releases) (linux and
darwin, amd64 and arm64), install it with
`go install github.com/sebrandon1/cmo-cli/cmd/cmo@latest`, or build it with `make install`.

## Quick start

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
| `cmo install`, `cmo upgrade`, `cmo uninstall` | Install cert-manager (OLM on OpenShift, the release manifest elsewhere), upgrade it, or remove it; `--purge` also removes the CRDs |
| `cmo status`, `cmo doctor` | Show the install method, version and health; diagnose it when something isn't Ready |
| `cmo issuer create selfsigned\|ca\|acme` | Create an issuer (ClusterIssuer by default, Issuer with `-n`) |
| `cmo issuer list\|get\|delete` | List, show and delete issuers |
| `cmo cert create`, `cmo cert list\|get\|delete` | Request a certificate and wait for it; list, show and delete them |
| `cmo cert renew`, `cmo cert inspect`, `cmo cert export` | Reissue a certificate, decode the issued one, or write it and its key to files |
| `cmo pebble install\|issuer\|uninstall` | Run Pebble, an ACME test server, in the cluster to try ACME issuers without a public CA |
| `cmo version`, `cmo completion` | Version and shell completion |

## Documentation

- [docs/usage.md](docs/usage.md) — output formats, global flags, examples and requirements.
- [docs/commands.md](docs/commands.md) — every command and its flags.
- [docs/development.md](docs/development.md) — build, test and CI.
- [docs/roadmap.md](docs/roadmap.md) — what isn't ported from cert-manager-scripts yet.

## License

Apache-2.0
