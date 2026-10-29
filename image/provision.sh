#!/usr/bin/env bash
# Runs once inside the build VM (cloud-init runcmd, as root). Installs the basics plus the tools and
# Docker images the owner picked in `kgr` (Image contents), so jobs never download those. Anything
# else lands in shared caches on NFS: setup-* tools in RUNNER_TOOL_CACHE, Go modules, pip and uv.
# Docker Hub images come from the :5000 mirror, npm from Verdaccio.
set -Eeuo pipefail

# shellcheck source=/dev/null
source /root/kvm-gha-build.env # GUEST_USER, BUILD_PROXY, IMAGE_TOOLS, IMAGE_DOCKER_IMAGES

readonly node_version='26.13.0' # LTS (Lithium)
readonly node_sha256='d28a96af6c38596f6d9d97c0ee531c13d238d757742387d1928de822d959e997'
readonly pnpm_version='12.14.0'
readonly runner_version='2.336.0'
readonly runner_sha256='04cf0be1aff4c3ec3554466c39124ca250e3effd8873bb7e8d68535aa9505d5d'
# Optional tools (IDs match image_tool_catalog in kgr). Bump the version and the checksum together.
readonly go_version='1.27.3'
readonly go_sha256='65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40'
readonly uv_version='0.13.6'
readonly uv_sha256='f29c3e94ffabd7e6f3ba521dba169a9b7ec12100bf3257087f0f24f6123f3645'
readonly supabase_version='2.122.0'
readonly supabase_sha256='59ed066847b98d7461c60e9109d40cab6b9756177ce06ebb05b51338519f5136'
readonly deno_version='2.9.8'
readonly deno_sha256='e890975bc2549807bbcc0b584ffafda72cc7c93349dd99c80483dd784f6c4609'
readonly actionlint_version='1.7.12'
readonly actionlint_sha256='8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8'
readonly aliyun_version='3.5.2'
readonly aliyun_sha256='8058cb91fa0e2d46a7c5fd47e44ab6a465be8477b28595d408dcc03a0ed5bd4e'
readonly cache_host='192.168.122.1'
readonly cache_root='/mnt/ci-cache'
readonly registry="http://${cache_host}:4873/"

if [ -n "$BUILD_PROXY" ]; then
  export http_proxy="$BUILD_PROXY" https_proxy="$BUILD_PROXY" HTTP_PROXY="$BUILD_PROXY" HTTPS_PROXY="$BUILD_PROXY"
  export no_proxy="localhost,127.0.0.1,${cache_host}" NO_PROXY="localhost,127.0.0.1,${cache_host}"
fi
export DEBIAN_FRONTEND=noninteractive
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
has_tool() { [[ " ${IMAGE_TOOLS:-} " == *" $1 "* ]]; }
fetch() { # fetch URL FILE SHA256
  curl --fail --location --silent --show-error "$1" --output "$2"
  echo "$3  $2" | sha256sum --check --strict --quiet
}
gh_release() { echo "https://github.com/$1/releases/download/$2"; }

# One-shot VMs boot from a stale image: background apt upgrades would grab the dpkg lock mid-job.
systemctl disable --now apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service >/dev/null 2>&1 || true
systemctl mask apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service >/dev/null

apt_extra=()
has_tool python && apt_extra+=(python3 python3-pip python3-venv)
has_tool shellcheck && apt_extra+=(shellcheck)
has_tool skopeo && apt_extra+=(skopeo)
has_tool psql && apt_extra+=(postgresql-client)
has_tool openslide && apt_extra+=(openslide-tools)
apt-get update
apt-get install --yes --no-install-recommends \
  build-essential ca-certificates curl docker-buildx docker-compose-v2 docker.io gh git jq \
  nfs-common qemu-guest-agent rsync unzip xz-utils zip "${apt_extra[@]}"
usermod -aG docker "$GUEST_USER"
install -d -m 0755 /etc/docker
cat >/etc/docker/daemon.json <<EOF
{
  "features": { "buildkit": true },
  "log-driver": "local",
  "registry-mirrors": ["http://${cache_host}:5000"],
  "insecure-registries": ["${cache_host}:5000"]
}
EOF
systemctl enable docker.service qemu-guest-agent.service

