package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/mitstack-eu/operator-kit/v2/contract"
	"github.com/mitstack-eu/operator-kit/v2/handler"
	"github.com/mitstack-eu/talos-cluster-kit/components"
)

// blockResource keeps no state between calls and finds every resource by label, so each Ensure takes the cluster
// on from wherever an earlier call left it.
type blockResource struct {
	now func() time.Time
}

var _ handler.ResourceReporter = (*blockResource)(nil)

func (r *blockResource) clock() time.Time {
	if r.now == nil {
		return time.Now()
	}
	return r.now()
}

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
	platformURL         = "https://api.mitstack.dev"
	envLogExportAddress = "MITSTACK_LOG_EXPORT_ENDPOINT"
	envLogExportCA      = "MITSTACK_LOG_EXPORT_CA"
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
	defaultRoute   = "0.0.0.0/0"

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

// logExport reads the receiver every node exports its logs to, which the operator's host names for every cluster.
func logExport() (components.LogExport, error) {
	endpoint, ca := os.Getenv(envLogExportAddress), os.Getenv(envLogExportCA)
	if endpoint == "" || ca == "" {
		return components.LogExport{}, handler.Refuse(handler.ClassHostCannotRun,
			"the operator's host sets no %s and %s, so the nodes would have no log receiver; nothing was changed", envLogExportAddress, envLogExportCA)
	}
	return components.LogExport{Endpoint: endpoint, CA: []byte(ca)}, nil
}

