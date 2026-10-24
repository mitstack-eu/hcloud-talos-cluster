# Build progress and failures

This page says what hcloud-talos-cluster reports while the builder makes the
cluster's disk image, and every reason a subscribe can be refused. It is for
anyone watching a subscribe that runs long or reading a refusal.
[How a subscribe runs](how-a-subscribe-runs.md) explains the builder and the
steps around it, and
[the workspace glossary](https://github.com/mitstack-eu/mitstack-workspace/blob/main/docs/glossary.md)
defines words such as validate and subscribe.

## What a build reports while it runs

No session can watch the builder, and its build script discards imager's
output, since that output prints every key of the cluster. Hetzner's metrics
of the builder are therefore the view of a running build. `readBuilderMetrics`
in `metrics.go` reads the `cpu` and `disk` types through `ServerMetrics` in
`hetzner.go`, a point a minute from the builder's creation to now.

While the builder runs, the cluster resource's validate detail ends with a
summary of those metrics, which `buildDetail` in `observe.go` words. It gives
the CPU over the last minute, the bytes written to disk over the last minute,
and the bytes written to disk since the builder's creation. This detail comes
from the tests:

```text
builder 1006, created at 2026-10-21T12:02:45Z, is building the cluster's image; Hetzner reads its CPU at 118.7% over the last minute, 120.0 MiB written to disk over the last minute and 180.0 MiB since its creation
```

Hetzner's CPU figure can pass 100% on a server with several cores. A cpx52
builder in the proof project read 118.7% while imager ran.

## When a build fails

A builder still running 30 minutes after its creation has failed, since its
script leaves a failed build running. A failed `zstd` or `dd` after the freeze
ends the script before the power-off, so it also leaves the builder running.
So does a private interface that gets no address within two minutes, and the
script's last line in the builder's cloud-init output names that interface.
Validate then asks for a re-drive, and that subscribe deletes the builder and
refuses with `image-build-failed`.

The `image-build-failed` refusal from `builderStep` in `resource.go` carries
the same summary as the validate detail. It adds when the disk last read or
wrote anything, and when the CPU was last above 10%, a level an idle server
stays under. The bytes written since creation tell a builder that never wrote
its 4.2 GiB image from one that wrote it and stopped later:

```text
refused:image-build-failed: the builder did not power itself off within 30m0s of its creation, so the cluster's image was not built and the builder is deleted; Hetzner reads its CPU at 2.0% over the last minute, 0 B written to disk over the last minute and 120.0 MiB since its creation; its disk last moved at 2026-10-21T12:03:45Z; its CPU was last above 10% at 2026-10-21T12:05:45Z; unsubscribe and subscribe again to rebuild the cluster
```

A metrics read that fails never fails validate or the refusal. The text then
says that the builder's Hetzner metrics could not be read. Before Hetzner
holds a point for the builder, it says that Hetzner holds no metrics for the
builder yet.

Neither text carries network figures. Hetzner answers every network series
empty for a server whose only network is private, which the builder's is, so
the block reads CPU and disk alone.

## Other refusals

A server Hetzner cannot place in the location fails the subscribe with
`server-unavailable`, naming Hetzner's reason. So does an `fsn1` that can
order neither gateway type, before any server is created. An install whose
create fails deletes the gateway and the builder, so the next call installs
anew.

Two servers of one role in one location, such as two nodes in `nbg1`, make
the cluster ambiguous, and so do two load balancers of one role in any
locations. Every call then refuses with `ambiguous-cluster` before it changes
anything.

The cluster's keys exist only in the call that mints them, the builder's user
data and the snapshot. A cluster that loses its gateway while its nodes run is
therefore refused rather than rebuilt, since the gateway's certificate
authority went with it.

A param or connection the block cannot use is refused before anything is
built, with `missing-param`, `invalid-param`, `missing-hetzner-access` or
`missing-github-access`, as [Params, connections and outputs](reference.md)
lists. An unsubscribe that cannot reach GitHub refuses with
`repository-left-in-place`, as
[How a subscribe runs](how-a-subscribe-runs.md#when-an-app-unsubscribes)
explains.