# Shared cache. nofail: a VM still boots without the host share and falls back to the local dirs below.
echo "${cache_host}:/srv/ci-cache ${cache_root} nfs4 rw,nofail,_netdev,soft,timeo=50,retrans=2 0 0" >>/etc/fstab
install -d -o "$GUEST_USER" -g "$GUEST_USER" -m 0775 "$cache_root"/{pnpm,pnpm-cache,npm,toolcache,go-mod,pip,uv}

if has_tool node; then
  fetch "https://nodejs.org/dist/v${node_version}/node-v${node_version}-linux-x64.tar.xz" "$tmp/node.tar.xz" "$node_sha256"
  tar --extract --xz --file "$tmp/node.tar.xz" --directory /usr/local --strip-components=1
  printf 'registry=%s\ncache=%s/npm\n' "$registry" "$cache_root" >/usr/local/etc/npmrc
  npm install --global --no-audit --no-fund "pnpm@${pnpm_version}"
fi

home="/home/${GUEST_USER}"
install -d -o "$GUEST_USER" -g "$GUEST_USER" "$home/.config/pnpm"
printf 'storeDir: %s/pnpm\ncacheDir: %s/pnpm-cache\n' "$cache_root" "$cache_root" >"$home/.config/pnpm/config.yaml"
printf 'registry=%s\n' "$registry" >"$home/.npmrc"
chown "$GUEST_USER:$GUEST_USER" "$home/.config/pnpm/config.yaml" "$home/.npmrc"

curl --fail --location --silent --show-error \
  "https://github.com/actions/runner/releases/download/v${runner_version}/actions-runner-linux-x64-${runner_version}.tar.gz" \
  --output "$tmp/runner.tar.gz"
echo "${runner_sha256}  $tmp/runner.tar.gz" | sha256sum --check --strict --quiet
install -d -o "$GUEST_USER" -g "$GUEST_USER" /opt/actions-runner
tar --extract --gzip --file "$tmp/runner.tar.gz" --directory /opt/actions-runner
/opt/actions-runner/bin/installdependencies.sh
# The runner passes .env to every job: setup-node/go/python/... download into the shared tool cache,
# and Go modules, pip and uv downloads go to shared caches (uv copies: the cache is on another filesystem).
printf '%s\n' \
  "RUNNER_TOOL_CACHE=$cache_root/toolcache" "AGENT_TOOLSDIRECTORY=$cache_root/toolcache" \
  "GOMODCACHE=$cache_root/go-mod" "PIP_CACHE_DIR=$cache_root/pip" \
  "UV_CACHE_DIR=$cache_root/uv" 'UV_LINK_MODE=copy' \
  "GIT_ALTERNATE_OBJECT_DIRECTORIES=$cache_root/git/mirror.git/objects" \
  'ACTIONS_RUNNER_HOOK_JOB_COMPLETED=/opt/actions-runner/kgr-job-completed.sh' >/opt/actions-runner/.env
# After each job, copy the git objects it fetched into the shared mirror; the next checkout borrows them.
cat >/opt/actions-runner/kgr-job-completed.sh <<'HOOK'
#!/usr/bin/env bash
# Runner job-completed hook. Best effort: it never fails the job.
mirror=/mnt/ci-cache/git/mirror.git
work="${GITHUB_WORKSPACE:-}"
if [ -d "$mirror/objects" ] && [ -n "$work" ] && git -C "$work" rev-parse --verify --quiet HEAD >/dev/null 2>&1; then
  timeout 120 git -C "$mirror" fetch --quiet --no-tags --update-shallow "$work" \
    "+HEAD:refs/kgr/${GITHUB_REPOSITORY:-repo}/${GITHUB_REF:-HEAD}" >/dev/null 2>&1 || true
fi
exit 0
HOOK
chmod 0755 /opt/actions-runner/kgr-job-completed.sh
chown -R "$GUEST_USER:$GUEST_USER" /opt/actions-runner

if has_tool go; then
  fetch "https://go.dev/dl/go${go_version}.linux-amd64.tar.gz" "$tmp/go.tar.gz" "$go_sha256"
  tar --extract --gzip --file "$tmp/go.tar.gz" --directory /usr/local
  ln -sfn /usr/local/go/bin/go /usr/local/bin/go
  ln -sfn /usr/local/go/bin/gofmt /usr/local/bin/gofmt
fi
if has_tool python; then
  fetch "$(gh_release astral-sh/uv "$uv_version")/uv-x86_64-unknown-linux-gnu.tar.gz" "$tmp/uv.tar.gz" "$uv_sha256"
  tar --extract --gzip --file "$tmp/uv.tar.gz" --directory /usr/local/bin --strip-components=1
