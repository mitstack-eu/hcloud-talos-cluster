package main

import (
	"context"
	"fmt"
	"log"

	"github.com/mitstack-eu/operator-kit/v2/contract"
	"github.com/mitstack-eu/operator-kit/v2/handler"
	"github.com/mitstack-eu/operator-kit/v2/safe"
)

// Delete deletes every labelled Hetzner resource, the snapshot that holds every key of the cluster among them, even
// without a usable GitHub connection, and then refuses, naming the repository it left in place.
func (r *blockResource) Delete(ctx context.Context, d contract.Desired) (map[string]string, error) {
	token, err := hetznerToken(d)
	if err != nil {
		return nil, err
	}
	subID, err := subscriptionID(d)
	if err != nil {
		return nil, err
	}
	gh, ghErr := dialClusterRepository(d)
	if err := deleteLabelled(ctx, newHetznerAPI(token), subID); err != nil {
		return nil, err
	}
	if ghErr != nil {
		return nil, handler.Refuse("repository-left-in-place",
			"the cluster's Hetzner servers, load balancers, snapshot, firewall and network are deleted, but the cluster repository was left in place: %s",
			refusalDetail(ghErr))
	}
	return nil, deleteMarkedRepository(ctx, gh, d)
}

func dialClusterRepository(d contract.Desired) (*githubAccess, error) {
	appName := d.Params["appName"]
	if appName == "" {
		return nil, handler.Refuse("missing-param", `the app name is required to name the cluster repository: params["appName"] is empty`)
	}
	return dialGitHub(d, appName)
}

// deleteLabelled deletes servers and load balancers before the firewall and network they use, and the snapshot with them.
func deleteLabelled(ctx context.Context, api hetznerAPI, subID string) error {
	st, err := listCluster(ctx, api, subID)
	if err != nil {
		return err
	}
	for _, servers := range st.servers {
		for _, s := range servers {
			if err := api.DeleteServer(ctx, s.ID); err != nil {
				return fmt.Errorf("deleting the cluster server %d: %w", s.ID, err)
			}
		}
	}
	for _, lbs := range st.loadBalancers {
		for _, lb := range lbs {
			if err := api.DeleteLoadBalancer(ctx, lb.ID); err != nil {
				return fmt.Errorf("deleting the cluster load balancer %d: %w", lb.ID, err)
			}
		}
	}
	for _, snap := range st.snapshots {
		if err := api.DeleteSnapshot(ctx, snap.ID); err != nil {
			return fmt.Errorf("deleting the cluster snapshot %d: %w", snap.ID, err)
		}
	}
	for _, fw := range st.firewalls {
		if err := api.DeleteFirewall(ctx, fw.ID); err != nil {
			return fmt.Errorf("deleting the cluster firewall %d: %w", fw.ID, err)
		}
	}
	for _, n := range st.networks {
		if err := api.DeleteNetwork(ctx, n.ID); err != nil {
			return fmt.Errorf("deleting the cluster network %d: %w", n.ID, err)
		}
	}
	return nil
}

// deleteMarkedRepository reads the mark just before the delete, and keeps a repository without it untouched.
func deleteMarkedRepository(ctx context.Context, gh *githubAccess, d contract.Desired) error {
	repo, err := gh.readRepo(ctx, d.SubscriptionID)
	if err != nil {
		return err
	}
	switch {
	case !repo.exists:
		return nil
	case !repo.marked:
		log.Print(safe.LogSafe(fmt.Sprintf("%s: subscription %s: kept the repository %s, which does not carry this subscription's mark",
			blockID, d.SubscriptionID, gh.repo.fullName()), requestSecrets(d)...))
		return nil
	}
	return gh.deleteRepo(ctx)
}

func requestSecrets(d contract.Desired) []string {
	var out []string
	for _, group := range d.Secrets {
		for _, v := range group {
			out = append(out, v)
		}
	}
	for _, v := range d.SecretInputs {
		out = append(out, v)
	}
	return out
}
