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

func TestManifest_ProvidesTheKubernetesClusterType(t *testing.T) {
	var m struct {
		Metadata struct{ ID string } `yaml:"metadata"`
		Spec     struct {
			Provides  []string `yaml:"provides"`
			Resources []any    `yaml:"resources"`
		} `yaml:"spec"`
	}

	require.NoError(t, yaml.Unmarshal(manifestYAML, &m))

	assert.Equal(t, blockID, m.Metadata.ID)
	assert.Equal(t, []string{"hetzner.kubernetes-cluster"}, m.Spec.Provides)
	var names []string
	for _, r := range m.Spec.Resources {
		switch v := r.(type) {
		case string:
			names = append(names, v)
		case map[string]any:
			names = append(names, v["name"].(string))
		}
	}
	observed := observers()
	for _, name := range names {
		assert.Contains(t, observed, name, "the declared resource %s has no observer", name)
	}
	for name := range observed {
		assert.True(t, slices.Contains(names, name), "the observer %s serves no declared resource", name)
	}
}

func TestManifest_DeclaresANodeAndAPublicAndAPrivateLoadBalancerInEachLocation(t *testing.T) {
	var m struct {
		Spec struct {
			Resources []any `yaml:"resources"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(manifestYAML, &m))
	declared := map[string]string{}
	for _, r := range m.Spec.Resources {
		if v, ok := r.(map[string]any); ok {
			declared[v["name"].(string)] = v["type"].(string)
		}
	}

	want := map[string]string{"gateway": "hetzner.server", "github.repository": "github.gitops-repository"}
	for _, location := range clusterLocationNames {
		want["node-"+location] = "hetzner.server"
		want["api-load-balancer-"+location] = "hetzner.load-balancer"
		want["ingress-load-balancer-"+location] = "hetzner.load-balancer"
	}
	assert.Equal(t, want, declared, "the named resources and their types")
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
