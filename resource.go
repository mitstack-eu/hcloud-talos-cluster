package main

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/mitstack-eu/operator-kit/v3/contract"
	"github.com/mitstack-eu/operator-kit/v3/handler"
)

// blockResource keeps no state between calls and finds every resource by label, so each Ensure takes the cluster
// on from wherever an earlier call left it.
type blockResource struct{}

var _ handler.ResourceReporter = (*blockResource)(nil)

// clock is a variable so a test can move the time a builder's deadline is judged against.
var clock = time.Now

const (
	resNetwork         = "hetzner.network"
	resFirewall        = "hetzner.firewall"
	resPrivateLB       = "api-load-balancer"
	resPublicLB        = "ingress-load-balancer"
	resRepository      = "github.repository"
	resSeedCommit      = "github.gitops-commit"
	resGateway         = "gateway"
	resNode            = "node"
	resCluster         = "hetzner.kubernetes-cluster"
	typeServer         = "hetzner.server"
	typeLoadBalancer   = "hetzner.load-balancer"
	typeRepository     = "github.gitops-repository"
	paramHostsPlatform = "hostsPlatform"
	// platformURL is where every node enrols and reports, as the manifest's endpoint names the platform.
	platformURL = "https://api.mitstack.dev"
	// unreleasedToolsTag is the tag of the zero-digest placeholder tools.Image carries between the kit's releases.
	unreleasedToolsTag = ":unreleased@"
)

const (
	defaultLocation   = "nbg1"
	defaultServerType = "cx33"
	gatewayServerType = "cx23"
	gatewayImage      = "ubuntu-24.04"
	// builderImage is Hetzner's Ubuntu image with Docker installed, so the builder runs imager without a package install.
	builderImage     = "docker-ce"
	loadBalancerType = "lb11"
	networkZone      = "eu-central"
	networkIPRange   = "10.0.0.0/16"
	subnetIPRange    = "10.0.0.0/24"
	// routerAddress is the first address of the network, which Hetzner's own router answers on.
	routerAddress = "10.0.0.1"
	// gatewayAddress is fixed, since the network's default route and every node's mirror name it.
	gatewayAddress = "10.0.0.2"
	// The load balancers join the network at fixed addresses before the gateway does, so neither takes the gateway's.
	apiLoadBalancerAddress     = "10.0.0.3"
	ingressLoadBalancerAddress = "10.0.0.4"
	defaultRoute               = "0.0.0.0/0"

	apiServerPort = 6443
	httpPort      = 80
	httpsPort     = 443
	// The public load balancer forwards to the NodePorts the edge listens on, which ADR 0076 fixes.
	httpNodePort  = 30080
	httpsNodePort = 30081

	// buildDeadline bounds the builder from its creation to powering itself off with the image on its disk.
	buildDeadline = 30 * time.Minute
)

// allowedLocations are the three locations of the eu-central network zone the cluster's subnet lives in.
var allowedLocations = []string{"fsn1", "nbg1", "hel1"}

// allowedServerTypes are x86, since the image is amd64, and hold a control plane with Cilium and Argo CD.
var allowedServerTypes = []string{"cx23", "cx33", "cx43", "cx53", "cpx22", "cpx52", "cpx62"}

type clusterParams struct {
	appName, location, serverType string
}

func readParams(d contract.Desired) (clusterParams, error) {
	p := clusterParams{appName: d.Params["appName"], location: d.Params["location"], serverType: d.Params["serverType"]}
	if p.appName == "" {
		return p, handler.Refuse("missing-param", `the app name is required: params["appName"] is empty`)
	}
	if p.location == "" {
		p.location = defaultLocation
	}
	if !slices.Contains(allowedLocations, p.location) {
		return p, handler.Refuse("invalid-param", `params["location"] is %q; the cluster runs in fsn1, nbg1 or hel1`, p.location)
	}
	if p.serverType == "" {
		p.serverType = defaultServerType
	}
	if !slices.Contains(allowedServerTypes, p.serverType) {
		return p, handler.Refuse("invalid-param", `params["serverType"] is %q; the cluster runs on %s`,
			p.serverType, strings.Join(allowedServerTypes, ", "))
	}
	if d.Params[paramHostsPlatform] == "true" {
		return p, handler.Refuse("invalid-param",
			`params["%s"] is true, and this release builds no cluster that boots the platform's own stores`, paramHostsPlatform)
	}
	return p, nil
}

