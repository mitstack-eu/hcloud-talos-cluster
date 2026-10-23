# Params, connections and outputs

This page lists everything hcloud-talos-cluster takes and hands back: its
params, its connections, its outputs and the settings its host gives it. It is
for anyone subscribing an app to the block or wiring another block to it, and
it assumes no earlier knowledge of mitstack.

## Words used on this page

Each word is defined in full in
[the workspace glossary](https://github.com/mitstack-eu/mitstack-workspace/blob/main/docs/glossary.md).

| Word | What it means here |
|---|---|
| Block | A ready-made service that mitstack runs for you, such as a database or, here, a Kubernetes cluster |
| App | Your project in mitstack, which uses one or more blocks |
| Subscribe | What an app does to start using a block. Unsubscribe is how it stops |
| Operator | The program in this repository, which runs as a web service. mitstack sends it a request, signed with mitstack's own key, each time an app subscribes to the block, checks on it or unsubscribes |
| Param | A setting you fill in when your app subscribes, such as the app's name |
| Connection | An account your org has connected in mitstack, on the console's Capabilities page, such as a Hetzner Cloud project or a GitHub account. It belongs to the org, not to one app. mitstack passes the account's token to the block with each request, and the block acts for you with it |
| Resource | One thing the block creates and tracks, such as a server, a load balancer or the cluster repository |
| Output | A value the block hands back once it has built something, such as the cluster's address, which mitstack records for the app |
| Manifest | The file `manifest.yaml`, which tells mitstack what the block is, what it builds and which requests it answers |
| Type | The catalogue name of one kind of resource. mitstack matches a block that requires a type with a block that provides it |

## What the block provides

The block provides the type `hetzner.kubernetes-cluster`, which the `provides`
list in `manifest.yaml` declares. Kubernetes is the common system for running
containers across several servers.

The cluster belongs to one app alone. It has three servers, called nodes, one
in each of Hetzner's three European locations: `fsn1` in Falkenstein, `nbg1`
in Nuremberg and `hel1` in Helsinki.

The block also creates a private GitHub repository for the cluster, named
`<appName>-cluster`. Argo CD, a tool that runs inside the cluster, applies
what that repository holds to the cluster. A commit to the repository's
`cluster/` folder therefore changes what runs on the cluster.

## Outputs

`observe.go` sets these outputs.

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
missing.

In a subscribe the operator kit observes the resources in manifest order and
stops at the first one not present, so it reaches the cluster resource once
all three nodes exist. A validate observes every resource.
`clusterState.enrolment` in `observe.go` makes the outputs of a cluster not yet
present, and `clusterState.present` makes those of a present one.

## Params

The block reads three params: two you set, `appName` and `serverType`, and
`hostsPlatform`, which you leave unset. Its manifest lists no `requires`, so
no other block has to be in place first.

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

## Connections

The connections give the block its tokens, and subscribe needs both. Validate
is the read-only check mitstack sends while a subscription is being built. The
manifest's `verbConnections` names both connections for validate and
unsubscribe, so mitstack sends their tokens on those requests as well.

| Connection | What the block reads from it | What the block does with it | When it is missing |
|---|---|---|---|
| Hetzner | `api_token`, a Hetzner Cloud project's API token, read by `hetznerToken` in `resource.go` | Creates and deletes every server, load balancer, network, firewall and snapshot in that project, which Hetzner bills | Refused with `missing-hetzner-access` |
| GitHub | `token`, `owner` and the GitHub API address mitstack sends with them, read by `dialGitHub` in `repository.go` | Creates, reads and deletes the cluster repository under `owner` | Refused with `missing-github-access` |

## Host settings

The operator reads its settings from the environment its host gives it, in
`main.go` and `repository.go`.

| Variable | Required | What it sets |
|---|---|---|
| `MITSTACK_JWKS_URL` | Yes. The operator exits at start when it is unset | The address where mitstack publishes its signing keys, which the operator checks each request against |
| `LISTEN_ADDR` | No, `:8080` when unset | The address the operator's web service listens on |
| `MITSTACK_GITHUB_API_HOSTS` | No | A comma-separated list of GitHub API hosts the operator accepts beside `api.github.com` |
