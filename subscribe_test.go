package main

// Subscribe builds a three-node Talos cluster a step a call: the API and ingress load balancers in fsn1, gateway and
// builder, then the snapshot, then one node in each of fsn1, nbg1 and hel1 booted from it.

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mitstack-eu/operator-kit/v3/contract"
	"github.com/mitstack-eu/operator-kit/v3/handler"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscribe_BuildsANodeInEachOfFsn1Nbg1AndHel1OnOneNetworkWhateverLocationIsSent(t *testing.T) {
	cases := []struct {
		name string
		opts []envOption
	}{
		{name: "a subscribe with no location param builds all three"},
		{name: "a subscribe naming hel1 as its location builds all three", opts: []envOption{withLocation("hel1")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.opts...)

			obs := e.subscribe()

			require.Equal(t, contract.Present, obs.State, "detail: %s", obs.StatusDetail)
			networks, err := e.hetzner.ListNetworks(e.ctx(), subscriptionSelector(testSub))
			require.NoError(t, err)
			require.Len(t, networks, 1, "networks")
			require.Len(t, e.hetzner.networkSpecs, 1, "network creates")
			assert.Equal(t, "eu-central", e.hetzner.networkSpecs[0].Subnet.NetworkZone, "the network's zone")
			nodes := e.hetzner.byRole(t, roleNode)
			var locations []string
			for _, node := range nodes {
				locations = append(locations, node.Location)
				assert.NotEmpty(t, node.PrivateIPs[networks[0].ID], "node %s is not on the cluster's network", node.Name)
				assert.True(t, strings.HasSuffix(node.Name, "-node-"+node.Location), "node %s in %s is not named after its location", node.Name, node.Location)
			}
			assert.ElementsMatch(t, clusterLocationNames, locations, "the nodes' locations")
			gateways := e.hetzner.byRole(t, roleGateway)
			require.Len(t, gateways, 1, "gateways")
			assert.Equal(t, "fsn1", gateways[0].Location, "the gateway's location")
			assert.Equal(t, "fsn1", e.hetzner.specFor(t, roleBuilder).Location, "the builder's location")
			assert.Empty(t, e.hetzner.byRole(t, roleBuilder), "a builder outlived its snapshot")
		})
	}
}

func TestSubscribe_BuildsTheBuilderAndANodeInEachLocationOnCpx32WhenItIsSent(t *testing.T) {
	e := newEnv(t, withParam("serverType", "cpx32"))

	obs := e.subscribe()

	require.Equal(t, contract.Present, obs.State, "detail: %s", obs.StatusDetail)
	typeByLocation := map[string]string{}
	for _, node := range e.hetzner.byRole(t, roleNode) {
		typeByLocation[node.Location] = node.ServerType
	}
	assert.Equal(t, map[string]string{"fsn1": "cpx32", "nbg1": "cpx32", "hel1": "cpx32"}, typeByLocation,
		"each location's node server type")
	assert.Equal(t, "cpx32", e.hetzner.specFor(t, roleBuilder).ServerType, "the builder's server type")
}

func TestSubscribe_EveryNodeBootsFromTheClustersOwnSnapshot(t *testing.T) {
	e := newEnv(t)

	e.subscribe()

	snaps, err := e.hetzner.ListSnapshots(e.ctx(), subscriptionSelector(testSub))
	require.NoError(t, err)
	require.Len(t, snaps, 1, "snapshots")
	nodes := e.hetzner.byRole(t, roleNode)
	require.Len(t, nodes, 3, "nodes")
	for _, node := range nodes {
		assert.Equal(t, snaps[0].ID, node.ImageID, "node %d boots from image %d; want the snapshot %d", node.ID, node.ImageID, snaps[0].ID)
	}
	assert.Equal(t, string(roleImage), snaps[0].Labels[keyRole], "the snapshot's role label")
}