func hetznerToken(d contract.Desired) (string, error) {
	token := d.Secrets["hetzner"]["api_token"]
	if token == "" {
		return "", handler.Refuse("missing-hetzner-access",
			"the Hetzner connection is missing: secrets.hetzner.api_token is empty, so the cluster cannot be reached; connect a Hetzner Cloud project token for this app and retry")
	}
	return token, nil
}

func subscriptionID(d contract.Desired) (string, error) {
	if !validSubscriptionID(d.SubscriptionID) {
		return "", handler.Refuse("invalid-subscription",
			"the subscription id is not a valid Hetzner label value, so the cluster's resources cannot be labelled or found by it")
	}
	return d.SubscriptionID, nil
}

type ensureRequest struct {
	token, subID string
	params       clusterParams
	gh           *githubAccess
}

func readEnsureRequest(d contract.Desired) (ensureRequest, error) {
	var req ensureRequest
	var err error
	if req.token, err = hetznerToken(d); err != nil {
		return req, err
	}
	if req.subID, err = subscriptionID(d); err != nil {
		return req, err
	}
	if req.params, err = readParams(d); err != nil {
		return req, err
	}
	if strings.Contains(toolsImage(), unreleasedToolsTag) {
		return req, handler.Refuse("tools-image-unreleased",
			"the block is built against a talos-cluster-kit whose tools image %s is a placeholder, so the node report could not run and nothing was changed; build it against a released kit", toolsImage())
	}
	req.gh, err = dialGitHub(d, req.params.appName)
	return req, err
}

type convergence struct {
	api   hetznerAPI
	state clusterState
	req   ensureRequest
	repo  repoRead
}

// Ensure takes one step that waits on Hetzner, since a call waiting on every action of the install outlasted the
// platform's two-minute verb timeout, and refuses before any change a cluster it cannot act on unambiguously, or a
// repository without the subscription's mark. The network and the repository need no wait, so they come first.
func (r *blockResource) Ensure(ctx context.Context, d contract.Desired) error {
	req, err := readEnsureRequest(d)
	if err != nil {
		return err
	}
	c, err := readConvergence(ctx, newHetznerAPI(req.token), req)
	if err != nil {
		return err
	}
	network, err := ensureNetwork(ctx, c)
	if err != nil {
		return err
	}
	if err := ensureRepository(ctx, c); err != nil {
		return err
	}
	s, ok, err := nextStep(ctx, c, network)
	if err != nil || !ok {
		return err
	}
	return s.take(ctx, req.subID)
}

func readConvergence(ctx context.Context, api hetznerAPI, req ensureRequest) (convergence, error) {
	st, err := listCluster(ctx, api, req.subID)
	if err != nil {
		return convergence{}, err
	}
	repo, err := req.gh.readRepo(ctx, req.subID)
	if err != nil {
		return convergence{}, err
	}
	c := convergence{api: api, state: st, req: req, repo: repo}
	return c, c.refusal()
}

// refusal names a cluster Ensure must not touch.
func (c convergence) refusal() error {
	if dups := c.state.duplicates(); len(dups) > 0 {
		return handler.Refuse("ambiguous-cluster",
			"the cluster is ambiguous, so nothing was changed: %s; delete the extra ones from the Hetzner project and retry", strings.Join(dups, "; "))
	}
	if _, ok := c.state.network(); !ok && c.state.hasServers() {
		return handler.Refuse("network-gone",
			"servers carry this subscription's labels, but no network does, so nothing was changed; a new network would not hold their addresses, so unsubscribe and subscribe again to rebuild the cluster")
	}
	if c.repo.exists && !c.repo.marked {
		return handler.Refuse("repository-not-marked",
			"the repository %s exists but does not carry this subscription's mark, the topics %s, so nothing was changed. Add both topics if it is this cluster's repository, or move it out of the way, and retry",
			c.req.gh.repo.fullName(), strings.Join(repositoryMark(c.req.subID), " and "))
	}
	return nil
}

func (st clusterState) hasServers() bool {
	for _, servers := range st.servers {
		if len(servers) > 0 {
			return true
		}
	}
	return false
}

