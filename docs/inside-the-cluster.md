# Inside the cluster

This page lists what runs inside a cluster hcloud-talos-cluster builds, where
the block's own workloads get their image, and where the logs go. It is for
anyone who deploys to the cluster or wants to know what it holds before they
do. [How a subscribe runs](how-a-subscribe-runs.md) explains how the cluster
comes up, and
[the workspace glossary](https://github.com/mitstack-eu/mitstack-workspace/blob/main/docs/glossary.md)
defines mitstack's own words.

## What runs inside the cluster

The cluster's components come from `components.Default()` in the kit release
`go.mod` requires, and `clusterComponents` in `manifests.go` adds the block's
own manifests to that set. The kit here is
[talos-cluster-kit](https://github.com/mitstack-eu/talos-cluster-kit). The
machine configuration names each served manifest at the gateway, such as
`https://10.0.0.2/manifests/kyverno.yaml`. The gateway fetches each one from
the kit's tagged release and checks its SHA-256. An inline manifest travels
inside the configuration itself.

| Component | What it does | How the node gets it |
|---|---|---|
| Cilium and Argo CD | The cluster's network and the sync of the cluster repository | Served |
| Kyverno | The admission engine that runs the two policies below | Served |
| `mitstack-require-app-label` | Refuses a pod without a non-empty `mitstack.dev/app` label | Inline |
| `mitstack-generate-default-deny` | Gives every namespace outside the system list a `mitstack-default-deny` network policy | Inline |
| CloudNativePG | The Postgres operator, in `cnpg-system` | Served, its namespace inline |
| `mitstack-database-slices` | Lets a database slice's Postgres instances reach the API server and each other, and admits the operator on port 8000 and every slice on port 5432 | Inline |
| The local path provisioner | Gives each storage claim that names no class a directory on its node's own disk, through the default StorageClass `local-path` | Served, its namespace and network policy inline |
| The node report, the enrolment Job and the token renewal | The block's own workloads, in `mitstack-node` | Inline |

Both policies exempt `mitstack-node`, so the block's own pods need no app
label and keep their route to the platform. Every image these components pull
is listed by digest in the gateway's mirror.

A storage claim, a PersistentVolumeClaim, that names no class gets `local-path`. Its size caps nothing, and its
data is lost with its node's server, so a workload that must outlive a node
keeps copies on other nodes itself.
[talos-cluster-kit's README](https://github.com/mitstack-eu/talos-cluster-kit/blob/v0.9.0/README.md#where-a-claim-keeps-its-data)
gives the details.

The node report also lists CloudNativePG's clusters, databases and roles in a
read of its own, which fails apart from the nodes and Applications it reads.
Each read runs under a time limit. When any other read, or the build of the
report itself, fails or times out, the cluster posts a report holding only an
`error` that names the read, its exit status or that it timed out, and the end
of its error output with no token in it. That report reaches mitstack's
node-reports API, so a cluster that cannot report says why instead of going
silent, and it names no nodes, so the cluster does not read as ready.
Each report carries a `checks` member,
`{"contract": "node-report/v1", "names": []}`, which names the contract the
report follows and none of the platform verdict's checks, because none of its
reads carries one. The report also carries the API server's key set as `jwks`,
which talos-cluster-kit's `report/scripts/report.sh` reads. The platform
publishes that key set under `issuerUrl`, as
[When each output is published](reference.md#when-each-output-is-published)
describes.

## How the components change

Talos applies these manifests when the node first boots, and on each later
boot it creates only what is missing. It never edits a resource that exists.
The block has no call that re-applies a node's configuration. A cluster built
from an older release therefore keeps the components it was built with, and
only a new subscribe gets a newer kit's components.

## Where the tools image comes from

The node report, the enrolment Job and the token renewal run in the
talos-cluster-kit tools image, which the kit's own release publishes and signs.
The block takes it from `tools.Image` in the kit release `go.mod` requires,
such as `ghcr.io/mitstack-eu/talos-cluster-kit-tools:v0.9.0@sha256:f61b51e2…`.
A build against a kit whose `tools.Image` carries the `unreleased` placeholder
tag refuses every subscribe.

## Where the logs go

The cluster names no log receiver, so each node keeps its logs, and the gateway
forwards no log port. The operator itself serves its recent log lines on
`/logs`, beside `/hook` for mitstack's requests and `/healthz` for a health
check, as `main.go` sets up.