type ensureRequest struct {
	token, subID string
	params       clusterParams
	gh           *githubAccess
	logs         components.LogExport
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
	if toolsImage() == "" {
		return req, handler.Refuse("tools-image-unpinned",
			"this release pins no tools image for the node report and enrolment Jobs, so nothing was changed")
	}
	if req.logs, err = logExport(); err != nil {
		return req, err
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

// ensureSteps bounds how often one Ensure reads the cluster again after a step that made something new.
const ensureSteps = 3

// Ensure takes the cluster as far on as it can and returns while the builder is still building, refusing before any
// change a cluster it cannot act on unambiguously, or a repository without the subscription's mark.
func (r *blockResource) Ensure(ctx context.Context, d contract.Desired) error {
	req, err := readEnsureRequest(d)
	if err != nil {
		return err
	}
	api := newHetznerAPI(req.token)
	for range ensureSteps {
		c, err := readConvergence(ctx, api, req)
		if err != nil {
			return err
		}
		again, err := r.converge(ctx, c)
		if err != nil || !again {
			return err
		}
	}
	return nil
}

func readConvergence(ctx context.Context, api hetznerAPI, req ensureRequest) (convergence, error) {
	st, err := listCluster(ctx, api, req.subID)
	if err != nil {
		return convergence{}, err
	}
	if dups := st.duplicates(); len(dups) > 0 {
		return convergence{}, handler.Refuse("ambiguous-cluster",
			"the cluster is ambiguous, so nothing was changed: %s; delete the extra ones from the Hetzner project and retry", strings.Join(dups, "; "))
	}
	if _, ok := st.network(); !ok && st.hasServers() {
		return convergence{}, handler.Refuse("network-gone",
			"servers carry this subscription's labels, but no network does, so nothing was changed; a new network would not hold their addresses, so unsubscribe and subscribe again to rebuild the cluster")
	}
	repo, err := req.gh.readRepo(ctx, req.subID)
	if err != nil {
		return convergence{}, err
	}
	if repo.exists && !repo.marked {
		return convergence{}, handler.Refuse("repository-not-marked",
			"the repository %s exists but does not carry this subscription's mark, the topics %s, so nothing was changed. Add both topics if it is this cluster's repository, or move it out of the way, and retry",
			req.gh.repo.fullName(), strings.Join(repositoryMark(req.subID), " and "))
	}
	return convergence{api: api, state: st, req: req, repo: repo}, nil
}

func (st clusterState) hasServers() bool {
	for _, servers := range st.servers {
		if len(servers) > 0 {
			return true
		}
	}
	return false
}

// converge reports whether it made something the next step may already build on.
func (r *blockResource) converge(ctx context.Context, c convergence) (bool, error) {
	network, err := ensureNetwork(ctx, c)
	if err != nil {
		return false, err
	}
	firewall, err := ensureFirewall(ctx, c)
	if err != nil {
		return false, err
	}
	if err := ensureRepository(ctx, c); err != nil {
		return false, err
	}
	privateLB, err := ensureLoadBalancer(ctx, c, apiLoadBalancer(c.req, network.ID))
	if err != nil {
		return false, err
	}
	if _, err := ensureLoadBalancer(ctx, c, ingressLoadBalancer(c.req, network.ID)); err != nil {
		return false, err
	}
	apiAddress, err := privateAddress(ctx, c, privateLB, network.ID)
	if err != nil || apiAddress == "" {
		return false, err
	}
	site := clusterSite{network: network, firewall: firewall, apiAddress: apiAddress}
	return r.ensureNode(ctx, c, site)
}

// privateAddress reads the load balancer back when its create answer named no address on the network, and is
// empty while Hetzner has not assigned one, so a later call goes on from there.
func privateAddress(ctx context.Context, c convergence, lb hcLoadBalancer, networkID int64) (string, error) {
	if addr := lb.PrivateIPs[networkID]; addr != "" {
		return addr, nil
	}
	lbs, err := c.api.ListLoadBalancers(ctx, roleSelector(c.req.subID, role(lb.Labels[keyRole])))
	if err != nil {
		return "", fmt.Errorf("reading the %s load balancer's address: %w", lb.Labels[keyRole], err)
	}
	for _, found := range lbs {
		if found.ID == lb.ID {
			return found.PrivateIPs[networkID], nil
		}
	}
	return "", nil
}

type clusterSite struct {
	network    hcNetwork
	firewall   hcFirewall
	apiAddress string
}

// ensureNode builds anew only a cluster with neither node nor gateway left, since the cluster's keys live only in
// its node, its snapshot and a builder.
func (r *blockResource) ensureNode(ctx context.Context, c convergence, site clusterSite) (bool, error) {
	_, hasGateway := c.state.server(roleGateway)
	_, hasNode := c.state.server(roleNode)
	builder, hasBuilder := c.state.server(roleBuilder)
	snap, hasSnapshot := c.state.snapshot()
	if hasBuilder && (hasSnapshot || hasNode) {
		if err := c.api.DeleteServer(ctx, builder.ID); err != nil {
			return false, fmt.Errorf("deleting the builder %d, whose disk holds every key of the cluster, beside its snapshot: %w", builder.ID, err)
		}
		hasBuilder = false
	}
	switch {
	case hasNode && !hasGateway:
		return false, handler.Refuse("gateway-lost",
			"the node is running, but no gateway carries this subscription's labels, so the node cannot pull an image and nothing was changed; the gateway's certificate authority is gone with it, so unsubscribe and subscribe again to rebuild the cluster")
	case hasNode:
		return false, nil
	case !hasGateway:
		return true, r.installAnew(ctx, c, site)
	case hasSnapshot:
		return false, createNode(ctx, c, site, snap)
	case hasBuilder:
		return false, r.finishBuild(ctx, c, site, builder)
	default:
		return false, handler.Refuse("cluster-secrets-lost",
			"the gateway exists but neither the builder nor the cluster's snapshot does, so the cluster's keys, which only they held, are gone; unsubscribe and subscribe again to rebuild the cluster")
	}
}

// installAnew deletes a builder or snapshot left from a cluster whose gateway is gone, since the keys they hold
// match no gateway, and installs the cluster.
func (r *blockResource) installAnew(ctx context.Context, c convergence, site clusterSite) error {
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
	return r.install(ctx, c, site)
}

// finishBuild snapshots a builder that has powered itself off, and deletes one that has not within buildDeadline.
func (r *blockResource) finishBuild(ctx context.Context, c convergence, site clusterSite, builder hcServer) error {
	if builder.Status != "off" {
		if r.clock().Sub(builder.Created) < buildDeadline {
			return nil
		}
		if err := c.api.DeleteServer(ctx, builder.ID); err != nil {
			return fmt.Errorf("deleting the builder %d that did not finish: %w", builder.ID, err)
		}
		return handler.Refuse("image-build-failed",
			"the builder did not power itself off within %s of its creation, so the cluster's image was not built and the builder is deleted; unsubscribe and subscribe again to rebuild the cluster", buildDeadline)
	}
	snap, err := c.api.CreateSnapshot(ctx, builder.ID, snapshotSpec{
		Description: resourceName(c.req.params.appName, c.req.subID, roleImage),
		Labels:      ownerLabels(c.req.subID, roleImage),
	})
	if err != nil {
		return fmt.Errorf("snapshotting the builder %d: %w", builder.ID, err)
	}
	if err := c.api.DeleteServer(ctx, builder.ID); err != nil {
		return fmt.Errorf("deleting the builder %d once its disk is a snapshot: %w", builder.ID, err)
	}
	return createNode(ctx, c, site, snap)
}

// createNode boots the node from the cluster's own snapshot, with no user data and no public address.
func createNode(ctx context.Context, c convergence, site clusterSite, snap hcSnapshot) error {
	_, err := c.api.CreateServer(ctx, serverSpec{
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

func ensureFirewall(ctx context.Context, c convergence) (hcFirewall, error) {
	if fw, ok := c.state.firewall(); ok {
		return fw, nil
	}
	fw, err := c.api.CreateFirewall(ctx, firewallSpec{
		Name: resourceName(c.req.params.appName, c.req.subID, "firewall"), Labels: ownerLabels(c.req.subID, "firewall"),
	})
	if err != nil {
		return hcFirewall{}, fmt.Errorf("creating the cluster firewall: %w", err)
	}
	return fw, nil
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

func ensureLoadBalancer(ctx context.Context, c convergence, spec loadBalancerSpec) (hcLoadBalancer, error) {
	if lb, ok := c.state.loadBalancer(role(spec.Labels[keyRole])); ok {
		return lb, nil
	}
	lb, err := c.api.CreateLoadBalancer(ctx, spec)
	if err != nil {
		return hcLoadBalancer{}, fmt.Errorf("creating the cluster's %s load balancer: %w", spec.Labels[keyRole], err)
	}
	return lb, nil
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