// step is one change to the cluster and the wait on Hetzner's actions for it.
type step struct {
	what string
	run  func(ctx context.Context) error
}

// take logs how long the step ran, so a live run shows each call's share of the verb timeout.
func (s step) take(ctx context.Context, subID string) error {
	start := time.Now()
	err := s.run(ctx)
	outcome := "took"
	if err != nil {
		outcome = "failed after"
	}
	log.Printf("%s: subscription %s: the step to %s %s %s", blockID, subID, s.what, outcome, time.Since(start).Round(time.Millisecond))
	return err
}

// nextStep is the first step the cluster lacks, each in the order it depends on the one before, and none while only
// Hetzner or the node can finish the work in flight.
func nextStep(ctx context.Context, c convergence, network hcNetwork) (step, bool, error) {
	firewall, ok := c.state.firewall()
	if !ok {
		return step{"create the firewall", func(ctx context.Context) error { return createFirewall(ctx, c) }}, true, nil
	}
	for _, lb := range []struct {
		want    loadBalancerSpec
		address string
	}{
		{apiLoadBalancer(c.req, network.ID), apiLoadBalancerAddress},
		{ingressLoadBalancer(c.req, network.ID), ingressLoadBalancerAddress},
	} {
		if s, ok := loadBalancerStep(c, lb.want, lb.address); ok {
			return s, true, nil
		}
	}
	return nodeStep(ctx, c, clusterSite{network: network, firewall: firewall})
}

type clusterSite struct {
	network    hcNetwork
	firewall   hcFirewall
	apiAddress string
}

// nodeStep builds anew only a cluster with neither node nor gateway left, since the cluster's keys live only in its
// node, its snapshot and a builder.
func nodeStep(ctx context.Context, c convergence, site clusterSite) (step, bool, error) {
	gw, hasGateway := c.state.server(roleGateway)
	_, hasNode := c.state.server(roleNode)
	builder, hasBuilder := c.state.server(roleBuilder)
	snap, hasSnapshot := c.state.snapshot()
	imaged := hasSnapshot && snap.Status == snapshotAvailable
	switch {
	case hasNode && !hasGateway:
		return step{}, false, handler.Refuse("gateway-lost",
			"the node is running, but no gateway carries this subscription's labels, so the node cannot pull an image and nothing was changed; the gateway's certificate authority is gone with it, so unsubscribe and subscribe again to rebuild the cluster")
	case hasBuilder && (hasNode || imaged):
		return step{"delete the builder, whose disk holds every key of the cluster, beside its snapshot", func(ctx context.Context) error {
			return c.api.DeleteServer(ctx, builder.ID)
		}}, true, nil
	case hasNode:
		return step{}, false, nil
	case !hasGateway:
		return installStep(c, site), true, nil
	case gw.PrivateIPs[site.network.ID] == "":
		return step{"attach the gateway to the network at its fixed address before its first boot", func(ctx context.Context) error {
			return c.api.AttachServerToNetwork(ctx, gw.ID, site.network.ID, gatewayAddress)
		}}, true, nil
	case gw.Status == serverOff:
		return step{"power on the gateway", func(ctx context.Context) error { return c.api.PowerOnServer(ctx, gw.ID) }}, true, nil
	case hasSnapshot:
		return step{"create the node from the snapshot", func(ctx context.Context) error { return createNode(ctx, c, site, snap) }}, imaged, nil
	case hasBuilder:
		return builderStep(ctx, c, builder)
	default:
		return step{}, false, handler.Refuse("cluster-secrets-lost",
			"the gateway exists but neither the builder nor the cluster's snapshot does, so the cluster's keys, which only they held, are gone; unsubscribe and subscribe again to rebuild the cluster")
	}
}