func TestSubscribe_NoNodeCarriesUserDataAndOnlyTheGatewayHoldsAPublicAddress(t *testing.T) {
	e := newEnv(t)

	e.subscribe()

	nodes := 0
	for _, spec := range e.hetzner.serverSpecs {
		if spec.Labels[keyRole] == string(roleNode) {
			nodes++
			assert.Empty(t, spec.UserData, "node %s's user data", spec.Name)
		}
	}
	assert.Equal(t, 3, nodes, "nodes created")
	for _, spec := range e.hetzner.serverSpecs {
		wantPublic := spec.Labels[keyRole] == string(roleGateway)
		assert.Equal(t, wantPublic, spec.PublicIPv4, "server %s holds a public IPv4 = %v; want %v", spec.Name, spec.PublicIPv4, wantPublic)
	}
	firewalls, err := e.hetzner.ListFirewalls(e.ctx(), subscriptionSelector(testSub))
	require.NoError(t, err)
	require.Len(t, firewalls, 1)
	assert.Zero(t, firewalls[0].RuleCount, "the firewall's rules")
	for _, spec := range e.hetzner.serverSpecs {
		assert.Equal(t, []int64{firewalls[0].ID}, spec.FirewallIDs, "server %s's firewalls", spec.Name)
	}
	for _, r := range []role{roleAPI, roleIngress} {
		lb := e.hetzner.loadBalancer(t, r)
		wantPublic := r == roleIngress
		assert.Equal(t, wantPublic, lb.PublicInterface, "load balancer %s's public interface = %v; want %v", lb.Name, lb.PublicInterface, wantPublic)
	}
}

func TestSubscribe_FailsOnceTheBuilderStillRunsPastItsDeadline(t *testing.T) {
	e := newEnv(t)
	e.buildTakes = 0
	p := e.engine()
	p.settle(func() bool { return len(e.hetzner.byRole(t, roleBuilder)) == 1 })
	created := e.hetzner.byRole(t, roleBuilder)[0].Created

	final := p.settle(nil)

	assert.Equal(t, http.StatusBadRequest, final.status, "the last answer: %s", final.detail)
	assert.Contains(t, final.detail, "image-build-failed")
	assert.Less(t, e.now.Sub(created), buildDeadline+5*time.Minute, "how long the subscription waited on a failed build after the builder's creation")
	assert.Empty(t, e.hetzner.byRole(t, roleBuilder), "the builder past its deadline")
	assert.Empty(t, e.hetzner.byRole(t, roleNode), "a node with no image")
}

func TestValidate_TheBuildProgressCarriesTheBuildersCPUAndDiskFromItsCreation(t *testing.T) {
	e := newEnv(t)
	p := e.buildingBuilder()
	builder := e.hetzner.builderMetrics(t, map[string][]float64{
		"cpu":                    {5, 120, 118.7},
		"disk.0.bandwidth.read":  {0, 0, 0},
		"disk.0.bandwidth.write": {0, 1 * mib, 2 * mib},
	})
	e.now = builder.Created.Add(4 * time.Minute)

	v := p.post("validate")

	require.Equal(t, http.StatusOK, v.status, "the validate: %s", v.detail)
	assert.True(t, v.pending, "the validate of a running build: %s", v.detail)
	progress := v.observed(resCluster).StatusDetail
	assert.Contains(t, progress, "is building the cluster's image")
	assert.Contains(t, progress, "CPU at 118.7% over the last minute")
	assert.Contains(t, progress, "120.0 MiB written to disk over the last minute")
	assert.Contains(t, progress, "180.0 MiB since its creation")
	reads := e.hetzner.metricsReads
	require.NotEmpty(t, reads, "the validate never read the builder's metrics")
	last := reads[len(reads)-1]
	assert.Equal(t, metricsRead{id: builder.ID, start: builder.Created, end: e.now}, last, "the metrics read")
}

func TestValidate_TheBuildProgressSaysWhenHetznerHoldsNoMetricsYet(t *testing.T) {
	e := newEnv(t)
	p := e.buildingBuilder()

	v := p.post("validate")

	require.Equal(t, http.StatusOK, v.status, "the validate: %s", v.detail)
	assert.Contains(t, v.observed(resCluster).StatusDetail, "Hetzner holds no metrics for the builder yet")
}

