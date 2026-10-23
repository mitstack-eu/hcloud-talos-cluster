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
| Kyverno | The admission engine that runs the four policies below | Served |
| `mitstack-require-app-label` | Refuses a pod without a non-empty `mitstack.dev/app` label | Inline |
| `mitstack-generate-default-deny` | Gives every namespace outside the system list a `mitstack-default-deny` network policy | Inline |
| Istio in ambient mode | Puts every pod outside `kube-system`, `istio-system` and `kyverno` in a mesh, where ztunnel on each node wraps every call between two pods in mutual TLS with a certificate naming the caller's workload | Served |
| `mitstack-strict-mtls` | Makes every pod in the mesh refuse a call that carries no workload identity | Inline |
| `mitstack-require-mesh` and `mitstack-require-strict-mtls` | Refuse a pod the mesh would leave out, and a policy that lets a pod take a call with no workload identity | Inline |
| `mitstack-ambient-probes` | A Cilium policy that admits the kubelet's health probes, which reach a pod in the mesh from `169.254.7.127` | Inline |
| `mitstack-mesh-cloudnative-pg` | Lets the CloudNativePG operator's webhook port take the API server's calls without an identity, and keeps every caller but the operator off a Postgres instance's port 8000 and everyone off its ports other than 5432 and 8000 | Inline |
| `mitstack-mesh-argocd` | Admits the mesh's port 15008 between the same Argo CD pods that Argo CD's own network policies admit | Inline |
| CloudNativePG | The Postgres operator, in `cnpg-system` | Served, its namespace inline |
| `mitstack-database-slices` | Lets a database slice's Postgres instances reach the API server and each other, and admits the operator on port 8000 and every slice on port 5432, each also on the mesh's port 15008 | Inline |
| The local path provisioner | Gives each storage claim that names no class a directory on its node's own disk, through the default StorageClass `local-path` | Served, its namespace and network policy inline |
| The node report, the enrolment Job and the token renewal | The block's own workloads, in `mitstack-node` | Inline |

Every Kyverno policy exempts `mitstack-node`, so the block's own pods need no
app label and keep their route to the platform. Every image these components
pull is listed by digest in the gateway's mirror.

The block's own pods sit in the mesh too. Their calls go to the API server,
the Talos API and the platform, none of them a pod in the mesh, so those calls
leave the node as they are. Only the CloudNativePG webhook's port takes a call
with no workload identity, since the API server holds none.
[talos-cluster-kit's README](https://github.com/mitstack-eu/talos-cluster-kit/blob/v0.11.0/README.md#how-every-call-between-pods-proves-who-makes-it)
says how the mesh works, how long a certificate lives and how to read one.

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
such as `ghcr.io/mitstack-eu/talos-cluster-kit-tools:v0.11.0@sha256:db0c3184…`.
A build against a kit whose `tools.Image` carries the `unreleased` placeholder
tag refuses every subscribe.

## Where the logs go

The cluster names no log receiver, so each node keeps its logs, and the gateway
forwards no log port. The operator itself serves its recent log lines on
`/logs`, beside `/hook` for mitstack's requests and `/healthz` for a health
check, as `main.go` sets up.
