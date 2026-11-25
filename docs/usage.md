# Using cmo

How cmo behaves across every command, with examples for the common jobs.

## Behavior that applies everywhere

- `-o table|json|yaml`, plus `-o wide` on `cmo cert list` and `cmo issuer list`. Data goes to
  stdout and progress to stderr, so `cmo status -o json | jq` always works.
- `-v` traces each API request the way kubectl would name it
  (`→ get certificates.cert-manager.io web -n default`).
- `--dry-run` on `install`, `upgrade`, `pebble install` and the `create` commands prints the YAML
  and changes nothing.
- `--timeout` controls how long a command waits. `--no-wait` returns as soon as the request is made.
- Destructive commands ask first. `--yes` skips the question, and without a terminal cmo refuses
  instead of guessing.
- Everything cmo creates is labeled `app.kubernetes.io/managed-by=cmo-cli`. `--all-managed` uses
  this label to delete cmo's objects and nothing else.

## Examples

```bash
# Pin an operator release on OpenShift (Manual approval; channel stable-v1.18)
cmo install --version v1.18.0

# See an upgrade waiting for a pinned install, then take it
cmo status
cmo upgrade --dry-run && cmo upgrade

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

## Cleanup

```bash
cmo cert delete --all-managed -A --yes     # every certificate cmo created, and its secret
cmo issuer delete --all-managed --yes      # every issuer cmo created
cmo uninstall --purge --yes                # cert-manager, its namespace and its CRDs
```