func TestValidate_AFailedMetricsReadLeavesTheBuildProgressPendingAndSaysSo(t *testing.T) {
	e := newEnv(t)
	p := e.buildingBuilder()
	e.hetzner.failMetrics = errors.New("hetzner answered 503")

	v := p.post("validate")

	require.Equal(t, http.StatusOK, v.status, "the validate: %s", v.detail)
	assert.True(t, v.pending, "the validate: %s", v.detail)
	progress := v.observed(resCluster).StatusDetail
	assert.Contains(t, progress, "is building the cluster's image")
	assert.Contains(t, progress, "the builder's Hetzner metrics could not be read")
}

func TestSubscribe_TheImageBuildFailedRefusalSaysWhereTheBuildStopped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		series map[string][]float64
		want   func(created time.Time) []string
	}{
		{
			name: "a build that wrote and went quiet",
			series: map[string][]float64{
				"cpu":                    {80, 95, 40, 2},
				"disk.0.bandwidth.read":  {0, 0, 0, 0},
				"disk.0.bandwidth.write": {2 * mib, 0, 0, 0},
			},
			want: func(created time.Time) []string {
				return []string{
					"CPU at 2.0% over the last minute",
					"0 B written to disk over the last minute",
					"120.0 MiB since its creation",
					"its disk last moved at " + created.Add(time.Minute).UTC().Format(time.RFC3339),
					"its CPU was last above 10% at " + created.Add(3*time.Minute).UTC().Format(time.RFC3339),
				}
			},
		},
		{
			name: "a build that never got going",
			series: map[string][]float64{
				"cpu":                    {1, 2},
				"disk.0.bandwidth.read":  {0, 0},
				"disk.0.bandwidth.write": {0, 0},
			},
			want: func(time.Time) []string {
				return []string{"0 B since its creation", "its disk never moved", "its CPU was never above 10%"}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			p := e.buildingBuilder()
			builder := e.hetzner.builderMetrics(t, tc.series)

			final := p.settle(nil)

			require.Equal(t, http.StatusBadRequest, final.status, "the last answer: %s", final.detail)
			assert.Contains(t, final.detail, "image-build-failed")
			for _, want := range tc.want(builder.Created) {
				assert.Contains(t, final.detail, want)
			}
			reads := e.hetzner.metricsReads
			require.NotEmpty(t, reads, "the refusal never read the builder's metrics")
			assert.Equal(t, metricsRead{id: builder.ID, start: builder.Created, end: e.now}, reads[len(reads)-1], "the refusal's metrics read")
			assert.Empty(t, e.hetzner.byRole(t, roleBuilder), "the builder past its deadline")
		})
	}
}

func TestSubscribe_AFailedMetricsReadStillRefusesTheBuildAndSaysSo(t *testing.T) {
	e := newEnv(t)
	p := e.buildingBuilder()
	e.hetzner.failMetrics = errors.New("hetzner answered 503")

	final := p.settle(nil)

	require.Equal(t, http.StatusBadRequest, final.status, "the last answer: %s", final.detail)
	assert.Contains(t, final.detail, "image-build-failed")
	assert.Contains(t, final.detail, "the builder's Hetzner metrics could not be read")
	assert.Empty(t, e.hetzner.byRole(t, roleBuilder), "the builder past its deadline")
}

