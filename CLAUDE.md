# CLAUDE.md

Guidance for working in this repository.

## Requirements

- Go 1.27+
- golangci-lint v2 for `make lint`
- For `make test-e2e`, a cluster in `$KUBECONFIG`. The test installs and uninstalls cert-manager.

## Build

```bash
make build             # ./cmo, version from git describe
make install           # build, then copy to $GOPATH/bin (rm first: stale macOS signatures)
make test              # all tests, including test/integration
make integration-test  # only the CLI-as-a-process tests, uncached
make test-e2e          # -tags e2e, the real lifecycle against $KUBECONFIG
make lint vet          # both also cover the e2e build tag
make docs              # regenerate docs/commands.md from --help (do this when a flag changes)
```

## Architecture

A Cobra CLI with the same layout as `sebrandon1/vsno` and `sebrandon1/succulent-cli`.
`lib/trace`, `lib/ui`, `lib/apply` and `lib/kube/client.go` were copied from vsno.

- `main.go`: passes the build version (`-ldflags "-X main.version=..."`) to `cmd.SetVersion`.
  `cmd/cmo/main.go` is an identical copy, so `go install .../cmd/cmo@latest` names the binary `cmo`
  rather than `cmo-cli`. Keep the two in step; `make build` and the release use the root one.
- `cmd/`: one file per command group.
  - `root.go`: the global flags. These are `--kubeconfig`, `--context`, `-n`, `-o table|wide|json|yaml`,
    `-v` (API trace on stderr), `--no-color`, `-y/--yes` and `--timeout` (0 means the command's
    default). `-o wide` is allowed only on the commands in `commandsWithWide`.
  - `helpers.go`: test seams (`newClients`, `newApplier`, `httpClient`, `stdinIsTTY`), `confirm`,
    `printStructured` / `printObjects`, `progress`, and the table cell helpers. `groupRunE` makes
    group commands reject unknown subcommands.
  - `install.go`, `upgrade.go`, `uninstall.go`, `status.go`, `doctor.go`, `issuer.go`, `cert.go`, `version.go`.
- `lib/kube`: client construction; GVRs for cert-manager, OLM and the built-in types; the managed-by
  label. `kubetest/` builds fake clusters for tests.
- `lib/cluster`: detects the platform from discovery. OpenShift is any cluster that serves
  `config.openshift.io`; OLM is detected through `operators.coreos.com`.
- `lib/install`:
  - `olm.go`: Subscription objects, channel resolution and the CSV wait. With a pinned version it
    uses Manual approval and approves only the matching InstallPlan. Transient ResolutionFailed
    messages are waited out; any other ResolutionFailed is terminal.
  - `manifest.go`: fetches and parses the upstream manifest. `DefaultVersion` lives here.
  - `operands.go`: the deployment wait, and `CheckAPI`, which dry-run creates an Issuer to confirm
    the webhook serves.
  - `uninstall.go`, `status.go`.
  - `upgrade.go`: `CompareVersions`, `PendingPlans`, `UpgradeOLM` (pinned installs only: channel
    move plus plan approval through `WaitForCSV`), and `Removed` (objects an older manifest had,
    never CRDs).
  - `upgrade.go`: `CompareVersions`, `PendingPlans`, `UpgradeOLM` (pinned installs only: channel
    move plus plan approval through `WaitForCSV`), and `Removed` (objects an older manifest had,
    never CRDs).
- `lib/issuer`: builders for selfsigned, ca and acme issuers, plus list, get, delete and `Resolve`.
  - `Resolve` picks an Issuer in the namespace first, then a ClusterIssuer.
  - `ca.go` holds `--generate`: a bootstrap self-signed issuer, a root CA Certificate, then the CA
    issuer. The CA issuer is annotated with the other two, so deleting it removes them.
- `lib/cert`: build and validate, list, delete, `WaitReady` and `Diagnose`, `Renew`, `Inspect`
  for x509, and `Export`/`Bundle` (`export.go`; the key is always written 0600).
  - `WaitReady` stops early if the CertificateRequest for the awaited revision failed or was denied.
  - `Diagnose` follows owner references from Certificate to CertificateRequest to Order to Challenge.
  - `Renew` sets the Issuing condition, as cmctl does.
- `lib/pebble`: the embedded Pebble manifest (`manifests/pebble.yaml`, a text/template) and
  `DirectoryURL`. No `runAsNonRoot`: the image runs as root, and OpenShift's SCC picks the user.
- `lib/wait`: `For` (polling with a timeout; a call cut off by the deadline counts as
  `ErrTimeout`), condition readers and deployment readiness.

## Patterns

- cert-manager and OLM objects go through the **dynamic client** as unstructured objects. Typed
  cert-manager API structs (`cmapi`) are used only to build objects, converted with
  `cert.ToUnstructured`. Don't add the cert-manager clientset.
- Every object cmo creates gets `app.kubernetes.io/managed-by=cmo-cli` (`kube.ManagedSelector`).
- stdout is for data, stderr for progress and traces. Structured output (`-o json|yaml`) turns
  progress off.
- Errors tell the user what to run next ("list them with: cmo issuer list").
- Destructive commands call `confirm`. It refuses without a TTY unless `--yes` is given.

## Tests

- Unit tests use `lib/kube/kubetest`: fake typed and dynamic clients, with every GVR's list kind
  registered.
- `cmd` tests run the real root command through `run(t, args...)`, which resets every flag first.
  `use(t, clients)` installs the fake cluster and a recording applier. `readyOnCreate` makes the fake
  act like cert-manager by marking new objects Ready.
- `test/integration` builds the binary and runs it against an httptest API server.
- `test/e2e` (build tag `e2e`) runs the whole lifecycle. CI runs it on Kind and on OpenShift (CRC).
  `CMO_E2E_INSTALL_ARGS` passes extra arguments to `cmo install`.
- CI enforces an 80% coverage floor (`.github/workflows/pre-main.yaml`).

## Adding a command

1. Add a file in `cmd/`, register it in `init()`, and keep the logic in `lib/`.
2. If it lists things and has extra columns, add it to `commandsWithWide`.
3. Add a `cmd` test through `run`, and add a `lib` test for the logic.
4. Run `make docs`, and add the command to the table in `README.md`.
