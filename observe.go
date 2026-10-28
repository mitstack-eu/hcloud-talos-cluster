package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mitstack-eu/operator-kit/v3/contract"
	"github.com/mitstack-eu/operator-kit/v3/handler"
)

// observers judge every declared resource by the labels and topics it carries, never by a recorded identity,
// so an unsubscribe of a subscription the engine never recorded still settles from what the provider holds.
func observers() map[string]handler.Observer {
	out := map[string]handler.Observer{
		resNetwork:    labelledObserver{kind: "network", list: listNetworks},
		resFirewall:   labelledObserver{kind: "firewall", list: listFirewalls},
		resGateway:    labelledObserver{kind: "gateway", list: listGateways},
		resRepository: repositoryObserver{},
		resSeedCommit: seedCommitObserver{},
		resCluster:    clusterObserver{},
		located(resPrivateLB, gatewayLocation): labelledObserver{
			kind: gatewayLocation + " API load balancer", list: listLoadBalancersIn(roleAPI, gatewayLocation),
		},
		located(resPublicLB, gatewayLocation): labelledObserver{
			kind: gatewayLocation + " ingress load balancer", list: listLoadBalancersIn(roleIngress, gatewayLocation),
		},
	}
	for _, loc := range clusterLocations {
		out[located(resNode, loc)] = labelledObserver{kind: loc + " node", list: listNodesIn(loc)}
	}
	return out
}

type labelledLister func(ctx context.Context, api hetznerAPI, subID string) ([]int64, error)

// labelledObserver reads one kind: present when exactly one resource carries the labels, absent when none does.
type labelledObserver struct {
	kind string
	list labelledLister
}

func (o labelledObserver) ObserveResource(ctx context.Context, r contract.ResourceRef) (contract.Observation, error) {
	api, refused, ok := hetznerReader(r.Desired)
	if !ok {
		return refused, nil
	}
	ids, err := o.list(ctx, api, r.Desired.SubscriptionID)
	if err != nil {
		return contract.Observation{}, fmt.Errorf("listing the subscription's %ss: %w", o.kind, err)
	}
	switch len(ids) {
	case 0:
		return contract.Observation{State: contract.Absent, StatusDetail: "no " + o.kind + " carries this subscription's labels"}, nil
	case 1:
		return contract.Observation{State: contract.Present, StatusDetail: fmt.Sprintf("%s %d carries this subscription's labels", o.kind, ids[0])}, nil
	default:
		return unknown(duplicatesDetail(o.kind, ids)), nil
	}
}

func enrolmentOutputs(digest, repoURL string) map[string]string {
	return map[string]string{"enrolmentTokenSha256": digest, "clusterRepoUrl": repoURL}
}

func listNetworks(ctx context.Context, api hetznerAPI, subID string) ([]int64, error) {
	networks, err := api.ListNetworks(ctx, subscriptionSelector(subID))
	return idsOf(networks, func(n hcNetwork) int64 { return n.ID }), err
}

func listFirewalls(ctx context.Context, api hetznerAPI, subID string) ([]int64, error) {
	firewalls, err := api.ListFirewalls(ctx, subscriptionSelector(subID))
	return idsOf(firewalls, func(f hcFirewall) int64 { return f.ID }), err
}

func listLoadBalancersIn(r role, loc string) labelledLister {
	return func(ctx context.Context, api hetznerAPI, subID string) ([]int64, error) {
		lbs, err := api.ListLoadBalancers(ctx, roleSelector(subID, r))
		return idsOf(loadBalancersIn(lbs, loc), loadBalancerID), err
	}
}

func listGateways(ctx context.Context, api hetznerAPI, subID string) ([]int64, error) {
	gateways, err := api.ListServers(ctx, roleSelector(subID, roleGateway))
	return idsOf(gateways, serverID), err
}

