package main

// The interface offers no rebuild, resize, type change or user-data update, so a verb can never reconfigure a running server.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// hetznerAPI is every Hetzner call the block makes; each list takes a label selector, and each delete treats a
// resource Hetzner no longer has as deleted.
type hetznerAPI interface {
	ListServers(ctx context.Context, labelSelector string) ([]hcServer, error)
	// CreateServer returns once Hetzner's create action has finished.
	CreateServer(ctx context.Context, spec serverSpec) (hcServer, error)
	AttachServerToNetwork(ctx context.Context, serverID, networkID int64, ip string) error
	PowerOnServer(ctx context.Context, id int64) error
	DeleteServer(ctx context.Context, id int64) error
	ListNetworks(ctx context.Context, labelSelector string) ([]hcNetwork, error)
	CreateNetwork(ctx context.Context, spec networkSpec) (hcNetwork, error)
	DeleteNetwork(ctx context.Context, id int64) error
	ListFirewalls(ctx context.Context, labelSelector string) ([]hcFirewall, error)
	CreateFirewall(ctx context.Context, spec firewallSpec) (hcFirewall, error)
	DeleteFirewall(ctx context.Context, id int64) error
	ListLoadBalancers(ctx context.Context, labelSelector string) ([]hcLoadBalancer, error)
	CreateLoadBalancer(ctx context.Context, spec loadBalancerSpec) (hcLoadBalancer, error)
	DeleteLoadBalancer(ctx context.Context, id int64) error
	ListSnapshots(ctx context.Context, labelSelector string) ([]hcSnapshot, error)
	// CreateSnapshot returns once Hetzner has finished imaging the server's disk.
	CreateSnapshot(ctx context.Context, serverID int64, spec snapshotSpec) (hcSnapshot, error)
	DeleteSnapshot(ctx context.Context, id int64) error
}

// hcServer is a server as the block reads it, with PrivateIPs keyed by network id.
type hcServer struct {
	ID                                 int64
	Name, Location, ServerType, Status string
	Labels                             map[string]string
	PublicIPv4                         string
	PrivateIPs                         map[int64]string
	FirewallIDs                        []int64
	ImageID                            int64
	Created                            time.Time
}

// serverSpec is everything the block sets on a server it creates. A server boots from Image by name, or from the
// snapshot ImageID names, and a stopped one waits for PowerOnServer.
type serverSpec struct {
	Name, ServerType, Location string
	Image                      string
	ImageID                    int64
	UserData                   string
	Labels                     map[string]string
	NetworkIDs, FirewallIDs    []int64
	PublicIPv4                 bool
	Stopped                    bool
}

type hcNetwork struct {
	ID            int64
	Name, IPRange string
	Labels        map[string]string
	Routes        []hcRoute
}

// hcRoute sends Destination to Gateway, a server inside the network.
type hcRoute struct {
	Destination, Gateway string
}

type networkSpec struct {
	Name, IPRange string
	Labels        map[string]string
	Subnet        hcSubnet
	Routes        []hcRoute
}

type hcSubnet struct {
	NetworkZone, IPRange string
}

// hcFirewall is a firewall as the block reads it; the block creates every firewall with no rule.
type hcFirewall struct {
	ID        int64
	Name      string
	Labels    map[string]string
	RuleCount int
}

type firewallSpec struct {
	Name   string
	Labels map[string]string
}

// hcLoadBalancer is a load balancer as the block reads it, with PrivateIPs keyed by network id.
type hcLoadBalancer struct {
	ID              int64
	Name, Location  string
	Labels          map[string]string
	PublicInterface bool
	PublicIPv4      string
	PrivateIPs      map[int64]string
	Services        []lbService
	TargetSelectors []string
}

// lbService forwards a listen port to a port on each target over TCP.
type lbService struct {
	ListenPort, DestinationPort int
	ProxyProtocol               bool
}

// loadBalancerSpec creates a load balancer on NetworkID that reaches every server TargetSelector matches by its private address.
type loadBalancerSpec struct {
	Name, Type, Location string
	Labels               map[string]string
	NetworkID            int64
	PublicInterface      bool
	Services             []lbService
	TargetSelector       string
}

type hcSnapshot struct {
	ID          int64
	Description string
	Status      string
	Labels      map[string]string
}