func TestSubscribe_ServesTheThreeLocationsNodesThroughOneAPIAndOneIngressLoadBalancerInFsn1(t *testing.T) {
	e := newEnv(t)

	obs := e.subscribe()

	require.Equal(t, contract.Present, obs.State, "detail: %s", obs.StatusDetail)
	networks, err := e.hetzner.ListNetworks(e.ctx(), subscriptionSelector(testSub))
	require.NoError(t, err)
	require.Len(t, networks, 1, "networks")
	want := map[role]struct {
		address  string
		services []lbService
	}{
		roleAPI: {address: "10.0.0.3", services: []lbService{{ListenPort: 6443, DestinationPort: 6443}}},
		roleIngress: {address: "10.0.0.4", services: []lbService{
			{ListenPort: 80, DestinationPort: 30080, ProxyProtocol: true},
			{ListenPort: 443, DestinationPort: 30081, ProxyProtocol: true},
		}},
	}
	assert.Len(t, e.hetzner.lbSpecs, 2, "load balancer creates")
	for r, w := range want {
		lb := e.hetzner.loadBalancer(t, r)
		assert.Equal(t, "fsn1", lb.Location, "the %s load balancer's location", r)
		assert.Equal(t, map[int64]string{networks[0].ID: w.address}, lb.PrivateIPs, "the %s load balancer's address", r)
		assert.Equal(t, w.services, lb.Services, "the %s load balancer's services", r)
		assert.Equal(t, []string{roleSelector(testSub, roleNode)}, lb.TargetSelectors, "the %s load balancer's targets", r)
	}
	var nodeLocations []string
	for _, node := range e.hetzner.byRole(t, roleNode) {
		nodeLocations = append(nodeLocations, node.Location)
	}
	assert.ElementsMatch(t, clusterLocationNames, nodeLocations, "the nodes' locations")
	ingress := e.hetzner.loadBalancer(t, roleIngress)
	require.NotEmpty(t, ingress.PublicIPv4, "the ingress load balancer's public address")
	var byLocation map[string]string
	require.NoError(t, json.Unmarshal([]byte(obs.Outputs["publicIpByLocation"]), &byLocation))
	assert.Equal(t, map[string]string{"fsn1": ingress.PublicIPv4}, byLocation, "publicIpByLocation")
	assert.Equal(t, "https://10.0.0.3:6443", obs.Outputs["privateEndpoint"], "the private endpoint is the API load balancer")
}

func TestSubscribe_ALoadBalancerStepThatFailedIsTakenUpByTheNextEnsureWithoutADuplicate(t *testing.T) {
	for _, step := range []string{"attach", "disable-public", "add-service", "add-target"} {
		t.Run(step, func(t *testing.T) {
			e := newEnv(t)
			e.hetzner.failLBStep[step] = errors.New("hetzner answered 503 to the " + step)
			require.Error(t, e.converge(), "the ensure whose %s fails", step)
			assert.Empty(t, e.hetzner.byRole(t, roleGateway), "a gateway built before the API load balancer was finished")

			require.NoError(t, e.converge())

			for _, r := range []role{roleAPI, roleIngress} {
				lb := e.hetzner.loadBalancer(t, r)
				assert.Len(t, lb.PrivateIPs, 1, "the %s load balancer's networks", r)
				assert.Equal(t, r == roleIngress, lb.PublicInterface, "the %s load balancer's public interface", r)
				assert.Len(t, lb.Services, map[role]int{roleAPI: 1, roleIngress: 2}[r], "the %s load balancer's services", r)
				assert.Len(t, lb.TargetSelectors, 1, "the %s load balancer's targets", r)
			}
			assert.Len(t, e.hetzner.byRole(t, roleGateway), 1, "gateways after the retry")
			assert.Len(t, e.hetzner.byRole(t, roleBuilder), 1, "builders after the retry")
		})
	}
}

func TestSubscribe_TheGatewayJoinsTheNetworkAtItsFixedAddressBeforeItBoots(t *testing.T) {
	e := newEnv(t)

	require.NoError(t, e.converge())

	gw := e.hetzner.specFor(t, roleGateway)
	assert.True(t, gw.Stopped, "the gateway is created stopped")
	assert.Empty(t, gw.NetworkIDs, "the gateway is attached after creation, at its address")
	gateways := e.hetzner.byRole(t, roleGateway)
	require.Len(t, gateways, 1)
	networks, err := e.hetzner.ListNetworks(e.ctx(), subscriptionSelector(testSub))
	require.NoError(t, err)
	require.Len(t, networks, 1)
	assert.Equal(t, gatewayAddress, gateways[0].PrivateIPs[networks[0].ID], "the gateway's private address")
	assert.Equal(t, "running", gateways[0].Status, "the gateway's status")
	assert.Equal(t, []hcRoute{{Destination: "0.0.0.0/0", Gateway: gatewayAddress}}, networks[0].Routes, "the network's routes")
}