func listNodesIn(loc string) labelledLister {
	return func(ctx context.Context, api hetznerAPI, subID string) ([]int64, error) {
		servers, err := api.ListServers(ctx, roleSelector(subID, roleNode))
		return idsOf(serversIn(servers, loc), serverID), err
	}
}

// repositoryObserver reads the cluster repository from GitHub alone; one without the subscription's mark is not the cluster's.
type repositoryObserver struct{}

func (repositoryObserver) ObserveResource(ctx context.Context, r contract.ResourceRef) (contract.Observation, error) {
	repo, err := observeRepo(ctx, r.Desired)
	if err != nil {
		return contract.Observation{}, err
	}
	switch repo.state {
	case repoPresent:
		return contract.Observation{State: contract.Present, Outputs: map[string]string{"repo": repo.name}, StatusDetail: repo.describe()}, nil
	case repoGone, repoUnmarked:
		return contract.Observation{State: contract.Absent, StatusDetail: repo.describe()}, nil
	default:
		return unknown(repo.describe()), nil
	}
}

// seedCommitObserver reads the seed present while the marked cluster repository holds it.
type seedCommitObserver struct{}

func (seedCommitObserver) ObserveResource(ctx context.Context, r contract.ResourceRef) (contract.Observation, error) {
	repo, err := observeRepo(ctx, r.Desired)
	if err != nil {
		return contract.Observation{}, err
	}
	switch repo.state {
	case repoGone, repoUnmarked:
		return contract.Observation{State: contract.Absent, StatusDetail: repo.describe()}, nil
	case repoUnknown:
		return unknown(repo.describe()), nil
	}
	held, err := repo.gh.holdsSeed(ctx)
	if err != nil {
		return contract.Observation{}, err
	}
	if !held {
		return contract.Observation{State: contract.Absent, StatusDetail: "the cluster repository " + repo.name + " holds no " + seedMarkerFile}, nil
	}
	return contract.Observation{State: contract.Present, StatusDetail: "the cluster repository " + repo.name + " holds the seed"}, nil
}

type clusterObserver struct{}

func (clusterObserver) ObserveResource(ctx context.Context, r contract.ResourceRef) (contract.Observation, error) {
	return observeCluster(ctx, r.Desired)
}

// hetznerReader answers Unknown, before any call, for a request the subscription's resources cannot be read with.
func hetznerReader(d contract.Desired) (hetznerAPI, contract.Observation, bool) {
	token := d.Secrets["hetzner"]["api_token"]
	if token == "" {
		return nil, unknown("no Hetzner token on this call (secrets.hetzner.api_token is empty), so the cluster cannot be read"), false
	}
	if !validSubscriptionID(d.SubscriptionID) {
		return nil, unknown("the subscription id is not a valid Hetzner label value, so the cluster's resources cannot be found by it"), false
	}
	return newHetznerAPI(token), contract.Observation{}, true
}

// Observe reads the whole cluster: present once every resource is as the block makes it and the newest node report
// since the last node's creation reads every node Ready; absent when no labelled resource or marked repository is left;
// unknown, naming what is missing, in between.
func (r *blockResource) Observe(ctx context.Context, d contract.Desired) (contract.Observation, error) {
	return observeCluster(ctx, d)
}