type snapshotSpec struct {
	Description string
	Labels      map[string]string
}

const (
	hetznerTimeout = 60 * time.Second

	healthCheckInterval = 15 * time.Second
	healthCheckTimeout  = 10 * time.Second
	healthCheckRetries  = 3
)

// hetznerEndpoint is fixed at build time and never taken from a request.
var hetznerEndpoint = "https://api.hetzner.cloud/v1"

// newHetznerAPI is a variable so a test can point every verb at a double.
var newHetznerAPI = func(token string) hetznerAPI {
	return &hcloudAPI{client: hcloud.NewClient(
		hcloud.WithToken(token),
		hcloud.WithEndpoint(hetznerEndpoint),
		hcloud.WithApplication("mitstack-"+blockID, ""),
		hcloud.WithHTTPClient(&http.Client{Timeout: hetznerTimeout}),
	)}
}

// hcloudAPI waits for every Hetzner action a call starts, so the next call never meets a locked resource.
type hcloudAPI struct {
	client *hcloud.Client
}

func listOpts(labelSelector string) hcloud.ListOpts {
	return hcloud.ListOpts{LabelSelector: labelSelector}
}

func (a *hcloudAPI) ListServers(ctx context.Context, labelSelector string) ([]hcServer, error) {
	servers, err := a.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{ListOpts: listOpts(labelSelector)})
	if err != nil {
		return nil, fmt.Errorf("listing servers: %w", err)
	}
	out := make([]hcServer, 0, len(servers))
	for _, s := range servers {
		out = append(out, serverFromHcloud(s))
	}
	return out, nil
}

func (a *hcloudAPI) CreateServer(ctx context.Context, spec serverSpec) (hcServer, error) {
	opts := hcloud.ServerCreateOpts{
		Name:             spec.Name,
		ServerType:       &hcloud.ServerType{Name: spec.ServerType},
		Image:            &hcloud.Image{ID: spec.ImageID, Name: spec.Image},
		Location:         &hcloud.Location{Name: spec.Location},
		UserData:         spec.UserData,
		Labels:           spec.Labels,
		StartAfterCreate: hcloud.Ptr(!spec.Stopped),
		PublicNet:        &hcloud.ServerCreatePublicNet{EnableIPv4: spec.PublicIPv4, EnableIPv6: false},
	}
	for _, id := range spec.NetworkIDs {
		opts.Networks = append(opts.Networks, &hcloud.Network{ID: id})
	}
	for _, id := range spec.FirewallIDs {
		opts.Firewalls = append(opts.Firewalls, &hcloud.ServerCreateFirewall{Firewall: hcloud.Firewall{ID: id}})
	}
	res, _, err := a.client.Server.Create(ctx, opts)
	if err != nil {
		return hcServer{}, fmt.Errorf("creating server %s: %w", spec.Name, err)
	}
	if err := a.client.Action.WaitFor(ctx, append([]*hcloud.Action{res.Action}, res.NextActions...)...); err != nil {
		return hcServer{}, fmt.Errorf("waiting for server %s to be created: %w", spec.Name, err)
	}
	return serverFromHcloud(res.Server), nil
}

func (a *hcloudAPI) AttachServerToNetwork(ctx context.Context, serverID, networkID int64, ip string) error {
	action, _, err := a.client.Server.AttachToNetwork(ctx, &hcloud.Server{ID: serverID},
		hcloud.ServerAttachToNetworkOpts{Network: &hcloud.Network{ID: networkID}, IP: net.ParseIP(ip)})
	if err != nil {
		return fmt.Errorf("attaching server %d to network %d at %s: %w", serverID, networkID, ip, err)
	}
	return a.wait(ctx, action, fmt.Sprintf("server %d to attach to network %d", serverID, networkID))
}

func (a *hcloudAPI) PowerOnServer(ctx context.Context, id int64) error {
	action, _, err := a.client.Server.Poweron(ctx, &hcloud.Server{ID: id})
	if err != nil {
		return fmt.Errorf("powering on server %d: %w", id, err)
	}
	return a.wait(ctx, action, fmt.Sprintf("server %d to power on", id))
}