func TestSubscribe_TheBuilderJoinsTheNetworkOnlyOnceTheGatewayHoldsItsAddress(t *testing.T) {
	e := newEnv(t)

	last := e.engine().settle(e.building)

	require.NoError(t, last.err())
	networks, err := e.hetzner.ListNetworks(e.ctx(), subscriptionSelector(testSub))
	require.NoError(t, err)
	require.Len(t, networks, 1)
	assert.Equal(t, gatewayAddress, e.hetzner.byRole(t, roleGateway)[0].PrivateIPs[networks[0].ID], "the gateway's address")
	builder := e.hetzner.byRole(t, roleBuilder)[0].PrivateIPs[networks[0].ID]
	assert.NotEmpty(t, builder, "the builder runs off the network, with no route to the gateway")
	assert.NotEqual(t, gatewayAddress, builder, "the builder's address")
}

func TestSubscribe_AServerAttachThatLandedButFailedItsCallIsDoneOnTheNextEnsure(t *testing.T) {
	for _, r := range []role{roleGateway, roleBuilder} {
		t.Run(string(r), func(t *testing.T) {
			e := newEnv(t)
			e.hetzner.failAttach[r] = errors.New("context deadline exceeded while waiting for the attach action")
			require.Error(t, e.converge(), "the ensure whose %s attach fails after it landed", r)

			require.NoError(t, e.converge())

			assert.True(t, e.building(), "the builder runs its build")
			gw := e.hetzner.byRole(t, roleGateway)[0]
			assert.Equal(t, []string{gatewayAddress}, slices.Collect(maps.Values(gw.PrivateIPs)), "the gateway's addresses")
		})
	}
}

func TestSubscribe_RefusesBeforeAnyChange(t *testing.T) {
	cases := []struct {
		name  string
		opts  []envOption
		setup func(t *testing.T)
		want  string
	}{
		{name: "a server type the image cannot run on is refused", opts: []envOption{withParam("serverType", "cax21")}, want: `params["serverType"] is "cax21"`},
		{name: "a cluster that hosts the platform is refused", opts: []envOption{withParam("hostsPlatform", "true")}, want: "hostsPlatform"},
		{
			name: "a build against a kit whose tools image is unreleased is refused",
			setup: func(t *testing.T) {
				swap(t, &toolsImage, func() string {
					return "ghcr.io/mitstack-eu/talos-cluster-kit-tools:unreleased@sha256:0000000000000000000000000000000000000000000000000000000000000000"
				})
			},
			want: "tools-image-unreleased",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.opts...)
			if tc.setup != nil {
				tc.setup(t)
			}

			err := e.ensure()

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Zero(t, e.hetzner.count(), "Hetzner resources after the refusal")
			assert.False(t, e.github.exists(e.repo()), "the repository after the refusal")
		})
	}
}

func TestSubscribe_RefusesAServerTypeTheBlockDoesNotListAsAnInvalidParam(t *testing.T) {
	cases := []struct{ name, serverType string }{
		{name: "an x86 type Hetzner sells that the block does not list is refused", serverType: "cpx42"},
		{name: "an Arm type the amd64 image cannot boot is refused", serverType: "cax21"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, withParam("serverType", tc.serverType))

			err := e.ensure()

			var refused *handler.Refused
			require.ErrorAs(t, err, &refused)
			assert.Equal(t, "invalid-param", refused.Class, "serverType %q refused as %q; want invalid-param", tc.serverType, refused.Class)
			assert.Zero(t, e.hetzner.count(), "Hetzner resources after the refusal")
		})
	}
}

func TestSubscribe_CreatesAndMarksTheClusterRepository(t *testing.T) {
	e := newEnv(t)

	e.subscribe()

	assert.ElementsMatch(t, []string{"mitstack-hcloud-talos-cluster", subscriptionRepoTopic(testSub)}, e.github.topics(e.repo()))
	assert.Contains(t, e.github.files, e.repo()+"/contents/"+seedMarkerFile, "the seed")
}

