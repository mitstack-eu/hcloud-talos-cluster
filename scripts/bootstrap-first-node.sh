#!/usr/bin/env bash
# Finds the cluster's first node among the addresses the gateway has seen on
# the private network, proves it is the cluster's by its Talos API's
# certificate, and runs the kit's etcd bootstrap on it once. It deletes the
# admin talosconfig on every exit.
# Usage: bootstrap-first-node.sh <subnet prefix> <gateway address> <talosconfig> <deadline in Unix seconds>
#   BOOTSTRAP_FLOWS_LOG and BOOTSTRAP_POLL_SECONDS default to
#   /var/log/mitstack/flows.log, the kit's record of each new flow, and 15.
# It exists because Hetzner assigns the node its private address after the
# gateway's user data is fixed, and the talosconfig verifies the node's
# certificate against the cluster's own CA, so an address alone is never trusted.
set -euo pipefail

readonly flows=${BOOTSTRAP_FLOWS_LOG:-/var/log/mitstack/flows.log}
readonly poll=${BOOTSTRAP_POLL_SECONDS:-15}
readonly probe_timeout=10
talosconfig="" work=""

fail() {
  echo "bootstrap-first-node: $*" >&2
  exit 1
}

cleanup() {
  [ -z "$talosconfig" ] || rm -f "$talosconfig"
  [ -z "$work" ] || rm -rf "$work"
}

# candidates prints each source address of the subnet in the flow record, apart from the gateway's own.
candidates() {
  local prefix=$1 self=$2
  [ -f "$flows" ] || return 0
  grep -o "src=${prefix//./\\.}[0-9]*" "$flows" | cut -d= -f2 | sort -u | grep -v -x -F "$self" || true
}

is_cluster_node() {
  timeout "$probe_timeout" talosctl --talosconfig "$talosconfig" -e "$1" -n "$1" version > /dev/null 2>&1
}

# bootstrap hands the kit's script a copy, since that script deletes its talosconfig whatever the outcome.
bootstrap() {
  cp "$talosconfig" "$work/talosconfig"
  mitstack-talos-bootstrap --node "$1" --talosconfig "$work/talosconfig"
}

main() {
  [ "$#" -eq 4 ] || fail "usage: bootstrap-first-node.sh <subnet prefix> <gateway address> <talosconfig> <deadline>"
  local prefix=$1 self=$2 deadline=$4 address
  talosconfig=$3
  trap cleanup EXIT
  [ -f "$talosconfig" ] || fail "no talosconfig at $talosconfig"
  work=$(mktemp -d)
  while [ "$(date +%s)" -lt "$deadline" ]; do
    for address in $(candidates "$prefix" "$self"); do
      if is_cluster_node "$address" && bootstrap "$address"; then
        echo "bootstrap-first-node: bootstrapped etcd on $address"
        return 0
      fi
    done
    sleep "$poll"
  done
  fail "no node of the cluster answered before the talosconfig expired"
}

main "$@"