// installStep first deletes a builder or snapshot left from a cluster whose gateway is gone, since the keys they hold
// match no gateway.
func installStep(c convergence, site clusterSite) step {
	if len(c.state.servers[roleBuilder]) == 0 && len(c.state.snapshots) == 0 {
		api, _ := c.state.loadBalancer(roleAPI)
		site.apiAddress = api.PrivateIPs[site.network.ID]
		return step{"create the gateway and the builder", func(ctx context.Context) error { return install(ctx, c, site) }}
	}
	return step{"delete the builder and snapshot of a cluster with no gateway", func(ctx context.Context) error {
		for _, builder := range c.state.servers[roleBuilder] {
			if err := c.api.DeleteServer(ctx, builder.ID); err != nil {
				return fmt.Errorf("deleting the builder %d of a cluster with no gateway: %w", builder.ID, err)
			}
		}
		for _, snap := range c.state.snapshots {
			if err := c.api.DeleteSnapshot(ctx, snap.ID); err != nil {
				return fmt.Errorf("deleting the snapshot %d of a cluster with no gateway: %w", snap.ID, err)
			}
		}
		return nil
	}}
}

// builderStep powers on a builder that has never run, snapshots one that ran and powered itself off, and deletes one
// still running past buildDeadline, since its script leaves a failed build running and only the deadline tells.
func builderStep(ctx context.Context, c convergence, builder hcServer) (step, bool, error) {
	switch builder.Status {
	case serverRunning:
		if clock().Sub(builder.Created) < buildDeadline {
			return step{}, false, nil
		}
		return step{"delete the builder that did not finish", func(ctx context.Context) error {
			if err := c.api.DeleteServer(ctx, builder.ID); err != nil {
				return fmt.Errorf("deleting the builder %d that did not finish: %w", builder.ID, err)
			}
			return handler.Refuse("image-build-failed",
				"the builder did not power itself off within %s of its creation, so the cluster's image was not built and the builder is deleted; unsubscribe and subscribe again to rebuild the cluster", buildDeadline)
		}}, true, nil
	case serverOff:
		started, err := c.api.ServerStarted(ctx, builder.ID)
		if err != nil || !started {
			return step{"power on the builder", func(ctx context.Context) error { return c.api.PowerOnServer(ctx, builder.ID) }}, err == nil, err
		}
		return step{"start the snapshot of the builder's disk", func(ctx context.Context) error {
			_, err := c.api.StartSnapshot(ctx, builder.ID, snapshotSpec{
				Description: resourceName(c.req.params.appName, c.req.subID, roleImage),
				Labels:      ownerLabels(c.req.subID, roleImage),
			})
			return err
		}}, true, nil
	default:
		return step{}, false, nil
	}
}

// createNode boots the node from the cluster's own snapshot, with no user data and no public address.
func createNode(ctx context.Context, c convergence, site clusterSite, snap hcSnapshot) error {
	_, err := c.api.CreateServers(ctx, serverSpec{
		Name:        resourceName(c.req.params.appName, c.req.subID, roleNode),
		ServerType:  c.req.params.serverType,
		Location:    c.req.params.location,
		ImageID:     snap.ID,
		Labels:      ownerLabels(c.req.subID, roleNode),
		NetworkIDs:  []int64{site.network.ID},
		FirewallIDs: []int64{site.firewall.ID},
	})
	if err != nil {
		return fmt.Errorf("creating the cluster node from snapshot %d: %w", snap.ID, err)
	}
	return nil
}

func ensureNetwork(ctx context.Context, c convergence) (hcNetwork, error) {
	if n, ok := c.state.network(); ok {
		return n, nil
	}
	n, err := c.api.CreateNetwork(ctx, networkSpec{
		Name:    resourceName(c.req.params.appName, c.req.subID, "network"),
		IPRange: networkIPRange,
		Labels:  ownerLabels(c.req.subID, "network"),
		Subnet:  hcSubnet{NetworkZone: networkZone, IPRange: subnetIPRange},
		Routes:  []hcRoute{{Destination: defaultRoute, Gateway: gatewayAddress}},
	})
	if err != nil {
		return hcNetwork{}, fmt.Errorf("creating the cluster network: %w", err)
	}
	return n, nil
}

func createFirewall(ctx context.Context, c convergence) error {
	_, err := c.api.CreateFirewall(ctx, firewallSpec{
		Name: resourceName(c.req.params.appName, c.req.subID, "firewall"), Labels: ownerLabels(c.req.subID, "firewall"),
	})
	if err != nil {
		return fmt.Errorf("creating the cluster firewall: %w", err)
	}
	return nil
}

