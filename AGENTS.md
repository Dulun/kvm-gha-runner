# AGENTS.md

Rules for coding agents working in this repository. Human docs are in [README.md](README.md).

## What this is

One host runs ephemeral GitHub Actions runners: one KVM VM per job, cloned from a slim read-only golden
image, with local caches and a proxy. `./start.sh` is the only entry point; everything else is called by it.

| Path | Role |
| --- | --- |
| `start.sh` | menu and commands (`up`, `status`, `github`, `proxy`, `runners`, `rebuild`, `stop`); host setup, config, service |
| `manager/` | Go scale-set manager (`actions/scaleset`): listens to GitHub, starts/cleans VMs, writes each VM's cloud-init |
| `image/build-image.sh` | unattended golden image build: verified Ubuntu cloud image → build VM → flattened qcow2 |
| `image/provision.sh` | what goes into the image (runs inside the build VM) |
| `cache/` | Verdaccio + Docker Hub mirror (compose), Verdaccio config, NFS export |

Runtime state is outside the repo:
- config and secrets: `~/.config/kvm-gha-runner/`
- manager state: `~/.local/state/kvm-gha-runner/`
- images: `/var/lib/libvirt/images/`
- caches: `/srv/ci-cache/`

## Invariants

- **One command.** `./start.sh up` must stay idempotent: safe to rerun after a reinstall, a reboot or a
  `git pull`. It may only stop for things a human must provide, such as the token, App key or group re-login.
  Interactive questions are asked once, on first run or from the menu; `up` never asks when the config is complete.
- **Proxy for every network call.**
  - On the host, `HOST_PROXY` covers apt, curl, `go build`, the host dockerd and cache misses.
  - In VMs, `GUEST_PROXY` covers the runner env, guest dockerd and the Docker client `proxies`.
  - The caches on `192.168.122.1` and private ranges bypass the proxy (`guestNoProxy` in `manager/libvirt.go`).
  - New downloads must honor the proxy. Empty proxy means a direct connection.
- **Cache first.** npm and pnpm go through Verdaccio, Docker Hub pulls through `:5000`, and the pnpm store and
  Actions tool cache are on NFS. Don't bake big tools or images into the golden image; workflows install them.
- **Pinned and verified.** Every downloaded binary (Go, Node, runner) has an exact version plus sha256. The
  cloud image is checked with `gpgv` against Ubuntu's keyring. Container images are pinned by digest.
  Bump the version and the checksum together.
- **The proxy is injected at boot, never baked into the image.** The image must stay usable without a proxy.
- **Secrets** (token, App private key, JIT config) are files with mode 0600, never printed, never logged,
  never committed, never put in cloud-init as plain text (the JIT config is base64 and written to `/run`).
- **One listener per scale set.** Don't add code paths that can run two managers for the same scale set.

## Working here

- Shell: `set -Eeuo pipefail`, quote everything, keep the style of `start.sh` (`step` / `warn` / `die`).
  Values that reach systemd `ExecStart` must not contain spaces (URLs, names, numbers only).
- Go: standard library first; keep `manager/` dependency-free beyond `actions/scaleset`.
- English everywhere: code, comments, messages and docs.
- Commits: `type(scope): <emoji> subject`, e.g. `feat(manager): 🎸 ...`, `fix(image): 🐛 ...`,
  `docs: ✏️ ...`. Never force-push `main`.

## Checks

These are light and safe to run anywhere:

```bash
bash -n start.sh image/*.sh
shellcheck start.sh image/*.sh
(cd manager && GOWORK=off gofmt -l . && GOWORK=off go vet ./... && GOWORK=off go test -race ./...)
```

**Don't run `start.sh`, `image/build-image.sh` or anything with `sudo` unless the owner asks.** They
install packages, restart dockerd, create VMs and replace the running service on the real host. The owner
runs them.