func TestSubscribe_RefusesARepositoryWithoutTheSubscriptionsMark(t *testing.T) {
	e := newEnv(t)
	e.github.repos[e.repo()] = []string{"someone-elses"}

	err := e.ensure()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "repository-not-marked")
	assert.Zero(t, e.hetzner.count(), "Hetzner resources after the refusal")
}

func TestSubscribe_RefusesTwoNodesInOneLocationBeforeAnyChange(t *testing.T) {
	e := newEnv(t)
	e.hetzner.instantBuild = true
	require.NoError(t, e.converge())
	for _, node := range e.hetzner.byRole(t, roleNode) {
		if node.Location == "nbg1" {
			extra := node
			extra.ID = e.hetzner.id()
			e.hetzner.servers[extra.ID] = &extra
		}
	}
	before := e.changes()

	err := e.ensure()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous-cluster")
	assert.Contains(t, err.Error(), "2 nbg1 node servers carry this subscription's labels")
	assert.Equal(t, before, e.changes(), "changes after the refusal")
}

func TestSubscribe_RefusesASecondAPILoadBalancerInAnotherLocationBeforeAnyChange(t *testing.T) {
	e := newEnv(t)
	e.hetzner.instantBuild = true
	require.NoError(t, e.converge())
	extra := e.hetzner.loadBalancer(t, roleAPI)
	extra.ID, extra.Location = e.hetzner.id(), "nbg1"
	e.hetzner.loadBalancers[extra.ID] = &extra
	before := e.changes()

	err := e.ensure()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous-cluster")
	assert.Contains(t, err.Error(), "2 api load balancers carry this subscription's labels")
	assert.Equal(t, before, e.changes(), "changes after the refusal")
}

func TestSubscribe_IsUnknownUntilTheNewestNodeReportReadsEveryNodeReady(t *testing.T) {
	e := newEnv(t)
	e.hetzner.instantBuild = true
	require.NoError(t, e.converge())
	require.Len(t, e.hetzner.byRole(t, roleNode), 3, "nodes")
	e.reports.report(e.now.Add(-time.Minute), true, true, true)

	silent := e.observe()
	e.now = e.now.Add(time.Minute)
	e.reports.report(e.now, true, false, true)
	notReady := e.observe()
	e.reports.report(e.now.Add(time.Second), true, true)
	twoOfThree := e.observe()
	e.reports.report(e.now.Add(2*time.Second), true, true, true)
	ready := e.observe()

	assert.Equal(t, contract.Unknown, silent.State)
	assert.Contains(t, silent.StatusDetail, "no node report since the last node's creation")
	assert.Equal(t, contract.Unknown, notReady.State)
	assert.Contains(t, notReady.StatusDetail, "not Ready")
	assert.Equal(t, contract.Unknown, twoOfThree.State)
	assert.Contains(t, twoOfThree.StatusDetail, "names 2 nodes, and the cluster has 3")
	assert.Equal(t, contract.Present, ready.State, "detail: %s", ready.StatusDetail)
}

func TestSubscribe_DeletesABuilderLeftBesideTheSnapshotWhoseDeleteFailed(t *testing.T) {
	e := newEnv(t)
	e.hetzner.instantBuild = true
	e.hetzner.failDelete = errors.New("hetzner answered 503 to the builder's delete")
	require.Error(t, e.converge(), "the ensure whose builder delete fails")

	require.NoError(t, e.converge())

	assert.Empty(t, e.hetzner.byRole(t, roleBuilder), "a powered-off builder, whose disk holds every cluster key, outlived its snapshot")
	assert.Len(t, e.hetzner.byRole(t, roleNode), 3, "nodes")
}

