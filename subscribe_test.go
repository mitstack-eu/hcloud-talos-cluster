package main

// Subscribe builds a one-node Talos cluster: gateway and builder, then the snapshot, then the node that boots from it.

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mitstack-eu/operator-kit/v2/contract"
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
	for _, lb := range e.hetzner.lbSpecs {
		wantPublic := lb.Labels[keyRole] == string(roleIngress)
		assert.Equal(t, wantPublic, lb.PublicInterface, "load balancer %s's public interface = %v; want %v", lb.Name, lb.PublicInterface, wantPublic)
	}
}

func TestSubscribe_DeletesTheBuilderWhoseUserDataHeldEveryKeyOnceItsDiskIsASnapshot(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.ensure())
	require.Len(t, e.hetzner.byRole(t, roleBuilder), 1, "builders after the first ensure")

	e.hetzner.powerOffBuilder(t)
	require.NoError(t, e.ensure())

	assert.Empty(t, e.hetzner.byRole(t, roleBuilder), "builders once the snapshot exists")
	assert.Len(t, e.hetzner.byRole(t, roleNode), 1, "nodes once the snapshot exists")
}

func TestSubscribe_WaitsForTheBuilderAndRefusesOnePastItsDeadline(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.ensure())

	e.now = e.now.Add(buildDeadline - time.Minute)
	require.NoError(t, e.ensure(), "an ensure within the deadline")
	waiting := e.observe()
	e.now = e.now.Add(2 * time.Minute)
	err := e.ensure()

	assert.Equal(t, contract.Unknown, waiting.State)
	assert.Contains(t, waiting.StatusDetail, "is building the cluster's image")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image-build-failed")
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
	for _, lb := range e.hetzner.lbSpecs {
		r := lb.Labels[keyRole]
		assert.Equal(t, want[r].Services, lb.Services, "the %s load balancer's services", r)
		assert.Equal(t, roleSelector(testSub, roleNode), lb.TargetSelector, "the %s load balancer's targets", r)
		assert.NotZero(t, lb.NetworkID, "the %s load balancer's network", r)
	}
}

func TestSubscribe_TheGatewayJoinsTheNetworkAtItsFixedAddressBeforeItBoots(t *testing.T) {
	e := newEnv(t)

	require.NoError(t, e.ensure())

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
	require.NoError(t, e.ensure())
	e.hetzner.powerOffBuilder(t)
	require.NoError(t, e.ensure())
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

func TestSubscribe_ConvergesWithoutDuplicatingOnceTheClusterIsPresent(t *testing.T) {
	e := newEnv(t)
	e.subscribe()
	before := e.hetzner.count()

	require.NoError(t, e.ensure())

	assert.Equal(t, before, e.hetzner.count(), "Hetzner resources after a repeated ensure")
	assert.Equal(t, contract.Present, e.observe().State)
}

func TestSubscribe_DeletesABuilderLeftBesideTheSnapshotWhoseDeleteFailed(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.ensure())
	e.hetzner.powerOffBuilder(t)
	e.hetzner.failDelete = errors.New("hetzner answered 503 to the builder's delete")
	require.Error(t, e.ensure(), "the ensure whose builder delete fails")

	require.NoError(t, e.ensure())

	assert.Empty(t, e.hetzner.byRole(t, roleBuilder), "a powered-off builder, whose disk holds every cluster key, outlived its snapshot")
	assert.Len(t, e.hetzner.byRole(t, roleNode), 1, "nodes")
}

func TestSubscribe_AnInstallThatFailsAfterTheGatewayExistsLeavesNothingTheNextEnsureRefuses(t *testing.T) {
	e := newEnv(t)
	e.hetzner.failPowerOn = errors.New("hetzner answered 503 to the gateway's power-on")
	require.Error(t, e.ensure(), "the ensure whose gateway power-on fails")
	assert.Empty(t, e.hetzner.byRole(t, roleGateway), "the gateway of a failed install")
	assert.Empty(t, e.hetzner.byRole(t, roleBuilder), "the builder of a failed install")

	require.NoError(t, e.ensure())

	assert.Len(t, e.hetzner.byRole(t, roleGateway), 1, "gateways after the retry")
	assert.Len(t, e.hetzner.byRole(t, roleBuilder), 1, "builders after the retry")
}

func TestSubscribe_ReadsTheAPIAddressFromHetznerWhenTheCreateAnswerLacksIt(t *testing.T) {
	e := newEnv(t)
	e.hetzner.lateLBAddress = true

	obs := e.subscribe()

	assert.Equal(t, contract.Present, obs.State, "detail: %s", obs.StatusDetail)
}
