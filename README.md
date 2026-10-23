# hcloud-talos-cluster

This repository holds hcloud-talos-cluster, a mitstack block that gives an app
its own Kubernetes cluster on Hetzner Cloud. This README is for anyone who
wants to use the block, or to learn from it how a block is made, and it
assumes no earlier knowledge of mitstack. The first four sections say what the
block provides, what it needs, how it works and what each file is for. The
sections after them give the detail.

A few mitstack words recur below. Each is defined in full in
[the workspace glossary](https://github.com/mitstack-eu/mitstack-workspace/blob/main/docs/glossary.md).

| Word | What it means here |
|---|---|
| Block | A ready-made service that mitstack runs for you, such as a database or, here, a Kubernetes cluster |
| App | Your project in mitstack, which uses one or more blocks |
| Subscribe | What an app does to start using a block. Unsubscribe is how it stops |
| Operator | The program in this repository, which runs as a web service. mitstack sends it a request, signed with mitstack's own key, each time an app subscribes to the block, checks on it or unsubscribes |
| Param | A setting you fill in when your app subscribes, such as the app's name |
| Connection | An account you have linked to your app in mitstack, such as a Hetzner Cloud project or a GitHub account. mitstack passes the account's token to the block with each request, and the block acts for you with it |
| Resource | One thing the block creates and tracks, such as a server, a load balancer or the cluster repository |
| Output | A value the block hands back once it has built something, such as the cluster's address, which mitstack records for the app |
| Manifest | The file `manifest.yaml`, which tells mitstack what the block is, what it builds and which requests it answers |

## What the block provides

When your app subscribes, the block builds a Kubernetes cluster that belongs to
that app alone. Kubernetes is the common system for running containers across
several servers. The cluster has three servers, called nodes, one in each of
Hetzner's three European locations: `fsn1` in Falkenstein, `nbg1` in Nuremberg
and `hel1` in Helsinki.

The cluster's state store, etcd, is designed to keep working while any two of
the three nodes are up. All outbound traffic leaves through one server, the
gateway. Two load balancers, which spread incoming traffic across the nodes,
sit beside it in `fsn1`. Losing `fsn1` therefore stops the cluster's outbound
and web traffic.

The block also creates a private GitHub repository for the cluster, named
`<appName>-cluster`. Argo CD, a tool that runs inside the cluster, applies what
that repository holds to the cluster. A commit to the repository's `cluster/`
folder therefore changes what runs on the cluster.

In mitstack's terms, the block provides the type `hetzner.kubernetes-cluster`,
which the `provides` list in `manifest.yaml` declares. A type is the catalogue name of
one kind of resource. mitstack matches a block that requires a type with a
block that provides it.

The block hands back these outputs. `observe.go` sets them, and
[When each output is published](#when-each-output-is-published) gives the
detail.

| Output | What it is | Example | When it appears |
|---|---|---|---|
| `privateEndpoint` | The address of the cluster's Kubernetes API, on the cluster's private network | `https://10.0.0.3:6443` | Once the cluster is ready |
| `publicIpByLocation` | The public address of the ingress load balancer, the one that takes web traffic from the internet on ports 80 and 443, under its location | `{"fsn1":"203.0.113.10"}` | Once the cluster is ready |
| `subscriptionId` | The id of your app's subscription to the block | `sub_0123456789ab` | Once the cluster is ready |
| `clusterRepoUrl` | The web address of the cluster repository | `https://github.com/acme/shop-cluster` | Once the gateway and the repository exist, before the cluster is ready |
| `enrolmentTokenSha256` | A fingerprint of the one-time token the cluster presents when it first signs up with mitstack, called enrolment. mitstack checks the presented token against it, so it needs the fingerprint before the cluster signs up | A base32 SHA-256 digest | Once the gateway and the repository exist, before the cluster is ready |
| `repo` | The cluster repository's owner and name, recorded on the repository resource rather than the cluster | `acme/shop-cluster` | Once the repository exists with this subscription's mark, two GitHub topics the block sets on it |

The cluster is ready once every resource exists and the newest node report
names at least three nodes, every one Ready. The node report is a small
workload inside the cluster that posts the nodes' state to mitstack every five
minutes.

## What the block needs

The block reads three params: two you set, `appName` and `serverType`, and
`hostsPlatform`, which you leave unset. It also needs two connections and no
other block. Its manifest lists no `requires`, so no other block has to be in
place first.

`readParams` in `resource.go` reads the params. A subscribe with a param it
cannot use is refused before anything is built, with the reason in the last
column.

| Param | Required | What it sets | Allowed values |
|---|---|---|---|
| `appName` | Yes | The name of the cluster repository, `<appName>-cluster` | Any name. An empty one is refused with `missing-param` |
| `serverType` | No, `cx33` when empty | The Hetzner server type of each of the three nodes, and of the short-lived builder that makes the cluster's disk image | `cx23`, `cx33`, `cx43`, `cx53`, `cpx32`, `cpx52` or `cpx62`, as `allowedServerTypes` lists. Any other is refused with `invalid-param` |
| `hostsPlatform` | No | Nothing you can use today: this release builds no cluster that hosts mitstack's own stores | Leave it unset. `true` is refused with `invalid-param` |

Every type in that list is an x86 type, because the cluster's disk image is
built for x86.

The connections give the block its tokens. Subscribe needs both. Validate is
the read-only check mitstack sends while a subscription is being built. The
manifest's `verbConnections` names both connections for validate and
unsubscribe, so mitstack sends their tokens on those requests as well.

| Connection | What the block reads from it | What the block does with it | When it is missing |
|---|---|---|---|
| Hetzner | `api_token`, a Hetzner Cloud project's API token, read by `hetznerToken` in `resource.go` | Creates and deletes every server, load balancer, network, firewall and snapshot in that project, which Hetzner bills | Refused with `missing-hetzner-access` |
| GitHub | `token`, `owner` and the GitHub API address mitstack sends with them, read by `dialGitHub` in `repository.go` | Creates, reads and deletes the cluster repository under `owner` | Refused with `missing-github-access` |

## How it works

The operator is a Go program built on the operator kit,
`github.com/mitstack-eu/operator-kit/v3`. The operator kit is mitstack's Go
library for blocks, and it checks each signed request and answers it. The
nodes run Talos Linux, an operating system that runs only Kubernetes and is
managed through an API, with no shell and no SSH. The Talos install comes from
[talos-cluster-kit](https://github.com/mitstack-eu/talos-cluster-kit), a Go
module of Talos settings and manifests that this block imports.
[ADR 0095](https://github.com/mitstack-eu/mitstack/blob/main/docs/adr/0095-mitstack-clusters-run-talos-across-fsn1-nbg1-and-hel1-with-a-load-balancer-per-location.md)
records the design.

mitstack gives each request two minutes, and a cluster takes up to 45 minutes
to build. The operator therefore builds the cluster one step per request, and
mitstack sends a fresh request until the cluster is ready. Every Hetzner
resource carries the subscription id as a label, so each request finds the
cluster by its labels alone and needs no memory of the last one.

### When an app subscribes

1. You subscribe your app to the block and fill in the params. mitstack sends
   the operator a subscribe request with the params and both connections'
   tokens.
2. The operator checks the params and the tokens, and refuses with a named
   reason when one is missing or wrong, as
   [What the block needs](#what-the-block-needs) lists.
3. The first requests create the private network and the cluster repository.
   They then create a firewall with no rule, applied to every server so
   nothing on the internet can reach one. Last come the two load balancers in
   `fsn1`: the API one, which carries the Kubernetes API privately, and the
   ingress one, which takes web traffic.
   [What a subscribe builds](#what-a-subscribe-builds) describes each one.
4. One request creates the gateway, the cluster's only server with a public
   address, which carries the nodes' outbound traffic. It also creates a
   short-lived builder server. Every node boots from the cluster's own disk
   image, which holds the cluster's whole configuration, so no node needs
   start-up data. The builder builds that image and powers itself off, and
   the block saves its disk as a snapshot and deletes it.
   [What a build reports while it runs and when it fails](#what-a-build-reports-while-it-runs-and-when-it-fails)
   covers a slow or failed build.
5. The next three requests create one node each, in `fsn1`, `nbg1` and `hel1`,
   every one booted from the snapshot. The gateway then finds the first node,
   checks that its Talos API presents a certificate signed by this cluster's
   own certificate authority, and asks it through that API to start etcd.
   [How a subscribe proceeds](#how-a-subscribe-proceeds) walks every step and
   says which parts a live run has not yet shown.
6. Between subscribe requests, mitstack checks the cluster with a validate
   request. When a step has finished, the operator's answer asks mitstack to
   send the next subscribe. While the builder or a node is still busy, mitstack
   waits.
7. Inside the cluster, a Kubernetes Job, a task that runs once, signs the
   cluster up with mitstack. Argo CD starts applying the cluster repository,
   and the node report starts posting.
   [What runs inside the cluster](#what-runs-inside-the-cluster) lists every
   component.
8. Once the node report names three Ready nodes, the cluster reads ready and
   mitstack records every output.

In the code, `nextStep` in `resource.go` picks the one step a request takes,
and `Ensure` takes it. `stepLeft` in `observe.go` uses the same `nextStep` to
decide when validate asks for the next subscribe. An upgrade request, which
moves a subscription to a newer release of the block, adds whatever is missing
and never replaces a server.

### When an app unsubscribes

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

`Delete` in `teardown.go` runs the unsubscribe.

## What is in the repository

Every new block starts from the same files, which mitstack's scaffold, its
generator of new block repositories, writes. They come from the templates in
the mitstack workspace's
`platform/api/internal/scaffold/blockkitv1/`, plus `go.mod` and
`.github/workflows/block-verify.yml`. This block has most of them, and adds its
own files after.

### Files every block starts with

| File | What it holds in this block |
|---|---|
| `manifest.yaml` | What the block is, the resources it builds, the type it provides and the requests it answers |
| `main.go` | Starts the operator's web service: `/hook` takes mitstack's requests, `/logs` serves the operator's recent log lines and `/healthz` answers a health check. It reads `MITSTACK_JWKS_URL`, the address where mitstack publishes its signing keys, to check each request |
| `resource.go` | Reads the params and the Hetzner token, and takes each subscribe step |
| `Dockerfile` | Builds the operator's container image |
| `go.mod` | The Go module, and the versions of the modules it uses, among them the operator kit and talos-cluster-kit |
| `go.sum` | The checksums of those modules |
| `secrets_leak_test.go` | Proves that no token reaches an answer or the logs |
| `teardown_test.go` | Proves that unsubscribe deletes every resource, the snapshot included |
| `.github/` | `workflows/ci.yml`, which this block has in place of the scaffold's `block-verify.yml`. It runs the operator kit's publish workflow, which tests the block and publishes its image on a push to `main`, and runs gitleaks and shellcheck |

The scaffold's `resource_test.go`, `verbsemantics_test.go` and
`types/snapshot.json` are not in this repository. Its copy of the platform's
type snapshot sits at `testdata/type-snapshot.json` instead.

### Files this block adds

| File | What it does |
|---|---|
| `README.md` | This guide |
| `LICENSE` | The Business Source License 1.1, whose grant allows production use only for infrastructure the mitstack platform manages |
| `hetzner.go` | The block's Hetzner Cloud client. It offers no rebuild, resize or user-data update, so no request can change a running server |
| `identity.go` | The labels every Hetzner resource carries, by which each request finds the subscription's resources |
| `state.go` | Reads every Hetzner resource with the subscription's labels and sorts them by role |
| `install.go` | Mints the cluster's keys and creates the gateway and the builder in the same request |
| `userdata.go` | The start-up data the builder and the gateway boot with |
| `manifests.go` | Adds the block's own Kubernetes manifests to talos-cluster-kit's components, and names the tools image |
| `manifests/` | Those manifests: the Argo CD Applications, the Job that signs the cluster up, and the token renewal |
| `scripts/` | The shell scripts the servers run, to build the image, start the first node and fetch the cluster's tokens |
| `repository.go` | Creates, reads and deletes the cluster repository on GitHub |
| `observe.go` | The observers, which read each resource from Hetzner and GitHub and report it present, absent or not yet known. They also set the outputs and tell mitstack when to send the next subscribe |
| `metrics.go` | Reads the builder's CPU and disk metrics from Hetzner, for the progress and failure messages |
| `nodereports.go` | Reads the cluster's node reports from mitstack |
| `teardown.go` | Runs the unsubscribe |
| `harness_test.go` | The shared test fakes: Hetzner in memory, GitHub, the node reports, and an engine that plays mitstack |
| `subscribe_test.go` | Proves what a subscribe builds, every refusal and the outputs |
| `redrive_test.go` | Proves that one subscribe reaches three nodes through mitstack's repeated requests |
| `userdata_test.go` | Proves the builder's and the gateway's start-up data |
| `scripts_test.go` | Runs each script against fake commands |
| `hetzner_adapter_test.go` | Proves the Hetzner client's requests, such as a node with no user data and no public address |
| `manifest_test.go` | Proves the manifest's type, server types and description, and that every declared resource has an observer |
| `declared_resources_test.go` | Runs the operator kit's block guards |
| `testdata/` | `type-snapshot.json`, the platform's resource types the tests read, and `default-cluster.golden`, a default cluster's expected components |

## What a subscribe builds

A subscribe builds three control-plane nodes, one in each of `fsn1`, `nbg1`
and `hel1`, Hetzner Cloud's three European locations. The subscriber picks
no location, and the block reads no `location` param, because every cluster
spans all three. Etcd, the store that holds the cluster's state, keeps
quorum while two of its three members are up. Each node boots from the cluster's own disk image, which embeds the
whole machine configuration, so no node carries user data or holds a public
address.

The one gateway lives in `fsn1` and carries the network's only default route,
so losing `fsn1` stops every node's outbound traffic.

The cluster's one load balancer pair, an API and an ingress load balancer,
sits in `fsn1` beside the gateway. Losing `fsn1` already stops every node's
outbound traffic, so a pair in another location would add load balancers to
the cluster but no location it survives. Each load balancer reaches the nodes
in `nbg1` and `hel1` by private IP over the cluster's network. The
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
| Builder | A short-lived server in `fsn1`, of the subscription's `serverType` like the nodes, that builds the image with imager, writes it over its own disk and powers off. It freezes its root filesystem before the write, and copies of `bash`, `zstd`, `dd` and `sleep` in memory then write the image, wait a second and power it off, so the script runs nothing from the disk being overwritten. The wait guards against a `cpx32` builder staying on after the power-off request, and whether it prevents that is not yet established. Its user data carries the machine configuration gzipped, in cloud-init's `gz+b64` encoding, since as text it is over Hetzner's 32 KiB user data limit | `install` in `install.go`, `builderUserData` in `userdata.go`, `scripts/build-image.sh` |
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

Each subscribe call takes one step that waits on Hetzner and then answers
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
function a subscribe takes its step from. A run moves through these steps:

1. Calls create the network, the repository and the firewall. Each of the
   two load balancers then takes its own calls, the API one first: it is
   created bare, attached to the network at its fixed address, and given its
   services and its target, and an API one has its public interface
   disabled. Hetzner refuses either of the last two on a load balancer with
   no network, so the attach comes first.
2. One call mints the cluster's keys, renders the machine configuration with
   `nodeconfig.Render`, and creates the gateway and the builder, both
   stopped and off the network. It waits on the two creates together. The
   keys exist only in that call, so both servers are made in it.
3. The next calls attach the gateway at `10.0.0.2` before its first boot,
   then attach the builder, power the gateway on, and then power on the
   builder, which pulls imager through the gateway. Hetzner gives a server
   that joins with no address the network's first free one. The builder
   therefore joins after the gateway, or it would take `10.0.0.2` itself.
4. The builder builds the image with imager, Sidero Labs' image builder, and
   powers itself off. imager first writes the whole uncompressed disk, about
   4.2 GiB, into a directory on the builder's own disk. The builder then
   copies the compressed image, about 230 MB, into memory, because `dd`
   writes the image over that same disk. It also copies `bash`, `zstd`, `dd`,
   `sleep`, the dynamic loader and the libraries `ldd` names for them into
   memory. Then `fsfreeze -f /` flushes the root filesystem and blocks every
   later write to it. The copy of `bash` runs `zstd` into `dd`, each through
   the copied loader. It then waits a second with the copied `sleep`, and
   writes `o` to `/proc/sysrq-trigger` to power off. Once
   the image starts landing, nothing the script runs reads a program from
   the disk, and the script prints nothing. Other processes on the builder
   still run from the disk. A call that finds the builder off,
   with a finished power-on in Hetzner's record of its actions, starts the
   snapshot of its disk and returns without waiting, since a snapshot can
   outlast a call. Once the snapshot reads available, one call deletes the
   builder. Each of the next three calls creates the node of the first
   location that has none, in the order `fsn1`, `nbg1`, `hel1`, so one call
   never waits on more than one create.
5. The gateway starts etcd on the first node it finds. Each node keeps the address Hetzner's DHCP gives it
   and sends all other traffic through the network's router at `10.0.0.1`,
   whose route leads to the gateway. The gateway therefore finds a node
   among the addresses its flow record holds. It then checks that the node's
   Talos API, on port 50000, presents a certificate signed by the cluster's
   own CA, so an address alone is never trusted. Last, it runs
   talos-cluster-kit's etcd bootstrap once, on that one node. Whether the
   other two nodes then join etcd has not yet been shown by a live run, and
   neither has whether Hetzner creates a node in `nbg1` or `hel1` from a
   snapshot of a builder in `fsn1`.
6. Inside the cluster, a Job calls the platform's enrolment route once. It
   presents the one-time enrolment token and receives two tokens: the report
   token, which authenticates the node report, and the repository token, a
   GitHub token that lets Argo CD read the cluster repository. A CronJob
   trades the report token for a fresh repository token every 15 minutes.
   Argo CD then syncs `cluster/` and `repository-credentials/`, and the node
   report posts every five minutes.

Talos accepts the etcd bootstrap call only from its full-rights role, so the
gateway holds an admin talosconfig for it. A talosconfig is the client
credential the Talos API takes, and this one expires two hours after the
install. The gateway's bootstrap deletes its working copy on exit, while the
copy in the gateway's user data stays readable on the gateway until it
expires.

The cluster reads present once every resource is in place and the newest node
report since the last node's creation names at least three nodes, every one
Ready. Each step logs how long it took, as in this line from the tests:

```text
hcloud-talos-cluster: subscription sub_0123456789ab: the step to create the node in nbg1 from the snapshot took 0s
```

A builder still running 30 minutes after its creation has failed, since its
script leaves a failed build running. A failed `zstd` or `dd` after the freeze
ends the script before the power-off, so it also leaves the builder running.
Validate then asks for a re-drive, and
that subscribe deletes the builder and refuses with `image-build-failed`. A
server Hetzner cannot place in the location fails the subscribe with
`server-unavailable`, naming Hetzner's reason. So does an `fsn1` that can
order neither gateway type, before any server is created. An install whose
create fails deletes the gateway and the builder, so the next call installs
anew. Two servers of one role in one location, such as two nodes in `nbg1`,
make the cluster ambiguous, and so do two load balancers of one role in any
locations. Every call then refuses with `ambiguous-cluster` before it changes
anything.

The cluster's keys exist only in the call that mints them, the builder's user
data and the snapshot. A cluster that loses its gateway while its nodes run is
therefore refused rather than rebuilt, since the gateway's certificate
authority went with it. Unsubscribe deletes every server, load balancer,
snapshot, firewall and network, then the repository when it carries the
subscription's mark.

## What a build reports while it runs and when it fails

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

The `image-build-failed` refusal from `builderStep` in `resource.go` carries
the same summary. It adds when the disk last read or wrote anything, and when
the CPU was last above 10%, a level an idle server stays under. The bytes
written since creation tell a builder that never wrote its 4.2 GiB image from
one that wrote it and stopped later:

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

## When each output is published

Each subscribe and validate answer carries one outcome per declared resource,
and the platform records each resource's outputs on that resource's row. The
cluster resource, `hetzner.kubernetes-cluster`, is the declared resource that
provides the block's kind. It reads present only once a node report reads
every node Ready, and a node reports only after it has enrolled. The cluster
resource therefore carries the two values the enrolment route judges a node by
while it still reads unknown, so the platform holds them before any node enrols.

| Output | While the cluster reads unknown | Once the cluster reads present |
|---|---|---|
| `enrolmentTokenSha256`, the base32 SHA-256 digest of the nodes' one-time enrolment token, read from the gateway's `mitstack.dev/enrolment-sha256` label | Once the gateway carries that label and the marked cluster repository exists | Yes |
| `clusterRepoUrl`, the web address of the cluster repository, such as `https://github.com/acme/shop-cluster` | Once the gateway carries that label and the marked cluster repository exists | Yes |
| `privateEndpoint`, the API load balancer as `https://10.0.0.3:6443` | No | Yes |
| `publicIpByLocation`, the ingress load balancer's public address under its location, such as `{"fsn1":"203.0.113.10"}` | No | Yes |
| `subscriptionId` | No | Yes |

The cluster resource carries both enrolment outputs or neither, and no other
resource carries either. It carries neither when the request cannot name the
cluster repository, such as when it has no GitHub token. It also carries
neither when the repository does not exist or lacks the subscription's mark, or
when the gateway has no digest label. Its status detail then names what is
missing. In a subscribe the operator kit observes the resources in manifest
order and stops at the first one not present, so it reaches the cluster
resource once all three nodes exist. A validate observes every resource.
`clusterState.enrolment` in `observe.go` makes the outputs of a cluster not yet
present, and `clusterState.present` makes those of a present one.

## What runs inside the cluster

The cluster's components come from `components.Default()` in the kit release
`go.mod` requires, and `clusterComponents` in `manifests.go` adds the block's
own manifests to that set. The machine configuration names each served
manifest at the gateway, such as `https://10.0.0.2/manifests/kyverno.yaml`.
The gateway fetches each one from the kit's tagged release and checks its
SHA-256. An inline manifest travels inside the configuration itself.

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
is listed by digest in the gateway's mirror. The node report also lists
CloudNativePG's clusters, databases and roles in a read of its own, which
fails apart from the nodes and Applications it reads. Each report carries a
`checks` member, `{"contract": "node-report/v1", "names": []}`, which names
the contract the report follows and none of the platform verdict's checks,
because none of its reads carries one.

Talos applies these manifests when the node first boots, and on each later
boot it creates only what is missing. It never edits a resource that exists. The block has no call that re-applies a node's configuration. A
cluster built from an older release therefore keeps the components it was
built with, and only a new subscribe gets a newer kit's components.

## Where the tools image and the logs come from

The node report, the enrolment Job and the token renewal run in the
talos-cluster-kit tools image, which the kit's own release publishes and signs.
The block takes it from `tools.Image` in the kit release `go.mod` requires,
such as `ghcr.io/mitstack-eu/talos-cluster-kit-tools:v0.7.0@sha256:2fdfcf40…`.
A build against a kit whose `tools.Image` carries the `unreleased` placeholder
tag refuses every subscribe.

The cluster names no log receiver, so each node keeps its logs, and the gateway
forwards no log port.

The operator reads its settings from the environment its host gives it, in
`main.go` and `repository.go`:

| Variable | Required | What it sets |
|---|---|---|
| `MITSTACK_JWKS_URL` | Yes. The operator exits at start when it is unset | The address where mitstack publishes its signing keys, which the operator checks each request against |
| `LISTEN_ADDR` | No, `:8080` when unset | The address the operator's web service listens on |
| `MITSTACK_GITHUB_API_HOSTS` | No | A comma-separated list of GitHub API hosts the operator accepts beside `api.github.com` |

## How you change the block

The tests run every verb against an in-memory Hetzner project, a GitHub fake
and a fake of the platform's node reports, in `harness_test.go`. Its `engine`
plays the platform's settle check, sending signed requests to the block's own
handler. The script
tests run each embedded script with bash against fake commands. Run the suites
from the repository root:

```bash
go vet ./...
go test ./...
shellcheck scripts/*.sh
```

| Test file | What it proves |
|---|---|
| `subscribe_test.go` | A node in each of the three locations on one network, one API and one ingress load balancer in `fsn1` at their fixed addresses, the ingress address under `fsn1`, no node with user data, only the gateway with a public address, every refusal, the builder's metrics in the build progress and the `image-build-failed` refusal, and the cluster's enrolment outputs while it waits on its nodes' report |
| `redrive_test.go` | One subscribe reaching all three nodes through the platform's re-drives, validate asking for one exactly when a subscribe has a step to take, at most one Hetzner action waited and one node created per call, and a doubled re-drive making nothing twice |
| `userdata_test.go` | The builder's embedded configuration with the API load balancer's address in its certificates and the cluster's components, its user data within Hetzner's limit, the gateway's two cloud-config parts, mirror and served manifests, the node report's `checks` member, and a default cluster's components, mirror and configuration documents against `testdata/default-cluster.golden` |
| `scripts_test.go` | The build, the first-node bootstrap and the cluster's credential calls |
| `teardown_test.go` | Unsubscribe deletes every resource, the snapshot included |
| `declared_resources_test.go`, `secrets_leak_test.go` | The operator kit's block guards and that no token reaches an answer or the logs |
