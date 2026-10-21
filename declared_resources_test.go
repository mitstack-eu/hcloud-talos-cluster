package main

// The block holds the kit's block guards, served as the engine reaches it, over fakes whose builder finishes at once.

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mitstack-eu/operator-kit/v2/contract"
	"github.com/mitstack-eu/operator-kit/v2/kittest"
	"github.com/mitstack-eu/operator-kit/v2/observe"
	"github.com/stretchr/testify/require"
)

//go:embed testdata/type-snapshot.json
var typeSnapshotJSON []byte

// clusterProvider lists what carries the subscription's labels and topics, and loses a declared resource from under the block.
type clusterProvider struct{ e *env }

func (p clusterProvider) ListTagged(ctx context.Context, subID string) ([]observe.Tagged, error) {
	st, err := listCluster(ctx, p.e.hetzner, subID)
	if err != nil {
		return nil, err
	}
	var tagged []observe.Tagged
	add := func(typ string, ids []int64) {
		for _, id := range ids {
			tagged = append(tagged, observe.Tagged{Type: typ, ID: strconv.FormatInt(id, 10)})
		}
	}
	add(resNetwork, idsOf(st.networks, func(n hcNetwork) int64 { return n.ID }))
	add(resFirewall, idsOf(st.firewalls, func(f hcFirewall) int64 { return f.ID }))
	for _, lbs := range st.loadBalancers {
		add(typeLoadBalancer, idsOf(lbs, func(lb hcLoadBalancer) int64 { return lb.ID }))
	}
	for _, servers := range st.servers {
		add(typeServer, idsOf(servers, func(s hcServer) int64 { return s.ID }))
	}
	// The snapshot is the cluster's own part, which the cluster's absence statement names.
	add(resCluster, idsOf(st.snapshots, func(s hcSnapshot) int64 { return s.ID }))
	if carriesMark(p.e.github.topics(p.e.repo()), subID) {
		tagged = append(tagged, observe.Tagged{Type: typeRepository, ID: p.e.repo()}, observe.Tagged{Type: resSeedCommit, ID: p.e.repo()})
	}
	return tagged, nil
}

func (p clusterProvider) Lose(ctx context.Context, r contract.CreatedResource) error {
	id := func(key string) int64 {
		n, _ := strconv.ParseInt(r.Identity[key], 10, 64)
		return n
	}
	h := p.e.hetzner
	switch r.Type {
	case resNetwork:
		return h.DeleteNetwork(ctx, id("networkId"))
	case resFirewall:
		return h.DeleteFirewall(ctx, id("firewallId"))
	case typeLoadBalancer:
		return h.DeleteLoadBalancer(ctx, id("loadBalancerId"))
	case typeServer:
		return h.DeleteServer(ctx, id("serverId"))
	case typeRepository:
		p.e.github.remove(r.Identity["repo"])
		return nil
	case resSeedCommit:
		p.e.github.removeFile(r.Identity["repo"] + "/contents/" + seedMarkerFile)
		return nil
	case resCluster:
		snaps, err := h.ListSnapshots(ctx, subscriptionSelector(testSub))
		for _, s := range snaps {
			err = h.DeleteSnapshot(ctx, s.ID)
		}
		return err
	}
	return fmt.Errorf("no declared resource is of type %q", r.Type)
}

func blockUnderTest(t *testing.T) kittest.Block {
	t.Helper()
	e := newEnv(t)
	e.hetzner.instantBuild = true
	e.reports.report(testStart.Add(time.Hour), true)
	snap, err := contract.ParseTypeSnapshot(typeSnapshotJSON)
	require.NoError(t, err)
	d := e.desired()
	d.BlockID, d.OrgID, d.AppID = blockID, "org_0123456789ab", "app_0123456789ab"
	return kittest.Block{
		Manifest:  manifestYAML,
		Types:     snap,
		Handler:   e.block,
		Observers: observers(),
		Provider:  clusterProvider{e: e},
		Desired:   d,
		Upgrade:   &d,
	}
}

// logRecorder keeps what AssertBlock logs about the guards it ran.
type logRecorder struct {
	testing.TB
	logs []string
}

func (r *logRecorder) Logf(format string, args ...any) {
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
	r.TB.Logf(format, args...)
}

func TestDeclaredResources(t *testing.T) {
	rec := &logRecorder{TB: t}

	kittest.AssertBlock(rec, blockUnderTest(t))

	ran := strings.Join(rec.logs, "\n")
	for _, guard := range []string{"tagging", "observe-asks-the-provider", "replacement", "absence-after-unsubscribe", "required-outputs"} {
		require.Contains(t, ran, guard, "guard %q did not run", guard)
	}
}