func (a *hcloudAPI) DeleteServer(ctx context.Context, id int64) error {
	res, _, err := a.client.Server.DeleteWithResult(ctx, &hcloud.Server{ID: id})
	if hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting server %d: %w", id, err)
	}
	return a.wait(ctx, res.Action, fmt.Sprintf("server %d to be deleted", id))
}

func (a *hcloudAPI) wait(ctx context.Context, action *hcloud.Action, what string) error {
	if err := a.client.Action.WaitFor(ctx, action); err != nil {
		return fmt.Errorf("waiting for %s: %w", what, err)
	}
	return nil
}

func (a *hcloudAPI) ListNetworks(ctx context.Context, labelSelector string) ([]hcNetwork, error) {
	networks, err := a.client.Network.AllWithOpts(ctx, hcloud.NetworkListOpts{ListOpts: listOpts(labelSelector)})
	if err != nil {
		return nil, fmt.Errorf("listing networks: %w", err)
	}
	out := make([]hcNetwork, 0, len(networks))
	for _, n := range networks {
		out = append(out, networkFromHcloud(n))
	}
	return out, nil
}

func (a *hcloudAPI) CreateNetwork(ctx context.Context, spec networkSpec) (hcNetwork, error) {
	ipRange, err := parseCIDR(spec.IPRange)
	if err != nil {
		return hcNetwork{}, err
	}
	subnetRange, err := parseCIDR(spec.Subnet.IPRange)
	if err != nil {
		return hcNetwork{}, err
	}
	opts := hcloud.NetworkCreateOpts{
		Name: spec.Name, IPRange: ipRange, Labels: spec.Labels,
		Subnets: []hcloud.NetworkSubnet{{
			Type: hcloud.NetworkSubnetTypeCloud, NetworkZone: hcloud.NetworkZone(spec.Subnet.NetworkZone), IPRange: subnetRange,
		}},
	}
	for _, r := range spec.Routes {
		destination, err := parseCIDR(r.Destination)
		if err != nil {
			return hcNetwork{}, err
		}
		opts.Routes = append(opts.Routes, hcloud.NetworkRoute{Destination: destination, Gateway: net.ParseIP(r.Gateway)})
	}
	n, _, err := a.client.Network.Create(ctx, opts)
	if err != nil {
		return hcNetwork{}, fmt.Errorf("creating network %s: %w", spec.Name, err)
	}
	return networkFromHcloud(n), nil
}

func (a *hcloudAPI) DeleteNetwork(ctx context.Context, id int64) error {
	_, err := a.client.Network.Delete(ctx, &hcloud.Network{ID: id})
	if err != nil && !hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
		return fmt.Errorf("deleting network %d: %w", id, err)
	}
	return nil
}

func (a *hcloudAPI) ListFirewalls(ctx context.Context, labelSelector string) ([]hcFirewall, error) {
	firewalls, err := a.client.Firewall.AllWithOpts(ctx, hcloud.FirewallListOpts{ListOpts: listOpts(labelSelector)})
	if err != nil {
		return nil, fmt.Errorf("listing firewalls: %w", err)
	}
	out := make([]hcFirewall, 0, len(firewalls))
	for _, fw := range firewalls {
		out = append(out, hcFirewall{ID: fw.ID, Name: fw.Name, Labels: fw.Labels, RuleCount: len(fw.Rules)})
	}
	return out, nil
}

// CreateFirewall sends an empty rule list, so the firewall admits no inbound packet from the internet.
func (a *hcloudAPI) CreateFirewall(ctx context.Context, spec firewallSpec) (hcFirewall, error) {
	res, _, err := a.client.Firewall.Create(ctx, hcloud.FirewallCreateOpts{Name: spec.Name, Labels: spec.Labels, Rules: []hcloud.FirewallRule{}})
	if err != nil {
		return hcFirewall{}, fmt.Errorf("creating firewall %s: %w", spec.Name, err)
	}
	if err := a.client.Action.WaitFor(ctx, res.Actions...); err != nil {
		return hcFirewall{}, fmt.Errorf("waiting for firewall %s to be created: %w", spec.Name, err)
	}
	return hcFirewall{ID: res.Firewall.ID, Name: res.Firewall.Name, Labels: res.Firewall.Labels, RuleCount: len(res.Firewall.Rules)}, nil
}

