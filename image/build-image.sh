#!/usr/bin/env bash
# Builds the slim runner golden volume unattended:
# verified Ubuntu 26.04 LTS cloud image → build VM runs provision.sh via cloud-init → powers off →
# flattened, read-only qcow2 in the libvirt default pool. Run as root: sudo image/build-image.sh <volume-name>
set -Eeuo pipefail

readonly golden_name="${1:?usage: sudo $0 <golden-volume-name, e.g. kvm-gha-slim-v1.qcow2>}"
readonly guest_user="${GUEST_USER:-runner}"
readonly build_proxy="${BUILD_PROXY:-}"     # what the build VM uses to download (e.g. http://192.168.122.1:7897)
readonly download_proxy="${HOST_PROXY:-}"   # what this host uses to fetch the cloud image
readonly base_image="${BASE_IMAGE:-}"       # your own Ubuntu 26.04 cloud image file; empty = the official one
readonly pool_dir='/var/lib/libvirt/images'
readonly image_url='https://cloud-images.ubuntu.com/resolute/current'
readonly image_file='resolute-server-cloudimg-amd64.img'
readonly cache_dir='/var/cache/kvm-gha-runner'
readonly build_name='kvm-gha-image-build'
readonly build_disk="$pool_dir/$build_name.qcow2"
readonly build_seed="$pool_dir/$build_name-seed.iso"
readonly target="$pool_dir/$golden_name"
readonly timeout_minutes=40
# The build VM's serial console (written by libvirt); provision.sh reports its steps there.
readonly console_log="/var/log/libvirt/qemu/$build_name-console.log"
virsh_cmd=(virsh --connect qemu:///system)

[ "$(id -u)" -eq 0 ] || { echo 'run as root (sudo)' >&2; exit 1; }
[[ "$golden_name" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*\.qcow2$ ]] || { echo "unsafe volume name: $golden_name" >&2; exit 1; }
[[ "$guest_user" =~ ^[a-z_][a-z0-9_-]*$ ]] || { echo "unsafe guest user: $guest_user" >&2; exit 1; }
[ ! -e "$target" ] || { echo "refusing to overwrite $target" >&2; exit 1; }

work="$(mktemp -d)"
cleanup() {
  local rc=$?
  "${virsh_cmd[@]}" destroy "$build_name" >/dev/null 2>&1 || true
  "${virsh_cmd[@]}" undefine "$build_name" --nvram >/dev/null 2>&1 || true
  rm -rf -- "$work"
  rm -f -- "$build_disk" "$build_seed" "$target.partial"
  "${virsh_cmd[@]}" pool-refresh default >/dev/null 2>&1 || true
  exit "$rc"
}
trap cleanup EXIT
# A build VM left over from an interrupted run would block virt-install.
"${virsh_cmd[@]}" destroy "$build_name" >/dev/null 2>&1 || true
"${virsh_cmd[@]}" undefine "$build_name" --nvram >/dev/null 2>&1 || true

# Base image, first match wins: your own file, the official image downloaded earlier, a fresh download.
# The official one is kept with its signed checksum list and re-verified offline on every build.
fetch() { curl --fail --location --silent --show-error ${download_proxy:+--proxy "$download_proxy"} "$@"; }
verify_cached() {
  [ -f "$cache_dir/$image_file" ] && [ -f "$cache_dir/SHA256SUMS" ] && [ -f "$cache_dir/SHA256SUMS.gpg" ] || return 1
  gpgv --keyring /usr/share/keyrings/ubuntu-cloudimage-keyring.gpg \
    "$cache_dir/SHA256SUMS.gpg" "$cache_dir/SHA256SUMS" >/dev/null 2>&1 || return 1
  local expected
  expected="$(awk -v f="*$image_file" '$2 == f || $2 == substr(f, 2) { print $1 }' "$cache_dir/SHA256SUMS")"
  [ -n "$expected" ] && echo "$expected  $cache_dir/$image_file" | sha256sum --check --strict --quiet 2>/dev/null
}
if [ -n "$base_image" ]; then
  [ -f "$base_image" ] && [ -r "$base_image" ] || { echo "base image not readable: $base_image" >&2; exit 1; }
  echo "==> Use your base image $base_image"
  source_image="$base_image"
else
  install -d -m 0755 "$cache_dir"
  if verify_cached; then
    echo "==> Reuse the downloaded $image_file (signature and checksum verified)"
  else
    echo "==> Download and verify $image_file"
    fetch "$image_url/SHA256SUMS" --output "$cache_dir/SHA256SUMS"
    fetch "$image_url/SHA256SUMS.gpg" --output "$cache_dir/SHA256SUMS.gpg"
    fetch "$image_url/$image_file" --output "$cache_dir/$image_file.partial"
    mv -- "$cache_dir/$image_file.partial" "$cache_dir/$image_file"
    verify_cached || { rm -f -- "$cache_dir/$image_file"; echo "downloaded $image_file failed verification" >&2; exit 1; }
  fi
  source_image="$cache_dir/$image_file"
fi

echo '==> Build VM disk and cloud-init seed'
qemu-img convert -O qcow2 "$source_image" "$build_disk"
# Small, so the per-runner disk setting decides the VM size; baked Docker images need more room.
disk_gib=10
if [[ " ${IMAGE_TOOLS:-} " == *" supabase-stack "* ]] || [ -n "${IMAGE_DOCKER_IMAGES:-}" ]; then disk_gib=30; fi
qemu-img resize "$build_disk" "${disk_gib}G" >/dev/null
chown libvirt-qemu:kvm "$build_disk"

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
printf 'GUEST_USER=%q\nBUILD_PROXY=%q\nIMAGE_TOOLS=%q\nIMAGE_DOCKER_IMAGES=%q\n' \
  "$guest_user" "$build_proxy" "${IMAGE_TOOLS:-}" "${IMAGE_DOCKER_IMAGES:-}" >"$work/build.env"
{
  cat <<EOF
#cloud-config
users:
  - name: ${guest_user}
    uid: '1000'
    groups: [adm, sudo]
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    lock_passwd: true
ssh_pwauth: false
EOF
  if [ -n "$build_proxy" ]; then
    printf 'apt:\n  http_proxy: %s\n  https_proxy: %s\n' "$build_proxy" "$build_proxy"
  fi
  cat <<EOF
write_files:
  - path: /root/kvm-gha-build.env
    permissions: '0600'
    encoding: b64
    content: $(base64 --wrap=0 "$work/build.env")
  - path: /root/kvm-gha-provision.sh
    permissions: '0700'
    encoding: b64
    content: $(base64 --wrap=0 "$script_dir/provision.sh")
runcmd:
  - [ bash, -c, '/root/kvm-gha-provision.sh 2>&1 | tee /var/log/kvm-gha-provision.log >/dev/ttyS0; rm -f /root/kvm-gha-provision.sh' ]
power_state:
  mode: poweroff
  condition: test -f /var/lib/kvm-gha-provisioned
EOF
} >"$work/user-data"
printf 'instance-id: %s\nlocal-hostname: %s\n' "$build_name-$(date +%s)" "$build_name" >"$work/meta-data"
cloud-localds "$build_seed" "$work/user-data" "$work/meta-data"
chown libvirt-qemu:kvm "$build_seed"

echo "==> Provision in a build VM (up to $timeout_minutes min; full output: sudo tail -f $console_log)"
virt-install --connect qemu:///system --name "$build_name" \
  --memory 4096 --vcpus 2 --cpu host-passthrough --machine q35 --os-variant ubuntu26.04 --boot uefi \
  --import \
  --disk "path=$build_disk,device=disk,bus=virtio" \
  --disk "path=$build_seed,device=cdrom,bus=sata,readonly=on" \
  --network network=default,model=virtio \
  --graphics none --serial "pty,log.file=$console_log,log.append=off" \
  --noautoconsole >/dev/null
started=$SECONDS
deadline=$((SECONDS + timeout_minutes * 60))
shown=''
keep_vm_and_exit() {
  echo '--- last lines of the build VM console ---' >&2
  tail -n 25 "$console_log" >&2 2>/dev/null || true
  echo "Inspect before it is destroyed: sudo virsh console $build_name (log: /var/log/kvm-gha-provision.log)" >&2
  trap - EXIT
  rm -rf -- "$work"
  exit 1
}
until [ "$(LC_ALL=C "${virsh_cmd[@]}" domstate "$build_name" 2>/dev/null)" = 'shut off' ]; do
  failed="$(grep -a -o 'KGR-FAILED [^[:cntrl:]]*' "$console_log" 2>/dev/null | tail -n 1 || true)"
  if [ -n "$failed" ]; then
    printf '\nprovisioning failed: %s\n' "${failed#KGR-FAILED }" >&2
    keep_vm_and_exit
  fi
  if [ "$SECONDS" -ge "$deadline" ]; then
    printf '\nbuild VM did not power off in %s min: provisioning hung.\n' "$timeout_minutes" >&2
    keep_vm_and_exit
  fi
  report="$(grep -a -o 'KGR-PROGRESS [0-9]* [^[:cntrl:]]*' "$console_log" 2>/dev/null | tail -n 1 || true)"
  percent=0 label='Booting the build VM'
  [ -z "$report" ] || read -r _ percent label <<<"$report"
  elapsed=$((SECONDS - started))
  if [ -t 1 ]; then
    # The newest console line shows the step is moving (boot messages, apt Get: lines, ...).
    latest="$(tail -c 4096 "$console_log" 2>/dev/null | tr -d '\r' | sed 's/\x1b\[[0-9;?]*[A-Za-z]//g' |
      grep -a -v -e '^[[:space:]]*$' -e '^KGR-PROGRESS ' | tail -n 1 | tr -cd '[:print:]' || true)"
    bar="$(printf '%*s' $((percent * 20 / 100)) '' | tr ' ' '#')"
    line="$(printf '  [%-20s] %3d%% %2dm%02ds  %s: %s' "$bar" "$percent" $((elapsed / 60)) $((elapsed % 60)) "$label" "$latest")"
    printf '\r%.*s\033[K' "$(($(tput cols 2>/dev/null || echo 100) - 1))" "$line"
  elif [ "$label" != "$shown" ]; then
    printf '  %3d%%  %s\n' "$percent" "$label"
  fi
  shown="$label"
  sleep 5
done
[ ! -t 1 ] || echo

echo "==> Flatten into $target"
qemu-img convert -O qcow2 -o compat=1.1,lazy_refcounts=on "$build_disk" "$target.partial"
qemu-img check "$target.partial" >/dev/null
chown libvirt-qemu:kvm "$target.partial"
chmod 0440 "$target.partial"
mv -- "$target.partial" "$target"
echo "Golden volume ready: $target"
