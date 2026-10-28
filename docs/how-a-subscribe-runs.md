# How a subscribe runs

This page explains what hcloud-talos-cluster builds when an app subscribes,
the order it builds it in, and how an unsubscribe takes it down. It is for
anyone who wants to know what happens in their Hetzner project and GitHub
account, or who studies the block to build their own. Words such as subscribe,
operator and resource are defined in
[the workspace glossary](https://github.com/mitstack-eu/mitstack-workspace/blob/main/docs/glossary.md)
and on [Params, connections and outputs](reference.md).

## Why the operator takes one step per request

mitstack gives each request two minutes, and a cluster takes up to 90 minutes
to build. The operator therefore builds the cluster one step per request, and
mitstack sends a fresh request until the cluster is ready. Every Hetzner
resource carries the subscription id as a label, so each request finds the
cluster by its labels alone and needs no memory of the last one.

The operator is a Go program built on the operator kit,
`github.com/mitstack-eu/operator-kit/v3`. The operator kit is mitstack's Go
library for blocks, and it checks each signed request and answers it.

The nodes run Talos Linux, an operating system that runs only Kubernetes and
is managed through an API, with no shell and no SSH. The Talos install comes
from [talos-cluster-kit](https://github.com/mitstack-eu/talos-cluster-kit), a
Go module of Talos settings and manifests that this block imports.
[ADR 0095](https://github.com/mitstack-eu/mitstack/blob/main/docs/adr/0095-mitstack-clusters-run-talos-across-fsn1-nbg1-and-hel1-with-a-load-balancer-per-location.md)
records the design.

## What a subscribe builds

A subscribe builds three control-plane nodes, one in each of `fsn1`, `nbg1`
and `hel1`, Hetzner Cloud's three European locations. The subscriber picks
no location, and the block reads no `location` param, because every cluster
spans all three. Etcd, the store that holds the cluster's state, keeps
quorum while two of its three members are up. Each node boots from the
cluster's own disk image, which embeds the whole machine configuration, so no
node carries user data or holds a public address.

The one gateway lives in `fsn1` and carries the network's only default route,
so losing `fsn1` stops every node's outbound traffic.

The cluster's one load balancer pair, an API and an ingress load balancer,
sits in `fsn1` beside the gateway. Losing `fsn1` already stops every node's
outbound traffic, so a pair in another location would add load balancers to
the cluster but no location it survives. Losing `fsn1` therefore stops both
the cluster's outbound and its web traffic.

Each load balancer reaches the nodes in `nbg1` and `hel1` by private IP over
the cluster's network. The
[Hetzner load balancer FAQ](https://docs.hetzner.com/cloud/load-balancers/faq/)
allows targets in other locations of the load balancer's network zone, here
`eu-central`, and asks that a server target share its Cloud Network.

| Resource | What it does | Where |
|---|---|---|
| Network `10.0.0.0/16` | Holds every server and load balancer, and routes `0.0.0.0/0` to the gateway at `10.0.0.2` | `ensureNetwork` in `resource.go` |
| Firewall | Holds no rule, and is applied to every server, so nothing reaches a server from the internet | `createFirewall` in `resource.go` |
| Gateway | The only server with a public address, in `fsn1`. It runs the kit's cloud-config, which forwards and records the nodes' DNS, NTP and HTTPS and serves their image mirror. It is a `cx23`, or a `cpx22` when Hetzner reads `cx23` as sold out in `fsn1` | `install` and `orderableGatewayType` in `install.go` |
| API load balancer, in `fsn1` | Carries the Kubernetes API on port 6443 on the private network to every node, with no public interface | `apiLoadBalancer` and `loadBalancerStep` in `resource.go` |
| Ingress load balancer, in `fsn1` | Serves ports 80 and 443 on its public address to every node's NodePorts 30080 and 30081, with PROXY protocol | `ingressLoadBalancer` and `loadBalancerStep` in `resource.go` |
| Builder | A short-lived server in `fsn1`, of the subscription's `serverType` like the nodes, that builds the image with imager, writes it over its own disk and powers off. Its private address comes from DHCP through Hetzner's `hc-net-ifup` unit, and cloud-init's final stage can start the script before that lease lands, so the script first waits up to two minutes for the lease and its route to `10.0.0.1`. It freezes its root filesystem before the write, and copies of `bash`, `zstd`, `dd` and `sleep` in memory then write the image, wait a second and power it off, so the script runs nothing from the disk being overwritten. The wait guards against a `cpx32` builder staying on after the power-off request, and whether it prevents that is not yet established. Before the write it waits until the gateway serves the manifest its prepare fetches last, under the gateway's CA, which its user data carries, so no node boots before the mirror is complete. Its user data carries the machine configuration and the script gzipped, in cloud-init's `gz+b64` encoding, since as text they are over Hetzner's 32 KiB user data limit | `install` in `install.go`, `builderUserData` in `userdata.go`, `scripts/build-image.sh` |
| Snapshot | The builder's disk, which every node boots from | `builderStep` in `resource.go` |
| Node, one per location | A control-plane node booted from the snapshot and named after its location, such as `shop-sub-0123-node-nbg1` | `missingNodeStep` and `createNode` in `resource.go` |
| Cluster repository | A private GitHub repository, `<appName>-cluster`, that Argo CD syncs | `repository.go` |

Each load balancer joins the network at a fixed address, so neither takes the
gateway's `10.0.0.2`. The API load balancer holds `10.0.0.3`, the
control-plane endpoint, and the ingress load balancer holds `10.0.0.4`, as
`apiAddress` and `ingressAddress` in `resource.go` set them.

The API server's certificate names the API load balancer's address. Both load
balancers find their targets by the label `mitstack.dev/role=node`, so they
reach the nodes of all three locations, and a replaced node takes its place
with no edit to either load balancer. Every Hetzner resource carries the labels
`mitstack.dev/block` and `mitstack.dev/subscription`, and every verb finds the
cluster by them alone. A server's or load balancer's location is read from
Hetzner's own record of it.

## How a subscribe proceeds

You subscribe your app to the block and fill in the params. mitstack sends the
operator a subscribe request with the params and both connections' tokens. The
operator checks the params and the tokens first, and refuses with a named
reason when one is missing or wrong, as
[Params, connections and outputs](reference.md#params) lists.

Each subscribe call then takes one step that waits on Hetzner and then answers
pending. A step is one change to the cluster and the wait on the Hetzner
action that carries it out, such as attaching a load balancer to the network.
The bound exists because a call that waited on every action of the install ran
past the platform's two-minute verb timeout. The network and the repository
need no wait, so a call makes them beside its step. The handler observes each
resource once per call, through `VerbBudget: 1` in `main.go`, so no call is held
open polling either.

While a subscription settles, the platform polls it with validate. The block's
validate answers `redrive` beside `pending` whenever a subscribe sent now would
take a step or refuse, judged from what Hetzner and GitHub hold:

```json
{"pending": true, "redrive": true}
```

The platform then re-drives subscribe, which means it sends a fresh subscribe,
as [ADR 0097](https://github.com/mitstack-eu/mitstack/blob/main/docs/adr/0097-the-settle-check-re-drives-subscribe-when-an-operator-reports-a-long-step-finished.md)
records. While the builder builds, the snapshot images or the node boots,
validate answers `pending` alone and the platform waits. `stepLeft` in
`observe.go` makes that judgement from `nextStep` in `resource.go`, the same
function a subscribe takes its step from, and `Ensure` takes the step. A run
moves through these steps:

1. Calls create the network, the repository and the firewall. The firewall
   has no rule and is applied to every server, so nothing on the internet can
   reach one. Each of the two load balancers then takes its own calls, the
   API one first: it is created bare, attached to the network at its fixed
   address, and given its services and its target, and an API one has its
   public interface disabled. Hetzner refuses either of the last two on a
   load balancer with no network, so the attach comes first.
2. One call mints the cluster's keys, renders the machine configuration with
   `nodeconfig.Render`, and creates the gateway and the builder, both
   stopped and off the network. It waits on the two creates together. The
   keys exist only in that call, so both servers are made in it.
3. The next calls attach the gateway at `10.0.0.2` before its first boot,
   then attach the builder, power the gateway on, and then power on the
   builder, which pulls imager through the gateway. Hetzner gives a server
   that joins with no address the network's first free one. The builder
   therefore joins after the gateway, or it would take `10.0.0.2` itself.
4. The builder builds the image with imager, Sidero Labs' image builder, waits
   until the gateway serves the manifest its prepare fetches last, which means
   the mirror is complete, and powers itself off. imager first writes the
   whole uncompressed disk, about 4.2 GiB, into a directory on the builder's
   own disk. The builder then copies the compressed image, about 230 MB, into
   memory, because `dd` writes the image over that same disk. It also copies `bash`, `zstd`, `dd`,
   `sleep`, the dynamic loader and the libraries `ldd` names for them into
   memory. Then `fsfreeze -f /` flushes the root filesystem and blocks every
   later write to it. The copy of `bash` runs `zstd` into `dd`, each through
   the copied loader. It then waits a second with the copied `sleep`, and
   writes `o` to `/proc/sysrq-trigger` to power off. Once the image starts
   landing, nothing the script runs reads a program from the disk, and the
   script prints nothing. Other processes on the builder still run from the
   disk. A call that finds the builder off, with a finished power-on in
   Hetzner's record of its actions, starts the snapshot of its disk and
   returns without waiting, since a snapshot can outlast a call. Once the
   snapshot reads available, one call deletes the builder. Each of the next
   three calls creates the node of the first location that has none, in the
   order `fsn1`, `nbg1`, `hel1`, so one call never waits on more than one
   create.
5. The gateway starts etcd on the first node it finds. Each node keeps the
   address Hetzner's DHCP gives it and sends all other traffic through the
   network's router at `10.0.0.1`, whose route leads to the gateway. The
   gateway therefore finds a node among the addresses its flow record holds.
   It then checks that the node's Talos API, on port 50000, presents a
   certificate signed by the cluster's own CA, so an address alone is never
   trusted. Last, it runs talos-cluster-kit's etcd bootstrap once, on that
   one node. Whether the other two nodes then join etcd has not yet been
   shown by a live run, and neither has whether Hetzner creates a node in
   `nbg1` or `hel1` from a snapshot of a builder in `fsn1`.
6. Inside the cluster, a Job, a Kubernetes task that runs once, calls the
   platform's enrolment route once. It presents the one-time enrolment token
   and receives two tokens: the report token, which authenticates the node
   report, and the repository token, a GitHub token that lets Argo CD read
   the cluster repository. A CronJob trades the report token for a fresh
   repository token every 15 minutes. Argo CD then syncs `cluster/` and
   `repository-credentials/`, and the node report posts every five minutes.
   [Inside the cluster](inside-the-cluster.md) lists every component.

Talos accepts the etcd bootstrap call only from its full-rights role, so the
gateway holds an admin talosconfig for it. A talosconfig is the client
credential the Talos API takes, and this one expires two hours after the
install. The gateway's bootstrap deletes its working copy on exit, while the
copy in the gateway's user data stays readable on the gateway until it
expires.

The cluster reads present once every resource is in place and the newest node
report since the last node's creation names at least three nodes, every one
Ready. mitstack then records every output. Each step logs how long it took, as
in this line from the tests:

```text
hcloud-talos-cluster: subscription sub_0123456789ab: the step to create the node in nbg1 from the snapshot took 0s
```

An upgrade request, which moves a subscription to a newer release of the
block, adds whatever is missing and never replaces a server.
[Build progress and failures](build-progress-and-failures.md) covers a slow or
failed build and every refusal a subscribe can end in.

## When an app unsubscribes

`Delete` in `teardown.go` runs the unsubscribe.

1. You remove the block from your app. mitstack sends the operator an
   unsubscribe request with both connections' tokens.
2. The operator deletes every server, then every load balancer, then the
   snapshot, the firewall and the network, each found by the subscription's
   labels. The snapshot goes too, because it holds every key of the cluster.
3. It then deletes the cluster repository, when the repository carries this
   subscription's mark, two GitHub topics. A repository without the mark stays
   untouched.
4. When the GitHub connection is unusable, the operator still deletes every
   Hetzner resource, then refuses with `repository-left-in-place` and names the
   repository it kept.
5. mitstack counts the unsubscribe finished once every resource reads absent.
