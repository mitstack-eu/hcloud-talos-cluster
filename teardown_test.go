package main

// Unsubscribe deletes every resource the block made, the snapshot that holds every key of the cluster among them.

import (
	"testing"

	"github.com/mitstack-eu/operator-kit/v2/contract"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnsubscribeDeletes_EveryResourceItMadeTheSnapshotIncluded(t *testing.T) {
	e := newEnv(t)
	require.Equal(t, contract.Present, e.subscribe().State)

	_, err := e.block.Delete(e.ctx(), e.desired())

	require.NoError(t, err)
	assert.Zero(t, e.hetzner.count(), "Hetzner resources under the subscription's labels after unsubscribe")
	assert.False(t, e.github.exists(e.repo()), "the cluster repository after unsubscribe")
	assert.Equal(t, contract.Absent, e.observe().State)
}

func TestUnsubscribe_MidBuildDeletesTheBuilderAndTheGateway(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.ensure())

	_, err := e.block.Delete(e.ctx(), e.desired())

	require.NoError(t, err)
	assert.Zero(t, e.hetzner.count(), "Hetzner resources after an unsubscribe during the build")
}

func TestUnsubscribe_KeepsARepositoryWithoutTheSubscriptionsMark(t *testing.T) {
	e := newEnv(t)
	e.subscribe()
	e.github.repos[e.repo()] = []string{"someone-elses"}

	_, err := e.block.Delete(e.ctx(), e.desired())

	require.NoError(t, err)
	assert.True(t, e.github.exists(e.repo()), "an unmarked repository is kept")
	assert.Zero(t, e.hetzner.count(), "Hetzner resources")
}

func TestUnsubscribe_WithoutGitHubDeletesTheHetznerResourcesAndRefusesNamingTheRepository(t *testing.T) {
	e := newEnv(t)
	e.subscribe()
	d := e.desired()
	d.Secrets["github"] = map[string]string{}

	_, err := e.block.Delete(e.ctx(), d)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "repository-left-in-place")
	assert.Zero(t, e.hetzner.count(), "Hetzner resources")
}