func TestSubscribe_AnInstallThatFailsAfterTheGatewayExistsLeavesNothingTheNextEnsureRefuses(t *testing.T) {
	e := newEnv(t)
	e.hetzner.failCreate[roleBuilder] = errors.New("hetzner answered 503 to the builder's create")
	require.Error(t, e.converge(), "the ensure whose builder create fails")
	assert.Empty(t, e.hetzner.byRole(t, roleGateway), "the gateway of a failed install")
	assert.Empty(t, e.hetzner.byRole(t, roleBuilder), "the builder of a failed install")

	require.NoError(t, e.converge())

	assert.Len(t, e.hetzner.byRole(t, roleGateway), 1, "gateways after the retry")
	assert.Len(t, e.hetzner.byRole(t, roleBuilder), 1, "builders after the retry")
}

func TestSubscribe_TheGatewayTakesTheFirstTypeTheLocationCanOrder(t *testing.T) {
	cases := []struct {
		name    string
		soldOut []string
		want    string
	}{
		{name: "cx23 when the location can order it", want: "cx23"},
		{name: "cpx22 when cx23 is sold out", soldOut: []string{"cx23"}, want: "cpx22"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.hetzner.soldOut = tc.soldOut

			e.subscribe()

			gateways := e.hetzner.byRole(t, roleGateway)
			require.Len(t, gateways, 1, "gateways")
			assert.Equal(t, tc.want, gateways[0].ServerType, "the gateway's server type")
		})
	}
}

func TestSubscribe_RefusesBeforeAnyServerWhenNoGatewayTypeCanBeOrdered(t *testing.T) {
	e := newEnv(t)
	e.hetzner.soldOut = []string{"cx23", "cpx22"}

	err := e.converge()

	require.Error(t, err)
	for _, want := range []string{"server-unavailable", "cx23", "cpx22", "fsn1"} {
		assert.Contains(t, err.Error(), want)
	}
	assert.Empty(t, e.hetzner.serverSpecs, "servers created with no gateway type to order")
}

func TestSubscribe_TheClusterCarriesTheEnrolmentDigestAndTheClusterRepositoryWhileItWaitsOnItsNodeReport(t *testing.T) {
	e := newEnv(t)
	e.quietNode = true
	p := e.engine()

	first := p.post("subscribe")
	p.settle(func() bool { return strings.HasPrefix(e.observe().StatusDetail, "the cluster has sent no node report") })
	waiting := p.post("subscribe")
	nodesWhileWaiting := len(e.hetzner.byRole(t, roleNode))
	e.quietNode = false
	final := p.settle(nil)

	gateway := e.hetzner.byRole(t, roleGateway)[0]
	require.NotEmpty(t, gateway.Labels[keyEnrolmentSHA256], "the gateway's enrolment digest label")
	want := map[string]string{
		"enrolmentTokenSha256": gateway.Labels[keyEnrolmentSHA256],
		"clusterRepoUrl":       "https://github.com/" + e.repo(),
	}
	assert.Nil(t, first.observed(resCluster), "the first subscribe, before the node exists, observed the cluster")
	require.True(t, waiting.pending, "a subscribe before the node reports: %s", waiting.detail)
	assert.Equal(t, 3, nodesWhileWaiting, "nodes while the cluster waits on their report")
	cluster := waiting.observed(resCluster)
	require.NotNil(t, cluster, "the subscribe after the node exists did not observe the cluster")
	assert.Equal(t, contract.Unknown, cluster.State, "the cluster's state")
	assert.Contains(t, cluster.StatusDetail, "no node report", "the cluster's detail")
	assert.Equal(t, want, cluster.Outputs, "the cluster's outputs while it waits on its node report")
	require.NotNil(t, waiting.observed(resGateway), "the subscribe after the node exists did not observe the gateway")
	assert.Equal(t, contract.Present, waiting.observed(resGateway).State, "the gateway's state")
	assert.Empty(t, waiting.observed(resGateway).Outputs, "the gateway's outputs while the cluster waits on its node report")
	require.False(t, final.pending, "the last answer: %s", final.detail)
	assert.Empty(t, final.observed(resGateway).Outputs, "the gateway's outputs once the cluster is present")
	for name, value := range want {
		assert.Equal(t, value, final.observed(resCluster).Outputs[name], "the cluster's output %s once it is present", name)
		assert.Equal(t, value, final.outputs[name], "the cluster's provided output %s", name)
	}
}