func observeCluster(ctx context.Context, d contract.Desired) (contract.Observation, error) {
	api, refused, ok := hetznerReader(d)
	if !ok {
		return refused, nil
	}
	st, err := listCluster(ctx, api, d.SubscriptionID)
	if err != nil {
		return contract.Observation{}, err
	}
	repo, err := observeRepo(ctx, d)
	if err != nil {
		return contract.Observation{}, err
	}
	if st.empty() && (repo.state == repoGone || repo.state == repoUnmarked) {
		return contract.Observation{
			State:        contract.Absent,
			StatusDetail: "no server, load balancer, firewall, network or snapshot carries this subscription's labels, and " + repo.describe(),
		}, nil
	}
	problems := st.problems(ctx, api)
	if repo.state != repoPresent {
		problems = append(problems, repo.describe())
	}
	if len(problems) == 0 {
		if problem := reportProblem(ctx, d, st.servers[roleNode]); problem != "" {
			problems = append(problems, problem)
		}
	}
	if len(problems) > 0 {
		outputs, gap := st.enrolment(repo)
		if gap != "" {
			problems = append(problems, gap)
		}
		obs := unknown(strings.Join(problems, "; "))
		obs.Outputs = outputs
		obs.Redrive, err = stepLeft(ctx, d, api, st, repo)
		return obs, err
	}
	return st.present(d.SubscriptionID, repo.url), nil
}

// stepLeft reports whether a subscribe sent now would take a step or refuse, read from what Hetzner and GitHub hold, so
// the engine re-drives subscribe once the step in flight has finished, and never while it runs. A request the subscribe
// refuses before reading the cluster asks for none, since its own subscribe was refused already.
func stepLeft(ctx context.Context, d contract.Desired, api hetznerAPI, st clusterState, repo repoObservation) (bool, error) {
	req, err := readEnsureRequest(d)
	if err != nil || repo.state == repoUnknown {
		return false, nil
	}
	c := convergence{api: api, state: st, req: req, repo: repoRead{exists: repo.state != repoGone, marked: repo.state == repoPresent}}
	network, ok := st.network()
	if c.refusal() != nil || !ok || !c.repo.exists {
		return true, nil
	}
	_, ok, err = nextStep(ctx, c, network)
	var refused *handler.Refused
	if errors.As(err, &refused) {
		return true, nil
	}
	return ok, err
}

// enrolment gives a cluster not yet present the values a node's enrolment is judged against, once the gateway carries
// its digest and the marked repository exists, so the platform holds them before any node can report.
func (st clusterState) enrolment(repo repoObservation) (map[string]string, string) {
	gw, ok := st.server(roleGateway)
	switch {
	case !ok || repo.state != repoPresent:
		return nil, ""
	case gw.Labels[keyEnrolmentSHA256] == "":
		return nil, fmt.Sprintf("gateway %d has no %s label, so the cluster carries no enrolment outputs", gw.ID, keyEnrolmentSHA256)
	}
	return enrolmentOutputs(gw.Labels[keyEnrolmentSHA256], repo.url), ""
}

func (st clusterState) present(subID, repoURL string) contract.Observation {
	network, _ := st.network()
	gw, _ := st.server(roleGateway)
	endpoint, _ := st.loadBalancerIn(roleAPI, gatewayLocation)
	ingress, _ := st.loadBalancerIn(roleIngress, gatewayLocation)
	var nodes []string
	for _, loc := range clusterLocations {
		node, _ := st.nodeIn(loc)
		nodes = append(nodes, fmt.Sprintf("%d in %s at %s", node.ID, loc, node.PrivateIPs[network.ID]))
	}
	byLocation, _ := json.Marshal(map[string]string{gatewayLocation: ingress.PublicIPv4})
	outputs := enrolmentOutputs(gw.Labels[keyEnrolmentSHA256], repoURL)
	outputs["privateEndpoint"] = "https://" + endpoint.PrivateIPs[network.ID] + ":" + strconv.Itoa(apiServerPort)
	outputs["publicIpByLocation"] = string(byLocation)
	outputs["subscriptionId"] = subID
	outputs["issuerBucket"] = gw.Labels[keyIssuerBucket]
	outputs["issuerUrl"] = issuerURL(gw.Labels[keyIssuerBucket], subID).String()
	return contract.Observation{
		State:   contract.Present,
		Outputs: outputs,
		StatusDetail: fmt.Sprintf("nodes %s are on network %d behind gateway %d, an API and an ingress load balancer in %s reach them, and the newest node report reads every node Ready",
			strings.Join(nodes, ", "), network.ID, gw.ID, gatewayLocation),
	}
}

