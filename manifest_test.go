package main

// The manifest provides hetzner.kubernetes-cluster and the handler serves an observer for every resource it declares.

import (
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

func TestHandler_BuildsFromTheManifestAndObservers(t *testing.T) {
	_, err := handler.New(handlerConfig("https://api.mitstack.test/.well-known/jwks.json", nil))

	require.NoError(t, err)
}
