#!/usr/bin/env bash
# Builds the cluster's Talos disk image on the builder server with imager,
# writes it over the builder's own disk and powers the builder off, so the
# block can snapshot that disk for the node to boot from.
# Usage: build-image.sh <imager image by digest> <machine configuration> <network router>
#   BUILD_WORK_DIR, BUILD_DISK, BUILD_PULL_ATTEMPTS, BUILD_RESOLV_CONF and
#   BUILD_PROC default to /mnt/mitstack-build, /dev/sda, 30, /etc/resolv.conf
#   and /proc.
# It exists because the image embeds every key of the cluster, so it is built
# inside the cluster's own Hetzner project and imager's output, which prints
# that configuration, is discarded. A failed step leaves the builder running,
# which the block reads as a failed build once its deadline passes.
set -euo pipefail

readonly work=${BUILD_WORK_DIR:-/mnt/mitstack-build}
readonly disk=${BUILD_DISK:-/dev/sda}
readonly attempts=${BUILD_PULL_ATTEMPTS:-30}
readonly resolv_conf=${BUILD_RESOLV_CONF:-/etc/resolv.conf}
readonly proc=${BUILD_PROC:-/proc}
readonly resolvers=("185.12.64.1" "185.12.64.2")

fail() {
  echo "build-image: $*" >&2
  exit 1
}

# route_through_gateway sends the builder's traffic to the network's router, which forwards it to the gateway.
route_through_gateway() {
  ip route replace default via "$1"
  rm -f "$resolv_conf"
  printf 'nameserver %s\n' "${resolvers[@]}" > "$resolv_conf"
}

pull_imager() {
  local attempt
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    if docker pull --quiet "$1" > /dev/null; then
      return 0
    fi
    sleep 10
  done
  fail "could not pull $1 after $attempts attempts"
}

# build runs imager with the kit's arguments and discards its output, since it prints every key of the configuration.
build() {
  local imager=$1 config=$2
  mkdir -p "$work"
  mountpoint -q "$work" || mount -t tmpfs -o size=2g tmpfs "$work"
  mkdir -p "$work/in" "$work/out"
  install -m 0600 "$config" "$work/in/config.yaml"
  rm -f "$config"
  if ! docker run --rm --privileged -v /dev:/dev -v "$work/in:/in:ro" -v "$work/out:/out" \
    "$imager" hcloud --arch amd64 --embedded-config-path /in/config.yaml > /dev/null 2>&1; then
    rm -f "$work/in/config.yaml"
    fail "imager failed; its output is discarded because it holds every cluster key"
  fi
  rm -f "$work/in/config.yaml"
}

write_disk() {
  local images=("$work"/out/*.raw.zst)
  if [ "${#images[@]}" -ne 1 ] || [ ! -f "${images[0]}" ]; then
    fail "imager left no single image in $work/out"
  fi
  zstd -dc "${images[0]}" | dd of="$disk" bs=4M oflag=direct status=none
  sync
}

# power_off goes through sysrq, since the disk under the running system now holds the Talos image.
power_off() {
  echo 1 > "$proc/sys/kernel/sysrq"
  echo o > "$proc/sysrq-trigger"
}

main() {
  [ "$#" -eq 3 ] || fail "usage: build-image.sh <imager image> <machine configuration> <network router>"
  route_through_gateway "$3"
  pull_imager "$1"
  build "$1" "$2"
  write_disk
  echo "build-image: wrote the image to $disk"
  power_off
}

main "$@"
