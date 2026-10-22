package main

// The manifest provides hetzner.kubernetes-cluster, an observer serves each resource it declares, and every server type is one the platform allows.

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/mitstack-eu/operator-kit/v3/handler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

type parsedManifest struct {
	Metadata struct{ ID string } `yaml:"metadata"`
	Spec     struct {
		Provides  []string `yaml:"provides"`
		Resources []any    `yaml:"resources"`
	} `yaml:"spec"`
}

// declaredTypes maps each declared resource's name to its type; a resource declared by its type alone is named by it.
func (m parsedManifest) declaredTypes() map[string]string {
	declared := map[string]string{}
	for _, r := range m.Spec.Resources {
		switch v := r.(type) {
		case string:
			declared[v] = v
		case map[string]any:
			declared[v["name"].(string)] = v["type"].(string)
		}
	}
	return declared
}

func parseManifest(t *testing.T) parsedManifest {
	t.Helper()
	var m parsedManifest
	require.NoError(t, yaml.Unmarshal(manifestYAML, &m))
	return m
}

func TestManifest_ProvidesTheKubernetesClusterType(t *testing.T) {
	m := parseManifest(t)

	declared, observed := m.declaredTypes(), observers()

	assert.Equal(t, blockID, m.Metadata.ID)
	assert.Equal(t, []string{"hetzner.kubernetes-cluster"}, m.Spec.Provides)
	for name := range declared {
		assert.Contains(t, observed, name, "the declared resource %s has no observer", name)
	}
	for name := range observed {
		assert.Contains(t, declared, name, "the observer %s serves no declared resource", name)
	}
}

func TestManifest_DeclaresANodeAndAPublicAndAPrivateLoadBalancerInEachLocation(t *testing.T) {
	want := map[string]string{
		"hetzner.network": "hetzner.network", "hetzner.firewall": "hetzner.firewall", "gateway": "hetzner.server",
		"github.repository": "github.gitops-repository", "github.gitops-commit": "github.gitops-commit",
		"hetzner.kubernetes-cluster": "hetzner.kubernetes-cluster",
	}
	for _, location := range clusterLocationNames {
		want["node-"+location] = "hetzner.server"
		want["api-load-balancer-"+location] = "hetzner.load-balancer"
		want["ingress-load-balancer-"+location] = "hetzner.load-balancer"
	}

	declared := parseManifest(t).declaredTypes()

	assert.Equal(t, want, declared, "the declared resources and their types")
}

func TestHandler_BuildsFromTheManifestAndObservers(t *testing.T) {
	_, err := handler.New(handlerConfig("https://api.mitstack.test/.well-known/jwks.json", nil))

	require.NoError(t, err)
}

func TestReadParams_EveryServerTypeTheBlockCreatesIsOneTheHetznerServerTypeAllows(t *testing.T) {
	var snap struct {
		Types []struct {
			ID     string `json:"id"`
			Inputs []struct {
				Name          string   `json:"name"`
				AllowedValues []string `json:"allowedValues"`
			} `json:"inputs"`
		} `json:"types"`
	}
	require.NoError(t, json.Unmarshal(typeSnapshotJSON, &snap))
	var allowed []string
	for _, typ := range snap.Types {
		for _, in := range typ.Inputs {
			if typ.ID == typeServer && in.Name == "serverType" {
				allowed = in.AllowedValues
			}
		}
	}
	require.NotEmpty(t, allowed, "the %s type's serverType input lists no allowed value", typeServer)

	for _, st := range slices.Concat(gatewayServerTypes, []string{defaultServerType}, allowedServerTypes) {
		assert.Contains(t, allowed, st, "the block takes server type %s, which the platform refuses at subscribe", st)
	}
}