fi
if has_tool supabase || has_tool supabase-stack; then
  fetch "$(gh_release supabase/cli "v$supabase_version")/supabase_${supabase_version}_linux_amd64.tar.gz" \
    "$tmp/supabase.tar.gz" "$supabase_sha256"
  tar --extract --gzip --file "$tmp/supabase.tar.gz" --directory /usr/local/bin supabase
fi
if has_tool deno; then
  fetch "$(gh_release denoland/deno "v$deno_version")/deno-x86_64-unknown-linux-gnu.zip" "$tmp/deno.zip" "$deno_sha256"
  unzip -o -q "$tmp/deno.zip" -d /usr/local/bin
fi
if has_tool actionlint; then
  fetch "$(gh_release rhysd/actionlint "v$actionlint_version")/actionlint_${actionlint_version}_linux_amd64.tar.gz" \
    "$tmp/actionlint.tar.gz" "$actionlint_sha256"
  tar --extract --gzip --file "$tmp/actionlint.tar.gz" --directory /usr/local/bin actionlint
fi
if has_tool aliyun; then
  fetch "$(gh_release aliyun/aliyun-cli "v$aliyun_version")/aliyun-cli-linux-${aliyun_version}-amd64.tgz" \
    "$tmp/aliyun.tgz" "$aliyun_sha256"
  tar --extract --gzip --file "$tmp/aliyun.tgz" --directory /usr/local/bin
fi

# Baked Docker images: Docker Hub through the :5000 mirror, other registries through the build proxy.
if has_tool supabase-stack || [ -n "${IMAGE_DOCKER_IMAGES:-}" ]; then
  if [ -n "$BUILD_PROXY" ]; then
    install -d /etc/systemd/system/docker.service.d
    printf '[Service]\nEnvironment="HTTP_PROXY=%s" "HTTPS_PROXY=%s" "NO_PROXY=localhost,127.0.0.1,%s"\n' \
      "$BUILD_PROXY" "$BUILD_PROXY" "$cache_host" >/etc/systemd/system/docker.service.d/build-proxy.conf
    systemctl daemon-reload
  fi
  systemctl restart docker.service
  for image in ${IMAGE_DOCKER_IMAGES:-}; do docker pull --quiet "$image"; done
  if has_tool supabase-stack; then
    # Starting a throwaway project pulls exactly the images this CLI version uses; jobs on the baked CLI reuse them.
    install -d "$tmp/supabase-project"
    (
      cd "$tmp/supabase-project"
      supabase init --yes >/dev/null
      supabase start --ignore-health-check || echo 'WARN: supabase start failed after pulling; images are kept.'
      supabase stop --no-backup || true
    )
    docker volume prune --all --force >/dev/null
  fi
  rm -f /etc/systemd/system/docker.service.d/build-proxy.conf
  docker image ls --format '{{.Repository}}:{{.Tag}}'
fi

if has_tool node; then node --version && pnpm --version; fi
docker --version
docker compose version
gh --version | head -1
if has_tool go; then go version; fi
if has_tool python; then python3 --version && uv --version; fi
if has_tool supabase || has_tool supabase-stack; then supabase --version; fi
if has_tool deno; then deno --version | head -1; fi
if has_tool actionlint; then actionlint --version | head -1; fi
if has_tool aliyun; then aliyun version; fi
runuser -u "$GUEST_USER" -- /opt/actions-runner/bin/Runner.Listener --version

# apt in jobs goes through the host cache (written last, so the build itself uses the build proxy).
printf 'Acquire::http::Proxy "http://%s:3142";\n' "$cache_host" >/etc/apt/apt.conf.d/01kvm-gha-cache

# Leave nothing build-specific behind: no build proxy, no apt lists, no host keys, fresh machine-id.
rm -f /etc/apt/apt.conf.d/90cloud-init-aptproxy /root/kvm-gha-build.env
apt-get clean
rm -rf /var/lib/apt/lists/* /etc/ssh/ssh_host_* /var/lib/systemd/random-seed
journalctl --rotate >/dev/null 2>&1 || true
journalctl --vacuum-time=1s >/dev/null 2>&1 || true
cloud-init clean --logs --machine-id --seed
# build-image.sh only accepts the disk when this marker exists (cloud-init powers off only then).
touch /var/lib/kvm-gha-provisioned
fstrim --all >/dev/null 2>&1 || true
