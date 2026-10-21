package main

// The cluster's keys exist only in this call's memory, the builder's user data and the snapshot it becomes, so
// the gateway and the builder are made in the one call that mints them.

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"runtime/debug"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
)

const (
	kitModule = "github.com/mitstack-eu/talos-cluster-kit"

	gatewayCAValidity   = 10 * 365 * 24 * time.Hour
	gatewayCertValidity = 365 * 24 * time.Hour
	// adminTalosconfigTTL covers the build, the snapshot and the node's first boot, after which the gateway's copy is worthless.
	adminTalosconfigTTL = 2 * time.Hour
)

// installInputs are what one install renders from, besides the keys it mints.
type installInputs struct {
	subID, appName string
	repo           clusterRepo
	apiAddress     netip.Addr
	logs           components.LogExport
	now            time.Time
}

// installation is what one install renders: the two servers' user data and the enrolment token's digest.
type installation struct {
	gatewayUserData, builderUserData string
	enrolmentDigest                  string
}

func (r *blockResource) install(ctx context.Context, c convergence, site clusterSite) error {
	in, err := readInstallInputs(c.req, site, r.clock())
	if err != nil {
		return err
	}
	inst, err := renderInstallation(in)
	if err != nil {
		return err
	}
	if err := createGateway(ctx, c, site, inst); err != nil {
		return err
	}
	_, err = c.api.CreateServer(ctx, serverSpec{
		Name:        resourceName(c.req.params.appName, c.req.subID, roleBuilder),
		ServerType:  c.req.params.serverType,
		Location:    c.req.params.location,
		Image:       builderImage,
		UserData:    inst.builderUserData,
		Labels:      ownerLabels(c.req.subID, roleBuilder),
		NetworkIDs:  []int64{site.network.ID},
		FirewallIDs: []int64{site.firewall.ID},
	})
	if err != nil {
		return fmt.Errorf("creating the image builder: %w", err)
	}
	return nil
}

func readInstallInputs(req ensureRequest, site clusterSite, now time.Time) (installInputs, error) {
	apiAddress, err := netip.ParseAddr(site.apiAddress)
	if err != nil {
		return installInputs{}, fmt.Errorf("the API load balancer has no address on network %d: %w", site.network.ID, err)
	}
	return installInputs{
		subID: req.subID, appName: req.params.appName, repo: req.gh.repo,
		apiAddress: apiAddress, logs: req.logs, now: now,
	}, nil
}

// createGateway attaches the gateway at its fixed address before its first boot, since its user data names that address.
func createGateway(ctx context.Context, c convergence, site clusterSite, inst installation) error {
	labels := ownerLabels(c.req.subID, roleGateway)
	labels[keyEnrolmentSHA256] = inst.enrolmentDigest
	gw, err := c.api.CreateServer(ctx, serverSpec{
		Name:        resourceName(c.req.params.appName, c.req.subID, roleGateway),
		ServerType:  gatewayServerType,
		Location:    c.req.params.location,
		Image:       gatewayImage,
		UserData:    inst.gatewayUserData,
		Labels:      labels,
		FirewallIDs: []int64{site.firewall.ID},
		PublicIPv4:  true,
		Stopped:     true,
	})
	if err != nil {
		return fmt.Errorf("creating the gateway: %w", err)
	}
	if err := c.api.AttachServerToNetwork(ctx, gw.ID, site.network.ID, gatewayAddress); err != nil {
		return err
	}
	return c.api.PowerOnServer(ctx, gw.ID)
}

// renderInstallation mints the cluster's keys, the gateway's CA and the enrolment token, and renders both servers' user data.
func renderInstallation(in installInputs) (installation, error) {
	secrets, err := nodeconfig.NewSecrets(in.now)
	if err != nil {
		return installation{}, err
	}
	gatewayCA, err := gateway.NewCA(in.now, gatewayCAValidity)
	if err != nil {
		return installation{}, err
	}
	token, err := mintEnrolmentToken()
	if err != nil {
		return installation{}, err
	}
	set, err := clusterComponents(manifestInputs{subID: in.subID, repoURL: in.repo.url(), enrolmentToken: token})
	if err != nil {
		return installation{}, err
	}
	gatewayAddr := netip.MustParseAddr(gatewayAddress)
	config, err := nodeconfig.Render(nodeconfig.Cluster{
		Name:           clusterName(in.appName),
		SubscriptionID: in.subID,
		Profile:        nodeconfig.HCloud,
		Components:     set,
		Secrets:        secrets,
		Network:        netip.MustParsePrefix(networkIPRange),
		APIAddresses:   []netip.Addr{in.apiAddress},
		Gateways:       []netip.Addr{gatewayAddr},
		GatewayCA:      gatewayCA.Cert,
		Logs:           in.logs,
	}, nodeconfig.Node{})
	if err != nil {
		return installation{}, err
	}
	gatewayData, err := renderGateway(in, gatewayCA, set, secrets)
	if err != nil {
		return installation{}, err
	}
	builderData, err := builderUserData(config)
	if err != nil {
		return installation{}, err
	}
	return installation{gatewayUserData: gatewayData, builderUserData: builderData, enrolmentDigest: enrolmentTokenDigest(token)}, nil
}

func renderGateway(in installInputs, ca gateway.CA, set components.Set, secrets *nodeconfig.Secrets) (string, error) {
	gatewayAddr := netip.MustParseAddr(gatewayAddress)
	tls, err := ca.Issue(gatewayAddr, in.now, gatewayCertValidity)
	if err != nil {
		return "", err
	}
	release, err := kitRelease()
	if err != nil {
		return "", err
	}
	kitPart, err := gateway.Render(gateway.Inputs{
		Address:         gatewayAddr,
		Network:         netip.MustParsePrefix(networkIPRange),
		TLS:             tls,
		Components:      set,
		ManifestBaseURL: gateway.ReleaseURL(release),
		Logs:            in.logs,
	})
	if err != nil {
		return "", err
	}
	// The address only fills the talosconfig's defaults, since the bootstrap names the node it finds on every call.
	talosconfig, err := secrets.AdminTalosconfig(in.apiAddress, adminTalosconfigTTL, in.now)
	if err != nil {
		return "", err
	}
	return gatewayUserData(kitPart, firstNodeBootstrap{talosconfig: talosconfig, deadline: in.now.Add(adminTalosconfigTTL)})
}

// kitRelease is the talos-cluster-kit tag this binary is built against, whose files the gateway fetches.
func kitRelease() (string, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", errors.New("the binary carries no build information, so the talos-cluster-kit release is unknown")
	}
	for _, dep := range info.Deps {
		if dep.Path == kitModule {
			return dep.Version, nil
		}
	}
	return "", fmt.Errorf("the binary is not built against %s", kitModule)
}

func clusterName(appName string) string {
	if name := hostnamePart(appName); name != "" {
		return name
	}
	return "talos"
}