func (a *hcloudAPI) DeleteFirewall(ctx context.Context, id int64) error {
	_, err := a.client.Firewall.Delete(ctx, &hcloud.Firewall{ID: id})
	if err != nil && !hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
		return fmt.Errorf("deleting firewall %d: %w", id, err)
	}
	return nil
}

func (a *hcloudAPI) ListLoadBalancers(ctx context.Context, labelSelector string) ([]hcLoadBalancer, error) {
	lbs, err := a.client.LoadBalancer.AllWithOpts(ctx, hcloud.LoadBalancerListOpts{ListOpts: listOpts(labelSelector)})
	if err != nil {
		return nil, fmt.Errorf("listing load balancers: %w", err)
	}
	out := make([]hcLoadBalancer, 0, len(lbs))
	for _, lb := range lbs {
		out = append(out, loadBalancerFromHcloud(lb))
	}
	return out, nil
}

// CreateLoadBalancer gives every service a TCP health check on its destination port and every target its private address.
func (a *hcloudAPI) CreateLoadBalancer(ctx context.Context, spec loadBalancerSpec) (hcLoadBalancer, error) {
	opts := hcloud.LoadBalancerCreateOpts{
		Name:             spec.Name,
		LoadBalancerType: &hcloud.LoadBalancerType{Name: spec.Type},
		Location:         &hcloud.Location{Name: spec.Location},
		Labels:           spec.Labels,
		Network:          &hcloud.Network{ID: spec.NetworkID},
		PublicInterface:  hcloud.Ptr(spec.PublicInterface),
		Targets: []hcloud.LoadBalancerCreateOptsTarget{{
			Type:          hcloud.LoadBalancerTargetTypeLabelSelector,
			LabelSelector: hcloud.LoadBalancerCreateOptsTargetLabelSelector{Selector: spec.TargetSelector},
			UsePrivateIP:  hcloud.Ptr(true),
		}},
	}
	for _, s := range spec.Services {
		opts.Services = append(opts.Services, hcloud.LoadBalancerCreateOptsService{
			Protocol:        hcloud.LoadBalancerServiceProtocolTCP,
			ListenPort:      hcloud.Ptr(s.ListenPort),
			DestinationPort: hcloud.Ptr(s.DestinationPort),
			Proxyprotocol:   hcloud.Ptr(s.ProxyProtocol),
			HealthCheck: &hcloud.LoadBalancerCreateOptsServiceHealthCheck{
				Protocol: hcloud.LoadBalancerServiceProtocolTCP,
				Port:     hcloud.Ptr(s.DestinationPort),
				Interval: hcloud.Ptr(healthCheckInterval),
				Timeout:  hcloud.Ptr(healthCheckTimeout),
				Retries:  hcloud.Ptr(healthCheckRetries),
			},
		})
	}
	res, _, err := a.client.LoadBalancer.Create(ctx, opts)
	if err != nil {
		return hcLoadBalancer{}, fmt.Errorf("creating load balancer %s: %w", spec.Name, err)
	}
	if err := a.wait(ctx, res.Action, "load balancer "+spec.Name+" to be created"); err != nil {
		return hcLoadBalancer{}, err
	}
	return loadBalancerFromHcloud(res.LoadBalancer), nil
}

func (a *hcloudAPI) DeleteLoadBalancer(ctx context.Context, id int64) error {
	_, err := a.client.LoadBalancer.Delete(ctx, &hcloud.LoadBalancer{ID: id})
	if err != nil && !hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
		return fmt.Errorf("deleting load balancer %d: %w", id, err)
	}
	return nil
}

func (a *hcloudAPI) ListSnapshots(ctx context.Context, labelSelector string) ([]hcSnapshot, error) {
	images, err := a.client.Image.AllWithOpts(ctx, hcloud.ImageListOpts{
		ListOpts: listOpts(labelSelector), Type: []hcloud.ImageType{hcloud.ImageTypeSnapshot},
	})
	if err != nil {
		return nil, fmt.Errorf("listing snapshots: %w", err)
	}
	out := make([]hcSnapshot, 0, len(images))
	for _, img := range images {
		out = append(out, snapshotFromHcloud(img))
	}
	return out, nil
}

