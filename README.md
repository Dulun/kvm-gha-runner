# kvm-gha-runner

Ephemeral GitHub Actions runners on one Linux host, with KVM, a slim Ubuntu image and local caches.
One command sets everything up; after that it runs by itself, including after a reboot.

```bash
git clone https://github.com/Dulun/kvm-gha-runner.git
cd kvm-gha-runner
./kgr
```

After that first run, `kgr` works from any directory. A new shell may be needed the first time
`~/.local/bin` is created.

The first run asks for:
- your proxy (optional)
- the repository the runners serve
- a GitHub fine-grained token or a GitHub App
- runner CPUs, memory and disk (default 2 CPUs, 4 GiB, 20 GiB), how many stay idle and ready (default 2)
  and the most at once (default 8)

It shows you exactly what to create on GitHub. Later runs ask nothing.

Workflows then use `runs-on: kvm-runner-set` (or the label you chose).

## Set up with an agent

Or let a coding agent (Claude Code, Codex, …) running on the runner host do it. Paste this, with your
repository filled in:

```text
Set up ephemeral GitHub Actions runners on this machine with https://github.com/Dulun/kvm-gha-runner
for the repository https://github.com/OWNER/REPO. Clone it to ~/kvm-gha-runner (or git pull --ff-only
if it is there) and follow the "Set up with an agent" section of its README step by step. Never ask me
to paste the GitHub token or key to you; I enter it myself.
```

Steps for the agent. Run everything as the human's normal user, not root.

1. **Check the host.**
   - It must be Ubuntu 26.04 LTS (`. /etc/os-release`), and `/dev/kvm` must exist.
   - `sudo -n true` must succeed. If sudo asks for a password, ask the human to run this in their own
     terminal: `echo "$USER ALL=(ALL) NOPASSWD:ALL" | sudo tee /etc/sudoers.d/kgr-setup`. Remind them
     to remove it at the end.
2. **Proxy.** Ask the human whether this host needs a proxy, for example Clash on port 7897.
   - `HOST_PROXY` is the proxy as this host sees it, e.g. `http://127.0.0.1:7897`.
   - `GUEST_PROXY` is the same port on `192.168.122.1`, e.g. `http://192.168.122.1:7897`. The proxy must
     accept LAN connections (Clash: *allow LAN*).
   - Leave both empty for a direct connection. Check `HOST_PROXY` with
     `curl --proxy "$HOST_PROXY" -sfo /dev/null https://github.com`.
3. **Write `~/.config/kvm-gha-runner/config.env`** (directory mode 0700, file mode 0600):
   ```bash
   GITHUB_URL=https://github.com/OWNER/REPO
   RUNNER_LABELS=kvm-runner-set
   RUNNER_MIN=2
   RUNNER_MAX=8
   RUNNER_CPUS=2
   RUNNER_MEMORY_GIB=4
   RUNNER_DISK_GIB=20
   GOLDEN_VOLUME=kvm-gha-slim-v1.qcow2
   GUEST_USER=runner
   IMAGE_TOOLS='node python go supabase skopeo psql'
   IMAGE_DOCKER_IMAGES=''
   BASE_IMAGE=''
   HOST_PROXY=http://127.0.0.1:7897
   GUEST_PROXY=http://192.168.122.1:7897
   ```
   Lower `RUNNER_MAX` so that `RUNNER_MAX × RUNNER_CPUS` fits `nproc`, and
   `RUNNER_MAX × RUNNER_MEMORY_GIB` plus 4 fits the host memory in GiB. Disks are thin, so
   `RUNNER_DISK_GIB` only needs to fit what one job writes.
4. **GitHub access: the human does this part.**
   - Ask them to create a fine-grained token at
     <https://github.com/settings/personal-access-tokens/new>, limited to only this repository, with
     Repository permissions → Administration: Read and write, and Contents: Read-only (for the git cache).
   - Then ask them to run `~/kvm-gha-runner/kgr github` in their own terminal. It asks for the
     repository URL (accept the default), then option `1`, then the token. Input is hidden, and the
     token is checked against GitHub before it is saved.
   - When they say it is done, confirm with `test -s ~/.config/kvm-gha-runner/github-token`. Never read,
     print or copy that file or `github-app.pem`.
5. **Run `~/kvm-gha-runner/kgr up` with output going to `~/kgr-up.log`.**
   - The first run builds the image and can take 30–60 minutes, so run it in the background (or with a
     long enough timeout) and follow the log.
   - It asks nothing when the config is complete. A line starting with `STOP:` says what is wrong.
   - If it stops after adding the user to the kvm/libvirt groups, ask the human to reboot. Then run it
     again, in a new agent session if needed.
   - Rerunning it is always safe.
6. **Done** when `kgr status` shows no `FAIL` lines and the runner is listed at
   `https://github.com/OWNER/REPO/settings/actions/runners`. Report that, and remind the human to remove
   `/etc/sudoers.d/kgr-setup` if they created it.

Don't edit files in the repository, run `kgr rebuild` or delete images or caches unless the human asks.

## What it does