// problems names every way the Hetzner half of the cluster falls short of present.
func (st clusterState) problems(ctx context.Context, api hetznerAPI) []string {
	missing := st.duplicates()
	if len(missing) > 0 {
		return missing
	}
	network, networkOK := st.network()
	firewall, firewallOK := st.firewall()
	if !networkOK {
		missing = append(missing, "no network carries this subscription's labels")
	}
	if !firewallOK {
		missing = append(missing, "no firewall carries this subscription's labels")
	}
	if builder, ok := st.server(roleBuilder); ok {
		missing = append(missing, builder.buildDetail(ctx, api))
	}
	if snap, ok := st.snapshot(); !ok {
		missing = append(missing, "no snapshot of the cluster's image carries this subscription's labels")
	} else if snap.Status != snapshotAvailable {
		missing = append(missing, fmt.Sprintf("snapshot %d of the cluster's image is %s", snap.ID, snap.Status))
	}
	if firewallOK && firewall.RuleCount > 0 {
		missing = append(missing, fmt.Sprintf("firewall %d holds %d rules, and the cluster's firewall holds none", firewall.ID, firewall.RuleCount))
	}
	if !networkOK || !firewallOK {
		return missing
	}
	missing = append(missing, st.gatewayProblems(network, firewall)...)
	missing = append(missing, st.nodeProblems(network, firewall)...)
	missing = append(missing, st.loadBalancerProblems(network)...)
	return missing
}

// buildDetail says where the builder stands, judged against buildDeadline as builderStep judges it.
func (b hcServer) buildDetail(ctx context.Context, api hetznerAPI) string {
	created := b.Created.UTC().Format(time.RFC3339)
	if b.Status != serverRunning {
		return fmt.Sprintf("builder %d, created at %s, is %s", b.ID, created, b.Status)
	}
	state := "is building the cluster's image or waiting for the gateway's mirror to complete"
	if clock().Sub(b.Created) >= buildDeadline {
		state = fmt.Sprintf("did not power itself off within %s", buildDeadline)
	}
	return fmt.Sprintf("builder %d, created at %s, %s; %s", b.ID, created, state, readBuilderMetrics(ctx, api, b).summary())
}

func (st clusterState) gatewayProblems(network hcNetwork, firewall hcFirewall) []string {
	gw, ok := st.server(roleGateway)
	if !ok {
		return []string{"no gateway carries this subscription's labels"}
	}
	var missing []string
	if gw.Status != serverRunning {
		missing = append(missing, fmt.Sprintf("gateway %d is %s", gw.ID, gw.Status))
	}
	if gw.PrivateIPs[network.ID] != gatewayAddress {
		missing = append(missing, fmt.Sprintf("gateway %d is not on network %d at %s", gw.ID, network.ID, gatewayAddress))
	}
	if gw.PublicIPv4 == "" {
		missing = append(missing, fmt.Sprintf("gateway %d has no public address", gw.ID))
	}
	if !slices.Contains(gw.FirewallIDs, firewall.ID) {
		missing = append(missing, fmt.Sprintf("firewall %d is not applied to gateway %d", firewall.ID, gw.ID))
	}
	if gw.Labels[keyIssuerBucket] == "" {
		missing = append(missing, fmt.Sprintf("gateway %d has no %s label, so the block cannot read the issuer its nodes sign for", gw.ID, keyIssuerBucket))
	}
	return missing
}

