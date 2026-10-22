# hcloud-talos-cluster

This repository holds hcloud-talos-cluster, the mitstack block that builds a
Talos Kubernetes cluster on Hetzner Cloud servers and provides the type
`hetzner.kubernetes-cluster`. It is for the engineers who build and maintain
the block. A block is a unit of infrastructure an app subscribes to, and its
operator is the service that serves the block's lifecycle calls, as
[the workspace glossary](https://github.com/mitstack-eu/mitstack-workspace/blob/main/docs/glossary.md)
defines. Talos Linux is an operating system that runs only Kubernetes and is
managed through an API, with no shell and no SSH. The Talos install comes from
[talos-cluster-kit](https://github.com/mitstack-eu/talos-cluster-kit), a Go
module of renders that both Hetzner cluster blocks import, apart from the
operator kit the glossary calls the kit. The block's lifecycle handling comes
from that operator kit, `github.com/mitstack-eu/operator-kit/v3`, and
[ADR 0095](https://github.com/mitstack-eu/mitstack/blob/main/docs/adr/0095-mitstack-clusters-run-talos-across-fsn1-nbg1-and-hel1-with-a-load-balancer-per-location.md)
records the design.

The block is licensed under the Business Source License 1.1, with a grant
that allows production use only for infrastructure the mitstack platform
manages. [LICENSE](LICENSE) holds the full terms.

## What a subscribe builds

A subscribe builds one control-plane node in the location the subscriber
picks: `fsn1`, `nbg1` or `hel1`, with `nbg1` when none is given. Any other
location is refused with a message naming the three, before any call to
Hetzner. The node boots from the cluster's own disk image, which embeds the
whole machine configuration, so the node carries no user data and has no
public address.

| Resource | What it does | Where |
|---|---|---|
| Network `10.0.0.0/16` | Holds every server and load balancer, and routes `0.0.0.0/0` to the gateway at `10.0.0.2` | `ensureNetwork` in `resource.go` |
| Firewall | Holds no rule, and is applied to every server, so nothing reaches a server from the internet | `createFirewall` in `resource.go` |
| Gateway | The only server with a public address. It runs the kit's cloud-config, which forwards and records the node's DNS, NTP and HTTPS and serves its image mirror | `install` in `install.go` |
| API load balancer | Carries the Kubernetes API on port 6443 at `10.0.0.3` on the private network, with no public interface | `apiLoadBalancer` and `loadBalancerStep` in `resource.go` |
| Ingress load balancer | Serves ports 80 and 443 on its public address to the NodePorts 30080 and 30081, with PROXY protocol, and sits at `10.0.0.4` on the network | `ingressLoadBalancer` and `loadBalancerStep` in `resource.go` |
| Builder | A short-lived server that builds the image with imager, writes it over its own disk and powers off. Its user data carries the machine configuration gzipped, in cloud-init's `gz+b64` encoding, since as text it is over Hetzner's 32 KiB user data limit | `builderUserData` in `userdata.go`, `scripts/build-image.sh` |
| Snapshot | The builder's disk, which every node boots from | `builderStep` in `resource.go` |
| Node | The control-plane node, booted from the snapshot | `createNode` in `resource.go` |
| Cluster repository | A private GitHub repository, `<appName>-cluster`, that Argo CD syncs | `repository.go` |

Both load balancers find their target by the label
`mitstack.dev/role=node`, so a replaced node takes its place with no edit to
either. Every Hetzner resource carries the labels `mitstack.dev/block` and
`mitstack.dev/subscription`, and every verb finds the cluster by them alone.

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

1. Calls create the network, the repository and the firewall. Each load
   balancer then takes its own calls: it is created bare, attached to the
   network at its fixed address, and given its services and its target, and
   the API one has its public interface disabled. Hetzner refuses either of
   the last two on a load balancer with no network, so the attach comes first.
2. One call mints the cluster's keys, renders the machine configuration with
   `nodeconfig.Render`, and creates the gateway and the builder, both
   stopped and off the network. It waits on the two creates together. The
   keys exist only in that call, so both servers are made in it.
3. The next calls attach the gateway at `10.0.0.2` before its first boot,
   then attach the builder, power the gateway on, and then power on the
   builder, which pulls imager through the gateway. Hetzner gives a server
   that joins with no address the network's first free one. The builder
   therefore joins after the gateway, or it would take `10.0.0.2` itself.
4. The builder builds the image and powers itself off. A call that finds it
   off, with a finished power-on in Hetzner's record of its actions, starts
   the snapshot of its disk and returns without waiting, since imaging can
   outlast a call. Once the snapshot reads available, one call deletes the
   builder and the next creates the node from the snapshot.
5. The gateway starts etcd, the store that holds the cluster's state, on the
   node. It finds the node among the addresses its flow record holds. It then
   checks that the node's Talos API, on port 50000, presents a certificate
   signed by the cluster's own CA, so an address alone is never trusted. Last,
   it runs talos-cluster-kit's etcd bootstrap once.
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
report since the node's creation reads every node Ready. Each step logs how
long it took, as in this line from the tests:

```text
hcloud-talos-cluster: subscription sub_0123456789ab: the step to power on the gateway took 0s
```

A builder still running 30 minutes after its creation has failed, since its
script leaves a failed build running. Validate then asks for a re-drive, and
that subscribe deletes the builder and refuses with `image-build-failed`. A
server Hetzner cannot place in the location fails the subscribe with
`server-unavailable`, naming Hetzner's reason. An install whose create fails
deletes the gateway and the builder, so the next call installs anew.

The cluster's keys exist only in the call that mints them, the builder's user
data and the snapshot. A cluster that loses its gateway while the node runs is
therefore refused rather than rebuilt, since the gateway's certificate
authority went with it. Unsubscribe deletes every server, load balancer,
snapshot, firewall and network, then the repository when it carries the
subscription's mark.

## Where the tools image and the logs come from

The node report, the enrolment Job and the token renewal run in the
talos-cluster-kit tools image, which the kit's own release publishes and signs.
The block takes it from `tools.Image` in the kit release `go.mod` requires,
such as `ghcr.io/mitstack-eu/talos-cluster-kit-tools:v0.3.0@sha256:16c519dc…`.
A build against a kit whose `tools.Image` carries the `unreleased` placeholder
tag refuses every subscribe.

The cluster names no log receiver, so each node keeps its logs, and the gateway
forwards no log port. The operator needs no setting from its host.

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
| `subscribe_test.go` | A one-node cluster in each of the three locations, the node with no user data, only the gateway with a public address, and every refusal |
| `redrive_test.go` | One subscribe reaching the node through the platform's re-drives, validate asking for one exactly when a subscribe has a step to take, at most one Hetzner action waited per call, and a doubled re-drive making nothing twice |
| `userdata_test.go` | The builder's embedded configuration, its user data within Hetzner's limit, and the gateway's two cloud-config parts |
| `scripts_test.go` | The build, the first-node bootstrap and the cluster's credential calls |
| `teardown_test.go` | Unsubscribe deletes every resource, the snapshot included |
| `declared_resources_test.go`, `secrets_leak_test.go` | The operator kit's block guards and that no token reaches an answer or the logs |