- **One VM per job.** A manager (Go, built from source here) listens to GitHub through the official
  [actions/scaleset](https://github.com/actions/scaleset) client. For each job it clones a fresh VM from a
  read-only golden image, registers it with a just-in-time config, and deletes the VM when the job ends.
  `RUNNER_MIN` VMs are kept idle and ready. Each VM gets its own CPUs, memory and a thin disk.
- **Image built unattended, with the tools you pick.** The image is official Ubuntu 26.04 LTS,
  signature-checked, plus `curl`, `git`, `gh`, `jq`, Docker (buildx, compose) and the Actions runner. In
  `kgr` → *Image contents* you tick what else to bake in (space selects, Enter confirms):

  | Tool | Default |
  | --- | --- |
  | Node.js LTS with pnpm, Python 3 (pip, venv, uv), Go, Supabase CLI, skopeo, PostgreSQL client | on |
  | Docker images of the local Supabase stack, Deno, actionlint, ShellCheck, OpenSlide, Alibaba Cloud CLI | off |

  Bake in anything your workflows would otherwise `apt install`: jobs then skip `apt update` entirely.

  You can also list any other Docker images to bake in, and point it at your own Ubuntu 26.04 cloud image
  file to build from. Otherwise it uses the official image: downloaded once into `/var/cache/kvm-gha-runner`,
  then reused for every rebuild after an offline signature and checksum check. An existing golden image is
  never rebuilt unless you change its contents or choose Rebuild. Baked tools are downloaded once, when the image is
  built; jobs find them already installed. Every download is pinned to a version and checksum in
  `image/provision.sh`. Tools you don't bake can still come from `setup-*` actions, which download each
  version once into the shared tool cache.
- **Caches on the host, shared by every VM.** Upstream is fetched once, through your proxy, on a miss:

  | Cache | Where | Used by |
  | --- | --- | --- |
  | Verdaccio npm registry | `192.168.122.1:4873` | npm and pnpm in VMs |
  | Docker Hub pull-through | `192.168.122.1:5000` | dockerd in VMs (`registry-mirrors`) |
  | apt-cacher-ng | `192.168.122.1:3142` | `apt-get update` and `apt-get install` in jobs |
  | git mirror of your repository (full history) | `/mnt/ci-cache/git/mirror.git` | each job start asks the host to fetch what is new (no timer); every git command in VMs borrows its objects (`GIT_ALTERNATE_OBJECT_DIRECTORIES`), so `actions/checkout` downloads only commits the mirror does not have yet |
  | NFS `/srv/ci-cache` | mounted at `/mnt/ci-cache` | pnpm store, npm cache, Actions tool cache (`RUNNER_TOOL_CACHE`), Go modules (`GOMODCACHE`), pip and uv caches |

  Not cached: images from registries other than Docker Hub (bake them in instead), Deno's `jsr:` imports,
  apt repositories served over HTTPS and `actions/cache`. Those go through the proxy each time.
- **Proxy everywhere.**
  - `HOST_PROXY` covers apt, downloads, `go build`, the host's dockerd and cache misses.
  - `GUEST_PROXY` is injected into every VM: the runner and job steps (`HTTP(S)_PROXY`), dockerd (image
    pulls) and the Docker client (`~/.docker/config.json` `proxies`), so containers from `docker run`,
    `docker build` and compose use it too. It must listen on `192.168.122.1`; in Clash, turn on *allow LAN*.
  - The host caches and private ranges (`192.168.0.0/16`, `10.0.0.0/8`, `172.16.0.0/12`) bypass the proxy.

## Menu

Just run `kgr`: everything is in its menu, and after a change it offers to apply it right away. Each item is
also a command:

| Command | What it does |
| --- | --- |
| `kgr up` | Set up or repair everything and start the runners. Safe to rerun. |
| `kgr status` | Caches, image, service, proxy, token expiry, running VMs and recent log |
| `kgr log` | Follow the runner log live (Ctrl+C goes back) |
| `kgr github` | Change the token or App, e.g. when a token expires |
| `kgr proxy` | Change the proxies |
| `kgr runners` | CPUs, memory and disk per runner, idle and maximum count, label, crashed VMs kept for debugging |
| `kgr image` | Pick the tools and Docker images baked into the image; the next apply builds it |
| `kgr update` | Pull the latest kvm-gha-runner and apply it |
| `kgr rebuild` | Build a new golden image (next version number) and switch to it |
| `kgr clean` | Show disk use; delete old images and, if you confirm, empty the caches |
| `kgr prune` | Remove offline runners from GitHub (left by old setups or crashed VMs); runners whose VM still exists here are kept |
| `kgr stop` | Stop the runners |

From the menu, changes are applied when you confirm. With the commands, run `kgr up` afterwards.

## GitHub access

- **Fine-grained token (simplest):** create it at <https://github.com/settings/personal-access-tokens/new>.
  - Repository access: only the repository the runners serve.
  - Permissions: Repository → Administration: Read and write, and Contents: Read-only so the git cache can
    fetch a private repository. Metadata: Read-only is added automatically.
  - When the token expires, runners stop registering. Run `kgr github` to paste a new one.
- **GitHub App (never expires):** Administration: Read and write, webhook off, installed on the
  repository. You need the App ID, the Installation ID and the downloaded private key.

Secrets stay in `~/.config/kvm-gha-runner/` with mode 0600 and are never printed.

## Requirements

- Ubuntu 26.04 LTS host with KVM (`/dev/kvm`) and sudo. `kgr` installs the packages it needs.
- The libvirt default network on `192.168.122.1`, which is the libvirt default.
- Disk: about 3 GB for the image, plus the caches as they grow.

## Files

```
kgr                   setup, menu and commands
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