func (a *hcloudAPI) CreateSnapshot(ctx context.Context, serverID int64, spec snapshotSpec) (hcSnapshot, error) {
	res, _, err := a.client.Server.CreateImage(ctx, &hcloud.Server{ID: serverID}, &hcloud.ServerCreateImageOpts{
		Type: hcloud.ImageTypeSnapshot, Description: hcloud.Ptr(spec.Description), Labels: spec.Labels,
	})
	if err != nil {
		return hcSnapshot{}, fmt.Errorf("creating a snapshot of server %d: %w", serverID, err)
	}
	if err := a.wait(ctx, res.Action, fmt.Sprintf("the snapshot of server %d", serverID)); err != nil {
		return hcSnapshot{}, err
	}
	return snapshotFromHcloud(res.Image), nil
}

func (a *hcloudAPI) DeleteSnapshot(ctx context.Context, id int64) error {
	_, err := a.client.Image.Delete(ctx, &hcloud.Image{ID: id})
	if err != nil && !hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
		return fmt.Errorf("deleting snapshot %d: %w", id, err)
	}
	return nil
}

func parseCIDR(cidr string) (*net.IPNet, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parsing IP range %q: %w", cidr, err)
	}
	return ipNet, nil
}

func serverFromHcloud(s *hcloud.Server) hcServer {
	out := hcServer{ID: s.ID, Name: s.Name, Status: string(s.Status), Labels: s.Labels, PrivateIPs: map[int64]string{}, Created: s.Created}
	if s.Location != nil {
		out.Location = s.Location.Name
	}
	if s.ServerType != nil {
		out.ServerType = s.ServerType.Name
	}
	if s.Image != nil {
		out.ImageID = s.Image.ID
	}
	if s.PublicNet.IPv4.IP != nil && !s.PublicNet.IPv4.IsUnspecified() {
		out.PublicIPv4 = s.PublicNet.IPv4.IP.String()
	}
	for _, pn := range s.PrivateNet {
		if pn.Network != nil && pn.IP != nil {
			out.PrivateIPs[pn.Network.ID] = pn.IP.String()
		}
	}
	for _, fw := range s.PublicNet.Firewalls {
		if fw != nil {
			out.FirewallIDs = append(out.FirewallIDs, fw.Firewall.ID)
		}
	}
	return out
}

func networkFromHcloud(n *hcloud.Network) hcNetwork {
	out := hcNetwork{ID: n.ID, Name: n.Name, Labels: n.Labels}
	if n.IPRange != nil {
		out.IPRange = n.IPRange.String()
	}
	for _, r := range n.Routes {
		route := hcRoute{Gateway: r.Gateway.String()}
		if r.Destination != nil {
			route.Destination = r.Destination.String()
		}
		out.Routes = append(out.Routes, route)
	}
	return out
}

func loadBalancerFromHcloud(lb *hcloud.LoadBalancer) hcLoadBalancer {
	out := hcLoadBalancer{ID: lb.ID, Name: lb.Name, Labels: lb.Labels, PublicInterface: lb.PublicNet.Enabled, PrivateIPs: map[int64]string{}}
	if lb.Location != nil {
		out.Location = lb.Location.Name
	}
	if lb.PublicNet.Enabled && lb.PublicNet.IPv4.IP != nil {
		out.PublicIPv4 = lb.PublicNet.IPv4.IP.String()
	}
	for _, pn := range lb.PrivateNet {
		if pn.Network != nil && pn.IP != nil {
			out.PrivateIPs[pn.Network.ID] = pn.IP.String()
		}
	}
	for _, s := range lb.Services {
		out.Services = append(out.Services, lbService{ListenPort: s.ListenPort, DestinationPort: s.DestinationPort, ProxyProtocol: s.Proxyprotocol})
	}
	for _, t := range lb.Targets {
		if t.Type == hcloud.LoadBalancerTargetTypeLabelSelector && t.LabelSelector != nil {
			out.TargetSelectors = append(out.TargetSelectors, t.LabelSelector.Selector)
		}
	}
	return out
}

func snapshotFromHcloud(img *hcloud.Image) hcSnapshot {
	return hcSnapshot{ID: img.ID, Description: img.Description, Status: string(img.Status), Labels: img.Labels}
}
