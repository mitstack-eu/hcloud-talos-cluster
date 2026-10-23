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
| The node report, the enrolment Job and the token renewal | The block's own workloads, in `mitstack-node` | Inline |

Both policies exempt `mitstack-node`, so the block's own pods need no app
label and keep their route to the platform. Every image these components pull
is listed by digest in the gateway's mirror.

The node report also lists CloudNativePG's clusters, databases and roles in a
read of its own, which fails apart from the nodes and Applications it reads.
Each report carries a `checks` member,
`{"contract": "node-report/v1", "names": []}`, which names the contract the
report follows and none of the platform verdict's checks, because none of its
reads carries one.

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
such as `ghcr.io/mitstack-eu/talos-cluster-kit-tools:v0.7.0@sha256:2fdfcf40…`.
A build against a kit whose `tools.Image` carries the `unreleased` placeholder
tag refuses every subscribe.

## Where the logs go

The cluster names no log receiver, so each node keeps its logs, and the gateway
forwards no log port. The operator itself serves its recent log lines on
`/logs`, beside `/hook` for mitstack's requests and `/healthz` for a health
check, as `main.go` sets up.