func (st clusterState) nodeProblems(network hcNetwork, firewall hcFirewall) []string {
	var missing []string
	for _, loc := range clusterLocations {
		node, ok := st.nodeIn(loc)
		if !ok {
			missing = append(missing, "no node in "+loc+" carries this subscription's labels")
			continue
		}
		if node.PrivateIPs[network.ID] == "" {
			missing = append(missing, fmt.Sprintf("node %d has no address on network %d", node.ID, network.ID))
		}
		if node.PublicIPv4 != "" {
			missing = append(missing, fmt.Sprintf("node %d holds the public address %s, and only the gateway may", node.ID, node.PublicIPv4))
		}
		if !slices.Contains(node.FirewallIDs, firewall.ID) {
			missing = append(missing, fmt.Sprintf("firewall %d is not applied to node %d", firewall.ID, node.ID))
		}
	}
	return missing
}

func (st clusterState) loadBalancerProblems(network hcNetwork) []string {
	var missing []string
	for _, lb := range []struct {
		role   role
		public bool
	}{{roleAPI, false}, {roleIngress, true}} {
		found, ok := st.loadBalancerIn(lb.role, gatewayLocation)
		if !ok {
			missing = append(missing, "no "+string(lb.role)+" load balancer in "+gatewayLocation+" carries this subscription's labels")
			continue
		}
		missing = append(missing, found.reachProblems(network, lb.public)...)
	}
	return missing
}

func (lb hcLoadBalancer) reachProblems(network hcNetwork, public bool) []string {
	var missing []string
	if lb.PrivateIPs[network.ID] == "" {
		missing = append(missing, fmt.Sprintf("load balancer %d is not on network %d", lb.ID, network.ID))
	}
	if lb.PublicInterface != public {
		missing = append(missing, fmt.Sprintf("load balancer %d's public interface is %s, want %s", lb.ID, onOff(lb.PublicInterface), onOff(public)))
	}
	if !slices.Contains(lb.TargetSelectors, roleSelector(lb.Labels[keySubscription], roleNode)) {
		missing = append(missing, fmt.Sprintf("load balancer %d does not target the subscription's nodes by label", lb.ID))
	}
	return missing
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

type repoState int

const (
	repoUnknown repoState = iota
	repoPresent
	repoUnmarked
	repoGone
)

// repoObservation is what reading the cluster repository found; detail says why its state is unknown.
type repoObservation struct {
	state     repoState
	name, url string
	detail    string
	gh        *githubAccess
}

func (o repoObservation) describe() string {
	switch o.state {
	case repoPresent:
		return "the cluster repository " + o.name + " exists"
	case repoUnmarked:
		return "the repository " + o.name + " does not carry this subscription's mark, so it is not the cluster's"
	case repoGone:
		return "the cluster repository " + o.name + " does not exist"
	default:
		return o.detail
	}
}

// observeRepo leaves the repository unknown, with no GitHub call, when the request cannot name it or reach GitHub.
func observeRepo(ctx context.Context, d contract.Desired) (repoObservation, error) {
	appName := d.Params["appName"]
	if appName == "" {
		return repoObservation{name: "(unnamed)", detail: `the cluster repository cannot be named: params["appName"] is empty`}, nil
	}
	obs := repoObservation{name: repoFor(d.Secrets["github"]["owner"], appName).fullName()}
	gh, err := dialGitHub(d, appName)
	if err != nil {
		obs.detail = "the cluster repository cannot be read: " + refusalDetail(err)
		return obs, nil
	}
	obs.name, obs.url, obs.gh = gh.repo.fullName(), gh.repo.url(), gh
	read, err := gh.readRepo(ctx, d.SubscriptionID)
	if err != nil {
		return obs, err
	}
	switch {
	case !read.exists:
		obs.state = repoGone
	case !read.marked:
		obs.state = repoUnmarked
	default:
		obs.state = repoPresent
	}
	return obs, nil
}

func unknown(detail string) contract.Observation {
	return contract.Observation{State: contract.Unknown, StatusDetail: detail}
}

// refusalDetail drops the refused:<class> prefix, which a validate answer does not carry.
func refusalDetail(err error) string {
	var refused *handler.Refused
	if errors.As(err, &refused) {
		return refused.Detail
	}
	return err.Error()
}
