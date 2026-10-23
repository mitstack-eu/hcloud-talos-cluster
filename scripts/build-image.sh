#!/usr/bin/env bash
# Builds the cluster's Talos disk image on the builder server with imager,
# writes it over the builder's own disk and powers the builder off, so the
# block can snapshot that disk for the node to boot from. It freezes the root
# filesystem first and runs the overwrite from copies in a tmpfs, so once the
# image starts landing, nothing it runs writes to or runs from that disk. A
# failure in that overwrite ends it before the power-off.
# Usage: build-image.sh <imager image by digest> <machine configuration> <network router>
#   BUILD_WORK_DIR, BUILD_OUT_DIR, BUILD_DISK, BUILD_PULL_ATTEMPTS,
#   BUILD_RESOLV_CONF and BUILD_PROC default to /mnt/mitstack-build,
#   /var/lib/mitstack-build, /dev/sda, 30, /etc/resolv.conf and /proc.
# It exists because the image embeds every key of the cluster, so it is built
# inside the cluster's own Hetzner project and imager's output, which prints
# that configuration, is discarded. A failed step leaves the builder running,
# which the block reads as a failed build once its deadline passes.
set -euo pipefail

readonly work=${BUILD_WORK_DIR:-/mnt/mitstack-build}
# out lies on the disk, since imager writes its whole 4.2 GiB raw disk there before compressing it.
readonly out=${BUILD_OUT_DIR:-/var/lib/mitstack-build}
readonly disk=${BUILD_DISK:-/dev/sda}
readonly attempts=${BUILD_PULL_ATTEMPTS:-30}
readonly resolv_conf=${BUILD_RESOLV_CONF:-/etc/resolv.conf}
readonly proc=${BUILD_PROC:-/proc}
readonly staged="$work/bin"
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
  mkdir -p "$work/in" "$out"
  install -m 0600 "$config" "$work/in/config.yaml"
  rm -f "$config"
  if ! docker run --rm --privileged -v /dev:/dev -v "$work/in:/in:ro" -v "$out:/out" \
    "$imager" hcloud --arch amd64 --embedded-config-path /in/config.yaml > /dev/null 2>&1; then
    rm -f "$work/in/config.yaml"
    fail "imager failed; its output is discarded because it holds every cluster key"
  fi
  rm -f "$work/in/config.yaml"
}

# stage copies a program, its libraries and its loader, as ldd resolves them on the builder, into the tmpfs.
stage() {
  local program name arrow library
  program=$(command -v "$1")
  cp -L "$program" "$staged/"
  ldd "$program" | while read -r name arrow library _; do
    if [ "$arrow" = "=>" ] && [ "$library" = "not" ]; then
      fail "ldd could not resolve $name for $program"
    elif [ "$arrow" = "=>" ] && [ "${library#/}" != "$library" ]; then
      cp -L "$library" "$staged/"
    elif [ "${name#/}" != "$name" ]; then
      cp -L "$name" "$staged/ld.so"
    fi
  done
}

# overwrite_disk is the final stage, run by the tmpfs bash, and prints nothing, since a write to the frozen disk blocks.
overwrite_disk() {
  local staged=$1 image=$2 disk=$3 proc=$4
  exec > /dev/null 2>&1
  printf 'diag: final stage started\n' > /dev/kmsg
  "$staged/ld.so" --inhibit-cache --library-path "$staged" "$staged/zstd" -dc "$image" |
    "$staged/ld.so" --inhibit-cache --library-path "$staged" "$staged/dd" of="$disk" bs=4M oflag=direct conv=fsync status=none
  printf 'diag: dd finished, writing o\n' > /dev/kmsg
  printf o > "$proc/sysrq-trigger"
  printf 'diag: o written\n' > /dev/kmsg
  "$staged/ld.so" --inhibit-cache --library-path "$staged" "$staged/sleep" 90
  printf 'diag: 90s after o, dumping blocked tasks\n' > /dev/kmsg
  printf w > "$proc/sysrq-trigger"
  "$staged/ld.so" --inhibit-cache --library-path "$staged" "$staged/sleep" 60
  printf 'diag: 150s after o, dumping blocked tasks and CPUs\n' > /dev/kmsg
  printf w > "$proc/sysrq-trigger"
  printf l > "$proc/sysrq-trigger"
}

# write_disk freezes the root filesystem, which flushes the removed out, then execs the final stage from the tmpfs in
# the C locale, which reads no locale file from the disk.
write_disk() {
  local images=("$out"/*.raw.zst) program
  if [ "${#images[@]}" -ne 1 ] || [ ! -f "${images[0]}" ]; then
    fail "imager left no single image in $out"
  fi
  cp "${images[0]}" "$work/image.raw.zst"
  rm -rf "$out"
  mkdir -p "$staged"
  for program in bash zstd dd sleep; do
    stage "$program"
  done
  printf '%s\n' "set -euo pipefail" "$(declare -f overwrite_disk)" "overwrite_disk \"\$@\"" > "$staged/overwrite-disk"
  echo "build-image: writing the image to $disk and powering off"
  printf 'diag: freezing / on %s vCPUs, %s\n' "$(nproc)" "$(grep MemTotal /proc/meminfo)" > /dev/kmsg
  fsfreeze -f /
  LC_ALL=C exec "$staged/ld.so" --inhibit-cache --library-path "$staged" "$staged/bash" "$staged/overwrite-disk" \
    "$staged" "$work/image.raw.zst" "$disk" "$proc"
}

# diag_netconsole streams every kernel message to the gateway on UDP 6666, and names each device device_shutdown reaches.
diag_netconsole() {
  local route via dev mac target=/sys/kernel/config/netconsole/probe
  route=$(ip -o route get 10.0.0.2)
  via=$(sed -n 's/.* via \([^ ]*\).*/\1/p' <<< "$route")
  dev=$(sed -n 's/.* dev \([^ ]*\).*/\1/p' <<< "$route")
  ping -c 2 -W 2 "${via:-10.0.0.2}" > /dev/null || true
  mac=$(ip neigh show "${via:-10.0.0.2}" dev "$dev" | sed -n 's/.* lladdr \([^ ]*\).*/\1/p')
  printf 8 > /proc/sys/kernel/printk
  printf 1 > /sys/module/kernel/parameters/initcall_debug
  printf 30 > /proc/sys/kernel/hung_task_timeout_secs
  printf -- -1 > /proc/sys/kernel/hung_task_warnings
  modprobe netconsole
  mountpoint -q /sys/kernel/config || mount -t configfs none /sys/kernel/config
  mkdir -p "$target"
  printf '%s' "$dev" > "$target/dev_name"
  printf 10.0.0.4 > "$target/local_ip"
  printf 10.0.0.2 > "$target/remote_ip"
  printf 6666 > "$target/remote_port"
  printf '%s' "$mac" > "$target/remote_mac"
  printf 1 > "$target/enabled"
  printf 'diag: netconsole on %s via %s mac %s, kernel %s\n' "$dev" "$via" "$mac" "$(uname -r)" > /dev/kmsg
}

main() {
  [ "$#" -eq 3 ] || fail "usage: build-image.sh <imager image> <machine configuration> <network router>"
  route_through_gateway "$3"
  diag_netconsole || echo "build-image: diag netconsole failed" >&2
  printf 'diag: pulling imager\n' > /dev/kmsg
  pull_imager "$1"
  printf 'diag: imager pulled, building\n' > /dev/kmsg
  build "$1" "$2"
  printf 'diag: image built, %s\n' "$(ls -l "$out")" > /dev/kmsg
  write_disk
}

main "$@"