func TestSubscribe_TheClusterOutputsTheIssuerItsNodesSignTokensForInAnIssuerBucketOfItsOwn(t *testing.T) {
	e := newEnv(t)

	obs := e.subscribe()

	require.Equal(t, contract.Present, obs.State, "detail: %s", obs.StatusDetail)
	bucket := obs.Outputs["issuerBucket"]
	assert.Regexp(t, `^mitstack-oidc-[a-z2-7]{26}$`, bucket, "issuerBucket")
	assert.Equal(t, "https://"+bucket+".s3.eu-central-1.amazonaws.com/"+testSub, obs.Outputs["issuerUrl"], "issuerUrl")
	config := decodeCloudConfig(t, e.hetzner.specFor(t, roleBuilder).UserData).file(t, builderConfigPath)
	accounts := testutil.Kind(testutil.Documents(t, []byte(config.Content)), "KubeServiceAccountConfig")
	require.Len(t, accounts, 1, "the builder's machine configuration")
	assert.Equal(t, obs.Outputs["issuerUrl"], testutil.Field(t, accounts[0], "issuer", "issuerURL"),
		"the issuer every node signs service-account tokens for")
}

func TestObserve_APresentClusterWhoseGatewayCarriesNoIssuerBucketOutputsNoIssuer(t *testing.T) {
	e := newEnv(t)
	require.Equal(t, contract.Present, e.subscribe().State, "the subscribe")
	gateway := e.hetzner.byRole(t, roleGateway)[0]
	delete(e.hetzner.servers[gateway.ID].Labels, keyIssuerBucket)

	obs := e.observe()

	require.Equal(t, contract.Present, obs.State, "detail: %s", obs.StatusDetail)
	assert.NotContains(t, obs.Outputs, "issuerBucket", "the outputs of a cluster whose gateway names no issuer bucket")
	assert.NotContains(t, obs.Outputs, "issuerUrl", "the outputs of a cluster whose gateway names no issuer bucket")
}

func TestObserve_AWaitingClusterWhoseRepositoryCannotBeNamedCarriesNeitherOutputAndSaysWhy(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.converge())
	require.Len(t, e.hetzner.byRole(t, roleGateway), 1, "gateways")
	d := e.desired()
	delete(d.Secrets, "github")

	obs, err := observers()[resCluster].ObserveResource(e.ctx(), contract.ResourceRef{Desired: d})

	require.NoError(t, err)
	assert.Equal(t, contract.Unknown, obs.State, "detail: %s", obs.StatusDetail)
	assert.Empty(t, obs.Outputs, "the cluster's outputs")
	assert.Contains(t, obs.StatusDetail, "secrets.github.token is empty")
}

func TestObserve_AWaitingClusterWhoseGatewayLacksItsDigestLabelCarriesNeitherOutputAndSaysWhy(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.converge())
	gateway := e.hetzner.byRole(t, roleGateway)[0]
	delete(e.hetzner.servers[gateway.ID].Labels, keyEnrolmentSHA256)

	obs, err := observers()[resCluster].ObserveResource(e.ctx(), contract.ResourceRef{Desired: e.desired()})

	require.NoError(t, err)
	assert.Equal(t, contract.Unknown, obs.State, "detail: %s", obs.StatusDetail)
	assert.Empty(t, obs.Outputs, "the cluster's outputs")
	assert.Contains(t, obs.StatusDetail, "no "+keyEnrolmentSHA256+" label")
}

func TestObserve_AWaitingClusterWithoutItsRepositoryCarriesNeitherOutput(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.converge())
	e.github.remove(e.repo())

	obs, err := observers()[resCluster].ObserveResource(e.ctx(), contract.ResourceRef{Desired: e.desired()})

	require.NoError(t, err)
	assert.Equal(t, contract.Unknown, obs.State, "detail: %s", obs.StatusDetail)
	assert.Empty(t, obs.Outputs, "the cluster's outputs")
}
