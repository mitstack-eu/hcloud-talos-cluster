package main

// One subscribe reaches the running node through the engine's re-drives, each call waiting on one Hetzner action at most.

import (
	"errors"
	"maps"
	"net/http"
	"testing"
	"time"

	"github.com/mitstack-eu/operator-kit/v3/contract"
	"github.com/mitstack-eu/operator-kit/v3/kittest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscribe_OneSubscribeCarriesTheClusterToItsNodeThroughTheEnginesRedrives(t *testing.T) {
	e := newEnv(t)
	p := e.engine()

	final := p.settle(nil)

	require.Equal(t, http.StatusOK, final.status, "the last answer: %s", final.detail)
	require.False(t, final.pending, "the last answer: %s", final.detail)
	assert.Len(t, e.hetzner.byRole(t, roleNode), 1, "nodes")
	assert.Len(t, e.hetzner.byRole(t, roleGateway), 1, "gateways")
	assert.Empty(t, e.hetzner.byRole(t, roleBuilder), "a builder outlived its snapshot")
	var subscribes int
	for _, a := range p.posted {
		if a.op == "subscribe" {
			subscribes++
			assert.True(t, a.pending, "a subscribe answered settled while the cluster was unfinished: %s", a.detail)
		}
	}
	assert.Greater(t, subscribes, 1, "subscribes: the engine never re-drove one")
}

func TestSubscribe_EachCallWaitsOnOneHetznerActionAtMostAndHoldsNoPoll(t *testing.T) {
	e := newEnv(t)
	p := e.engine()

	p.settle(nil)

	for _, a := range p.posted {
		assert.LessOrEqual(t, a.waits, 1, "the %s answering %q waited on %d Hetzner actions", a.op, a.detail, a.waits)
	}
	assert.Zero(t, p.sleeps, "the handler slept inside a call, holding it open")
}

func TestValidate_AsksForARedriveExactlyWhenASubscribeWouldTakeAStep(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.ensure())

	for start := e.now; e.observe().State != contract.Present && e.now.Sub(start) < settleBudget; e.now = e.now.Add(time.Minute) {
		e.progress()
		v := e.engine().post("validate")
		before := e.changes()
		err := e.ensure()

		assert.Equal(t, e.changes() != before || err != nil, v.redrive, "validate at %s: %s", e.now.Format(time.Kitchen), v.detail)
	}
	assert.Equal(t, contract.Present, e.observe().State)
}

func TestSubscribe_ARedriveSentTwiceAtEachSignalMakesEveryResourceOnce(t *testing.T) {
	e := newEnv(t)
	p := e.engine()
	require.True(t, p.post("subscribe").pending)

	for e.observe().State != contract.Present && e.now.Sub(testStart) < settleBudget {
		e.now = e.now.Add(time.Minute)
		e.progress()
		if p.post("validate").redrive {
			p.post("subscribe")
			p.post("subscribe")
		}
	}

	assert.Len(t, e.hetzner.byRole(t, roleNode), 1, "nodes")
	assert.Len(t, e.hetzner.byRole(t, roleGateway), 1, "gateways")
	assert.Empty(t, e.hetzner.byRole(t, roleBuilder), "builders")
	snaps, err := e.hetzner.ListSnapshots(e.ctx(), subscriptionSelector(testSub))
	require.NoError(t, err)
	assert.Len(t, snaps, 1, "snapshots")
	assert.Equal(t, 7, e.hetzner.count(), "two servers, two load balancers, a network, a firewall and a snapshot")
}

func TestVerbSemantics_ARedrivenSubscribeResumesTheBuildAndChangesNothingMadeBefore(t *testing.T) {
	e := newEnv(t)
	p := e.engine()

	kittest.AssertVerbSemantics(t, kittest.VerbSeams{
		Subscribe: func() (int, error) {
			err := p.settle(nil).err()
			return e.hetzner.count(), err
		},
		Unsubscribe: func() (int, error) {
			err := p.post("unsubscribe").err()
			return e.hetzner.count(), err
		},
		UnsubscribeWhenAbsent: func() error { return p.post("unsubscribe").err() },
		Upgrade: func() (bool, error) {
			before := e.hetzner.resources()
			err := p.post("upgrade").err()
			return maps.Equal(before, e.hetzner.resources()), err
		},
		ValidateWhileConverging: func() (bool, bool) {
			reports := e.reports.reports
			e.reports.reports = nil
			defer func() { e.reports.reports = reports }()
			v := p.post("validate")
			return v.pending, v.err() != nil
		},
		ValidateWhenDead: func() (bool, bool) {
			e.hetzner.failNext = errors.New("hetzner answered 401: the token was revoked")
			v := p.post("validate")
			return v.pending, v.err() != nil
		},
		Redrive: func() (kittest.RedriveRun, error) {
			e.reports.reports = nil
			if err := p.post("unsubscribe").err(); err != nil {
				return kittest.RedriveRun{}, err
			}
			e.buildTakes = 0
			first := p.settle(func() bool { return e.building() })
			e.hetzner.powerOffBuilder(t)
			v := p.post("validate")
			before := e.hetzner.resources()
			last := p.settle(nil)
			changed := 0
			for id, was := range before {
				if e.hetzner.resources()[id] != was {
					changed++
				}
			}
			return kittest.RedriveRun{FirstPending: first.pending, Redrive: v.pending && v.redrive, Settled: !last.pending, Changed: changed}, last.err()
		},
	})
}
