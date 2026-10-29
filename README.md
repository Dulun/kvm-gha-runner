# kvm-gha-runner

Ephemeral GitHub Actions runners on one Linux host, with KVM, a slim Ubuntu image and local caches.
One command sets everything up; after that it runs by itself, including after a reboot.

```bash
git clone https://github.com/Dulun/kvm-gha-runner.git
cd kvm-gha-runner
./start.sh
```

The first run asks for:
- your proxy (optional)
- the repository the runners serve
- a GitHub fine-grained token or a GitHub App
- runner size and count

It shows you exactly what to create on GitHub. Later runs ask nothing.

Workflows then use `runs-on: kvm-runner-set` (or the label you chose).

## What it does

- **One VM per job.** A manager (Go, built from source here) listens to GitHub through the official
  [actions/scaleset](https://github.com/actions/scaleset) client. For each job it clones a fresh VM from a
  read-only golden image, registers it with a just-in-time config, and deletes the VM when the job ends.
  `RUNNER_MIN` VMs are kept warm.
- **Slim image, built unattended.** The image is official Ubuntu 24.04, signature-checked, plus `curl`,
  `git`, `gh`, `jq`, Docker (buildx, compose), Node 24, pnpm, Python 3 and the Actions runner.
  Workflows install anything else with the usual `setup-*` actions, and those downloads land in a shared
  tool cache.
- **Caches on the host, shared by every VM.** Upstream is fetched once, through your proxy, on a miss:

  | Cache | Where | Used by |
  | --- | --- | --- |
  | Verdaccio npm registry | `192.168.122.1:4873` | npm and pnpm in VMs |
  | Docker Hub pull-through | `192.168.122.1:5000` | dockerd in VMs (`registry-mirrors`) |
  | NFS `/srv/ci-cache` | mounted at `/mnt/ci-cache` | pnpm store, npm cache, Actions tool cache (`RUNNER_TOOL_CACHE`) |
- **Proxy everywhere.**
  - `HOST_PROXY` covers apt, downloads, `go build`, the host's dockerd and cache misses.
  - `GUEST_PROXY` is injected into every VM, for the runner, job steps and dockerd. It must listen on
    `192.168.122.1`; in Clash, turn on *allow LAN*.

## Menu

`./start.sh` opens a menu. Each item is also a command:

| Command | What it does |
| --- | --- |
| `./start.sh up` | Set up or repair everything and start the runners. Safe to rerun. |
| `./start.sh status` | Caches, image, service, proxy, running VMs and recent log |
| `./start.sh github` | Change the token or App, e.g. when a token expires |
| `./start.sh proxy` | Change the proxies |
| `./start.sh runners` | Change the label, count, CPUs and memory |
| `./start.sh rebuild` | Build a new golden image (next version number) and switch to it |
| `./start.sh stop` | Stop the runners |

After `github`, `proxy` or `runners`, run `./start.sh up` to apply.

## GitHub access

- **Fine-grained token (simplest):** create it at <https://github.com/settings/personal-access-tokens/new>.
  - Repository access: only the repository the runners serve.
  - Permissions: Repository → Administration: Read and write. Metadata: Read-only is added automatically.
  - When the token expires, runners stop registering. Run `./start.sh github` to paste a new one.
- **GitHub App (never expires):** Administration: Read and write, webhook off, installed on the
  repository. You need the App ID, the Installation ID and the downloaded private key.

Secrets stay in `~/.config/kvm-gha-runner/` with mode 0600 and are never printed.

## Requirements

- Ubuntu 24.04 host with KVM (`/dev/kvm`) and sudo. `start.sh` installs the packages it needs.
- The libvirt default network on `192.168.122.1`, which is the libvirt default.
- Disk: about 3 GB for the image, plus the caches as they grow.

## Files

```
start.sh              setup, menu and commands
manager/              the scale-set manager (Go)
image/build-image.sh  unattended golden image build (Ubuntu cloud image + cloud-init)
image/provision.sh    what goes into the image
cache/                Verdaccio + Docker Hub mirror (docker compose) and the NFS export
```

Runtime state lives outside the repository:

| What | Where |
| --- | --- |
| config and secrets | `~/.config/kvm-gha-runner/` |
| manager state | `~/.local/state/kvm-gha-runner/` |
| images | `/var/lib/libvirt/images/` |
| caches | `/srv/ci-cache/` |
