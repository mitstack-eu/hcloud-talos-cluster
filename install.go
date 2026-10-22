package main

// The cluster's keys exist only in this call's memory, the builder's user data and the snapshot it becomes, so
// the gateway and the builder are made in the one call that mints them.

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/mitstack-eu/operator-kit/v3/handler"
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
	// apiAddresses are the private load balancers', the first of them the control-plane endpoint.
	apiAddresses []netip.Addr
	now          time.Time
}

// installation is what one install renders: the two servers' user data and the enrolment token's digest.
type installation struct {
	gatewayUserData, builderUserData string
	enrolmentDigest                  string
}

// install creates both servers stopped and off the network, since Hetzner gives a server joining with no address the
// first free one, which would be the gateway's own.
func install(ctx context.Context, c convergence, site clusterSite) error {
	in, err := readInstallInputs(c.req, site, clock())
	if err != nil {
		return err
	}
	inst, err := renderInstallation(in)
	if err != nil {
		return err
	}
	gatewayType, err := orderableGatewayType(ctx, c)
	if err != nil {
		return err
	}
	gatewayLabels := ownerLabels(c.req.subID, roleGateway)
	gatewayLabels[keyEnrolmentSHA256] = inst.enrolmentDigest
	_, err = c.api.CreateServers(ctx, serverSpec{
		Name:        resourceName(c.req.params.appName, c.req.subID, roleGateway),
		ServerType:  gatewayType,
		Location:    gatewayLocation,
		Image:       gatewayImage,
		UserData:    inst.gatewayUserData,
		Labels:      gatewayLabels,
		FirewallIDs: []int64{site.firewall.ID},
		PublicIPv4:  true,
		Stopped:     true,
	}, serverSpec{
		Name:        resourceName(c.req.params.appName, c.req.subID, roleBuilder),
		ServerType:  c.req.params.serverType,
		Location:    gatewayLocation,
		Image:       builderImage,
		UserData:    inst.builderUserData,
		Labels:      ownerLabels(c.req.subID, roleBuilder),
		FirewallIDs: []int64{site.firewall.ID},
		Stopped:     true,
	})
	if err != nil {
		return rollBackInstall(ctx, c, fmt.Errorf("creating the gateway and the image builder: %w", err))
	}
	return nil
}

// orderableGatewayType is the first of gatewayServerTypes Hetzner reads as orderable in the gateway's location.
func orderableGatewayType(ctx context.Context, c convergence) (string, error) {
	available, err := c.api.AvailableServerTypes(ctx, gatewayLocation)
	if err != nil {
		return "", err
	}
	for _, t := range gatewayServerTypes {
		if slices.Contains(available, t) {
			return t, nil
		}
	}
	return "", handler.Refuse("server-unavailable",
		"Hetzner can order none of the gateway's server types %s in %s now; subscribe again later",
		strings.Join(gatewayServerTypes, ", "), gatewayLocation)
}

// rollBackInstall deletes whatever gateway and builder a failed install made, since the keys they were made with
// are gone with this call, so the next Ensure installs anew.
func rollBackInstall(ctx context.Context, c convergence, cause error) error {
	errs := []error{cause}
	for _, r := range []role{roleGateway, roleBuilder} {
		servers, err := c.api.ListServers(ctx, roleSelector(c.req.subID, r))
		if err != nil {
			errs = append(errs, fmt.Errorf("listing the %s of the failed install: %w", r, err))
			continue
		}
		for _, s := range servers {
			if err := c.api.DeleteServer(ctx, s.ID); err != nil {
				errs = append(errs, fmt.Errorf("deleting the %s %d of the failed install: %w", r, s.ID, err))
			}
		}
	}
	return errors.Join(errs...)
}

func readInstallInputs(req ensureRequest, site clusterSite, now time.Time) (installInputs, error) {
	in := installInputs{subID: req.subID, appName: req.params.appName, repo: req.gh.repo, now: now}
	for _, addr := range site.apiAddresses {
		apiAddress, err := netip.ParseAddr(addr)
		if err != nil {
			return installInputs{}, fmt.Errorf("a private load balancer has no address on network %d: %w", site.network.ID, err)
		}
		in.apiAddresses = append(in.apiAddresses, apiAddress)
	}
	return in, nil
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
		APIAddresses:   in.apiAddresses,
		Gateways:       []netip.Addr{gatewayAddr},
		GatewayCA:      gatewayCA.Cert,
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
	})
	if err != nil {
		return "", err
	}
	// The address only fills the talosconfig's defaults, since the bootstrap names the node it finds on every call.
	talosconfig, err := secrets.AdminTalosconfig(in.apiAddresses[0], adminTalosconfigTTL, in.now)
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