// apiLoadBalancer carries the Kubernetes API on the private network alone, with no public interface.
func apiLoadBalancer(req ensureRequest, networkID int64) loadBalancerSpec {
	return loadBalancerSpec{
		Name:           resourceName(req.params.appName, req.subID, roleAPI),
		Type:           loadBalancerType,
		Location:       req.params.location,
		Labels:         ownerLabels(req.subID, roleAPI),
		NetworkID:      networkID,
		Services:       []lbService{{ListenPort: apiServerPort, DestinationPort: apiServerPort}},
		TargetSelector: roleSelector(req.subID, roleNode),
	}
}

// ingressLoadBalancer serves ports 80 and 443 on its public address to the edge's NodePorts, with PROXY protocol.
func ingressLoadBalancer(req ensureRequest, networkID int64) loadBalancerSpec {
	return loadBalancerSpec{
		Name:            resourceName(req.params.appName, req.subID, roleIngress),
		Type:            loadBalancerType,
		Location:        req.params.location,
		Labels:          ownerLabels(req.subID, roleIngress),
		NetworkID:       networkID,
		PublicInterface: true,
		Services: []lbService{
			{ListenPort: httpPort, DestinationPort: httpNodePort, ProxyProtocol: true},
			{ListenPort: httpsPort, DestinationPort: httpsNodePort, ProxyProtocol: true},
		},
		TargetSelector: roleSelector(req.subID, roleNode),
	}
}

// loadBalancerStep is the next step the load balancer lacks. It is attached before its public interface is disabled or
// its private target added, since Hetzner refuses either on a load balancer not yet attached to a network.
func loadBalancerStep(c convergence, want loadBalancerSpec, address string) (step, bool) {
	r := want.Labels[keyRole]
	lb, ok := c.state.loadBalancer(role(r))
	switch {
	case !ok:
		return step{"create the " + r + " load balancer", func(ctx context.Context) error { return createLoadBalancer(ctx, c.api, want) }}, true
	case lb.PrivateIPs[want.NetworkID] == "":
		return step{"attach the " + r + " load balancer to the network", func(ctx context.Context) error {
			if err := c.api.AttachLoadBalancerToNetwork(ctx, lb.ID, want.NetworkID, address); err != nil {
				return fmt.Errorf("attaching the cluster's %s load balancer: %w", r, err)
			}
			return nil
		}}, true
	case lb.PublicInterface && !want.PublicInterface:
		return step{"disable the " + r + " load balancer's public interface", func(ctx context.Context) error {
			return c.api.DisableLoadBalancerPublicInterface(ctx, lb.ID)
		}}, true
	}
	for _, s := range want.Services {
		if !slices.ContainsFunc(lb.Services, func(have lbService) bool { return have.ListenPort == s.ListenPort }) {
			return step{fmt.Sprintf("add port %d to the %s load balancer", s.ListenPort, r), func(ctx context.Context) error {
				return c.api.AddLoadBalancerService(ctx, lb.ID, s)
			}}, true
		}
	}
	if !slices.Contains(lb.TargetSelectors, want.TargetSelector) {
		return step{"target the nodes from the " + r + " load balancer", func(ctx context.Context) error {
			return c.api.AddLoadBalancerTarget(ctx, lb.ID, want.TargetSelector)
		}}, true
	}
	return step{}, false
}

// createLoadBalancer creates one bare, with its public interface on, since Hetzner's create takes no address on the network.
func createLoadBalancer(ctx context.Context, api hetznerAPI, want loadBalancerSpec) error {
	_, err := api.CreateLoadBalancer(ctx, loadBalancerSpec{
		Name: want.Name, Type: want.Type, Location: want.Location, Labels: want.Labels, PublicInterface: true,
	})
	if err != nil {
		return fmt.Errorf("creating the cluster's %s load balancer: %w", want.Labels[keyRole], err)
	}
	return nil
}

// ensureRepository creates and marks the cluster repository only when none exists, since Ensure has already
// refused an unmarked one, and seeds whatever of the seed is missing.
func ensureRepository(ctx context.Context, c convergence) error {
	if !c.repo.exists {
		if err := c.req.gh.createRepo(ctx); err != nil {
			return err
		}
		if err := c.req.gh.markRepo(ctx, c.req.subID); err != nil {
			return err
		}
	}
	return c.req.gh.seedRepo(ctx, seedFiles)
}
