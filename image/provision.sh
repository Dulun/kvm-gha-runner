#!/usr/bin/env bash
# Runs once inside the build VM (cloud-init runcmd, as root). Installs only the basics; every other
# tool is installed by workflows into RUNNER_TOOL_CACHE on the shared NFS cache, so each version
# is downloaded once for all VMs. Docker images come from the :5000 mirror, npm from Verdaccio.
set -Eeuo pipefail

# shellcheck source=/dev/null
source /root/kvm-gha-build.env # GUEST_USER, BUILD_PROXY

readonly node_version='24.19.0'
readonly node_sha256='14b342e71204f811bde6153be8e04b62aef63c236fef92b55f9c83154b409647'
readonly pnpm_version='12.14.0'
readonly runner_version='2.336.0'
readonly runner_sha256='04cf0be1aff4c3ec3554466c39124ca250e3effd8873bb7e8d68535aa9505d5d'
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

# One-shot VMs boot from a stale image: background apt upgrades would grab the dpkg lock mid-job.
systemctl disable --now apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service >/dev/null 2>&1 || true
systemctl mask apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service >/dev/null

apt-get update
apt-get install --yes --no-install-recommends \
  build-essential ca-certificates curl docker-buildx docker-compose-v2 docker.io gh git jq \
  nfs-common python3 python3-pip python3-venv qemu-guest-agent rsync unzip xz-utils zip
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
install -d -o "$GUEST_USER" -g "$GUEST_USER" -m 0775 "$cache_root"/{pnpm,pnpm-cache,npm,toolcache}

curl --fail --location --silent --show-error \
  "https://nodejs.org/dist/v${node_version}/node-v${node_version}-linux-x64.tar.xz" --output "$tmp/node.tar.xz"
echo "${node_sha256}  $tmp/node.tar.xz" | sha256sum --check --strict --quiet
tar --extract --xz --file "$tmp/node.tar.xz" --directory /usr/local --strip-components=1
printf 'registry=%s\ncache=%s/npm\n' "$registry" "$cache_root" >/usr/local/etc/npmrc
npm install --global --no-audit --no-fund "pnpm@${pnpm_version}"

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
# The runner passes .env to every job: setup-node/go/python/... download into the shared tool cache.
printf 'RUNNER_TOOL_CACHE=%s/toolcache\nAGENT_TOOLSDIRECTORY=%s/toolcache\n' "$cache_root" "$cache_root" >/opt/actions-runner/.env
chown -R "$GUEST_USER:$GUEST_USER" /opt/actions-runner

node --version
pnpm --version
docker --version
docker compose version
gh --version | head -1
runuser -u "$GUEST_USER" -- /opt/actions-runner/bin/Runner.Listener --version

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
