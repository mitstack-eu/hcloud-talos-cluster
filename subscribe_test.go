package main

// Subscribe builds a one-node Talos cluster a step a call: gateway and builder, then the snapshot, then the node it boots.

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/mitstack-eu/operator-kit/v3/contract"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscribe_BuildsAOneNodeClusterInEachLocation(t *testing.T) {
	for _, location := range []string{"fsn1", "nbg1", "hel1"} {
		t.Run(location, func(t *testing.T) {
			e := newEnv(t, withLocation(location))

			obs := e.subscribe()

			require.Equal(t, contract.Present, obs.State, "detail: %s", obs.StatusDetail)
			nodes, gateways := e.hetzner.byRole(t, roleNode), e.hetzner.byRole(t, roleGateway)
			require.Len(t, nodes, 1, "nodes")
			require.Len(t, gateways, 1, "gateways")
			assert.Empty(t, e.hetzner.byRole(t, roleBuilder), "a builder outlived its snapshot")
			assert.Equal(t, location, nodes[0].Location, "the node's location")
			assert.Equal(t, location, gateways[0].Location, "the gateway's location")
			for _, lb := range e.hetzner.lbSpecs {
				assert.Equal(t, location, lb.Location, "load balancer %s's location", lb.Name)
			}
			ingress, err := e.hetzner.ListLoadBalancers(e.ctx(), roleSelector(testSub, roleIngress))
			require.NoError(t, err)
			require.Len(t, ingress, 1)
			var byLocation map[string]string
			require.NoError(t, json.Unmarshal([]byte(obs.Outputs["publicIpByLocation"]), &byLocation))
			assert.Equal(t, map[string]string{location: ingress[0].PublicIPv4}, byLocation, "publicIpByLocation")
		})
	}
}

func TestSubscribe_TheNodeBootsFromTheClustersOwnSnapshot(t *testing.T) {
	e := newEnv(t)

	e.subscribe()

	snaps, err := e.hetzner.ListSnapshots(e.ctx(), subscriptionSelector(testSub))
	require.NoError(t, err)
	require.Len(t, snaps, 1, "snapshots")
	node := e.hetzner.byRole(t, roleNode)[0]
	assert.Equal(t, snaps[0].ID, node.ImageID, "node %d boots from image %d; want the snapshot %d", node.ID, node.ImageID, snaps[0].ID)
	assert.Equal(t, string(roleImage), snaps[0].Labels[keyRole], "the snapshot's role label")
}

func TestSubscribe_TheNodeCarriesNoUserDataAndOnlyTheGatewayHoldsAPublicAddress(t *testing.T) {
	e := newEnv(t)

	e.subscribe()

	node := e.hetzner.specFor(t, roleNode)
	assert.Empty(t, node.UserData, "the node's user data")
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

	final := e.engine().settle(nil)

	assert.Equal(t, http.StatusBadRequest, final.status, "the last answer: %s", final.detail)
	assert.Contains(t, final.detail, "image-build-failed")
	assert.Less(t, e.now.Sub(testStart), buildDeadline+5*time.Minute, "how long the subscription waited on a failed build")
	assert.Empty(t, e.hetzner.byRole(t, roleBuilder), "the builder past its deadline")
	assert.Empty(t, e.hetzner.byRole(t, roleNode), "a node with no image")
}

func TestSubscribe_LoadBalancersFindTheNodeByLabel(t *testing.T) {
	e := newEnv(t)

	e.subscribe()

	want := map[string]loadBalancerSpec{
		string(roleAPI): {Services: []lbService{{ListenPort: 6443, DestinationPort: 6443}}},
		string(roleIngress): {PublicInterface: true, Services: []lbService{
			{ListenPort: 80, DestinationPort: 30080, ProxyProtocol: true},
			{ListenPort: 443, DestinationPort: 30081, ProxyProtocol: true},
		}},
	}
	require.Len(t, e.hetzner.lbSpecs, 2)
	for _, r := range []role{roleAPI, roleIngress} {
		lb := e.hetzner.loadBalancer(t, r)
		assert.Equal(t, want[string(r)].Services, lb.Services, "the %s load balancer's services", r)
		assert.Equal(t, []string{roleSelector(testSub, roleNode)}, lb.TargetSelectors, "the %s load balancer's targets", r)
	}
}

func TestSubscribe_TheLoadBalancersJoinTheNetworkAtAddressesApartFromTheGateways(t *testing.T) {
	e := newEnv(t)

	require.NoError(t, e.converge())

	networks, err := e.hetzner.ListNetworks(e.ctx(), subscriptionSelector(testSub))
	require.NoError(t, err)
	require.Len(t, networks, 1)
	for r, want := range map[role]string{roleAPI: "10.0.0.3", roleIngress: "10.0.0.4"} {
		assert.Equal(t, map[int64]string{networks[0].ID: want}, e.hetzner.loadBalancer(t, r).PrivateIPs, "the %s load balancer's address", r)
	}
	assert.Equal(t, gatewayAddress, e.hetzner.byRole(t, roleGateway)[0].PrivateIPs[networks[0].ID], "the gateway's address")
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
		{name: "a location outside the three is refused naming them", opts: []envOption{withLocation("ash")}, want: "fsn1, nbg1 or hel1"},
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

func TestSubscribe_IsUnknownUntilTheNewestNodeReportReadsEveryNodeReady(t *testing.T) {
	e := newEnv(t)
	e.hetzner.instantBuild = true
	require.NoError(t, e.converge())
	e.reports.report(e.now.Add(-time.Minute), true)

	silent := e.observe()
	e.now = e.now.Add(time.Minute)
	e.reports.report(e.now, false)
	notReady := e.observe()
	e.reports.report(e.now.Add(time.Second), true)
	ready := e.observe()

	assert.Equal(t, contract.Unknown, silent.State)
	assert.Contains(t, silent.StatusDetail, "no node report since the node's creation")
	assert.Equal(t, contract.Unknown, notReady.State)
	assert.Contains(t, notReady.StatusDetail, "not Ready")
	assert.Equal(t, contract.Present, ready.State, "detail: %s", ready.StatusDetail)
}

func TestSubscribe_DeletesABuilderLeftBesideTheSnapshotWhoseDeleteFailed(t *testing.T) {
	e := newEnv(t)
	e.hetzner.instantBuild = true
	e.hetzner.failDelete = errors.New("hetzner answered 503 to the builder's delete")
	require.Error(t, e.converge(), "the ensure whose builder delete fails")

	require.NoError(t, e.converge())

	assert.Empty(t, e.hetzner.byRole(t, roleBuilder), "a powered-off builder, whose disk holds every cluster key, outlived its snapshot")
	assert.Len(t, e.hetzner.byRole(t, roleNode), 1, "nodes")
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
	for _, want := range []string{"server-unavailable", "cx23", "cpx22", "nbg1"} {
		assert.Contains(t, err.Error(), want)
	}
	assert.Empty(t, e.hetzner.serverSpecs, "servers created with no gateway type to order")
}
