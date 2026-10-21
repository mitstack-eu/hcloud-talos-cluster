package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mitstack-eu/operator-kit/v2/contract"
)

// clusterState is every Hetzner resource that carries a subscription's labels, sorted by role.
type clusterState struct {
	networks      []hcNetwork
	firewalls     []hcFirewall
	servers       map[role][]hcServer
	loadBalancers map[role][]hcLoadBalancer
	snapshots     []hcSnapshot
}

func listCluster(ctx context.Context, api hetznerAPI, subID string) (clusterState, error) {
	sel := subscriptionSelector(subID)
	st := clusterState{servers: map[role][]hcServer{}, loadBalancers: map[role][]hcLoadBalancer{}}
	servers, err := api.ListServers(ctx, sel)
	if err != nil {
		return st, fmt.Errorf("listing the subscription's servers: %w", err)
	}
	for _, s := range servers {
		st.servers[role(s.Labels[keyRole])] = append(st.servers[role(s.Labels[keyRole])], s)
	}
	lbs, err := api.ListLoadBalancers(ctx, sel)
	if err != nil {
		return st, fmt.Errorf("listing the subscription's load balancers: %w", err)
	}
	for _, lb := range lbs {
		st.loadBalancers[role(lb.Labels[keyRole])] = append(st.loadBalancers[role(lb.Labels[keyRole])], lb)
	}
	if st.networks, err = api.ListNetworks(ctx, sel); err != nil {
		return st, fmt.Errorf("listing the subscription's networks: %w", err)
	}
	if st.firewalls, err = api.ListFirewalls(ctx, sel); err != nil {
		return st, fmt.Errorf("listing the subscription's firewalls: %w", err)
	}
	if st.snapshots, err = api.ListSnapshots(ctx, sel); err != nil {
		return st, fmt.Errorf("listing the subscription's snapshots: %w", err)
	}
	return st, nil
}

func (st clusterState) network() (hcNetwork, bool) {
	if len(st.networks) != 1 {
		return hcNetwork{}, false
	}
	return st.networks[0], true
}

func (st clusterState) firewall() (hcFirewall, bool) {
	if len(st.firewalls) != 1 {
		return hcFirewall{}, false
	}
	return st.firewalls[0], true
}

func (st clusterState) server(r role) (hcServer, bool) {
	if len(st.servers[r]) != 1 {
		return hcServer{}, false
	}
	return st.servers[r][0], true
}

func (st clusterState) loadBalancer(r role) (hcLoadBalancer, bool) {
	if len(st.loadBalancers[r]) != 1 {
		return hcLoadBalancer{}, false
	}
	return st.loadBalancers[r][0], true
}

func (st clusterState) snapshot() (hcSnapshot, bool) {
	if len(st.snapshots) != 1 {
		return hcSnapshot{}, false
	}
	return st.snapshots[0], true
}

func (st clusterState) empty() bool {
	count := len(st.networks) + len(st.firewalls) + len(st.snapshots)
	for _, s := range st.servers {
		count += len(s)
	}
	for _, lb := range st.loadBalancers {
		count += len(lb)
	}
	return count == 0
}

// duplicates names each role more than one resource carries; the block touches nothing while any does.
func (st clusterState) duplicates() []string {
	var out []string
	add := func(what string, ids []int64) {
		if len(ids) > 1 {
			out = append(out, duplicatesDetail(what, ids))
		}
	}
	add("network", idsOf(st.networks, func(n hcNetwork) int64 { return n.ID }))
	add("firewall", idsOf(st.firewalls, func(f hcFirewall) int64 { return f.ID }))
	add("snapshot", idsOf(st.snapshots, func(s hcSnapshot) int64 { return s.ID }))
	for _, r := range []role{roleGateway, roleBuilder, roleNode} {
		add(string(r)+" server", idsOf(st.servers[r], func(s hcServer) int64 { return s.ID }))
	}
	for _, r := range []role{roleAPI, roleIngress} {
		add(string(r)+" load balancer", idsOf(st.loadBalancers[r], func(lb hcLoadBalancer) int64 { return lb.ID }))
	}
	return out
}

func duplicatesDetail(what string, ids []int64) string {
	named := make([]string, len(ids))
	for i, id := range ids {
		named[i] = strconv.FormatInt(id, 10)
	}
	return fmt.Sprintf("%d %ss carry this subscription's labels: %s", len(ids), what, strings.Join(named, ", "))
}

func idsOf[T any](items []T, id func(T) int64) []int64 {
	out := make([]int64, len(items))
	for i, item := range items {
		out[i] = id(item)
	}
	return out
}

// EnsureResources runs Ensure and then reads back, by label, what it converged.
func (r *blockResource) EnsureResources(ctx context.Context, d contract.Desired) ([]contract.CreatedResource, error) {
	if err := r.Ensure(ctx, d); err != nil {
		return nil, err
	}
	st, err := listCluster(ctx, newHetznerAPI(d.Secrets["hetzner"]["api_token"]), d.SubscriptionID)
	if err != nil {
		return nil, err
	}
	return st.createdResources(d.SubscriptionID, repoFor(d.Secrets["github"]["owner"], d.Params["appName"])), nil
}

// createdResources reports each declared resource by its identity, leaving out one not yet made.
func (st clusterState) createdResources(subID string, repo clusterRepo) []contract.CreatedResource {
	var out []contract.CreatedResource
	add := func(name, typ, key string, id int64, ok bool) {
		if ok {
			out = append(out, contract.CreatedResource{Name: name, Type: typ, Identity: map[string]string{key: strconv.FormatInt(id, 10)}})
		}
	}
	network, networkOK := st.network()
	add(resNetwork, resNetwork, "networkId", network.ID, networkOK)
	firewall, firewallOK := st.firewall()
	add(resFirewall, resFirewall, "firewallId", firewall.ID, firewallOK)
	api, apiOK := st.loadBalancer(roleAPI)
	add(resPrivateLB, typeLoadBalancer, "loadBalancerId", api.ID, apiOK)
	ingress, ingressOK := st.loadBalancer(roleIngress)
	add(resPublicLB, typeLoadBalancer, "loadBalancerId", ingress.ID, ingressOK)
	repoIdentity := map[string]string{"repo": repo.fullName()}
	out = append(out,
		contract.CreatedResource{Name: resRepository, Type: typeRepository, Identity: repoIdentity},
		contract.CreatedResource{Name: resSeedCommit, Type: resSeedCommit, Identity: repoIdentity})
	gw, gwOK := st.server(roleGateway)
	add(resGateway, typeServer, "serverId", gw.ID, gwOK)
	node, nodeOK := st.server(roleNode)
	add(resNode, typeServer, "serverId", node.ID, nodeOK)
	return append(out, contract.CreatedResource{Name: resCluster, Type: resCluster, Identity: map[string]string{"subscriptionId": subID}})
}
