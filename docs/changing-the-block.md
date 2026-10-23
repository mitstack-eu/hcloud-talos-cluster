# Changing the block

This page is for anyone who changes hcloud-talos-cluster or studies it to
build their own block. It says how to run the tests, what each test proves and
what every file in the repository holds. Words such as block, operator and
manifest are defined in
[the workspace glossary](https://github.com/mitstack-eu/mitstack-workspace/blob/main/docs/glossary.md)
and on [Params, connections and outputs](reference.md#words-used-on-this-page).

## How to run the tests

The tests run every verb against an in-memory Hetzner project, a GitHub fake
and a fake of the platform's node reports, in `harness_test.go`. Its `engine`
plays the platform's settle check, sending signed requests to the block's own
handler. The script tests run each embedded script with bash against fake
commands. Run the suites from the repository root:

```bash
go vet ./...
go test ./...
shellcheck scripts/*.sh
```

| Test file | What it proves |
|---|---|
| `subscribe_test.go` | A node in each of the three locations on one network, one API and one ingress load balancer in `fsn1` at their fixed addresses, the ingress address under `fsn1`, no node with user data, only the gateway with a public address, every refusal, the builder's metrics in the build progress and the `image-build-failed` refusal, the cluster's enrolment outputs while it waits on its nodes' report, its `issuerBucket` and `issuerUrl`, checked against the issuer in the machine configuration the builder bakes into the nodes' image, and a cluster whose gateway lacks the issuer label reading unknown |
| `redrive_test.go` | One subscribe reaching all three nodes through the platform's re-drives, validate asking for one exactly when a subscribe has a step to take, at most one Hetzner action waited and one node created per call, and a doubled re-drive making nothing twice |
| `userdata_test.go` | The builder's embedded configuration with the API load balancer's address in its certificates and the cluster's components, its user data within Hetzner's limit, the gateway's two cloud-config parts, mirror and served manifests, the node report's `checks` member, the object types it read whole with `madeAt` and no right on Secrets, `STATE` and `EPHEMERAL` encrypted under the `nodeID` key and named in the report, and a default cluster's components, mirror and configuration documents against `testdata/default-cluster.golden` |
| `scripts_test.go` | The build, the first-node bootstrap and the cluster's credential calls |
| `teardown_test.go` | Unsubscribe deletes every resource, the snapshot included |
| `declared_resources_test.go`, `secrets_leak_test.go` | The operator kit's block guards and that no token reaches an answer or the logs |

## Files every block starts with

Every new block starts from the same files, which mitstack's scaffold, its
generator of new block repositories, writes. They come from the templates in
the mitstack workspace's `platform/api/internal/scaffold/blockkitv1/`, plus
`go.mod` and `.github/workflows/block-verify.yml`. This block has most of
them, and adds its own files after.

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

## Files this block adds

| File | What it does |
|---|---|
| `README.md` | The front page of the repository |
| `docs/` | These pages, the detail behind the README |
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
