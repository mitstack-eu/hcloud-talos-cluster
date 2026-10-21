package main

// The adapter's requests on the wire: a node with no user data and no public address, a private load balancer with no public interface.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const finishedAction = `{"id": 1, "command": "x", "status": "success", "progress": 100, "resources": []}`

// recordHetzner answers each path with its canned body and records each request body by method and path.
func recordHetzner(t *testing.T, answers map[string]string) map[string]map[string]any {
	t.Helper()
	bodies := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies[r.Method+" "+r.URL.Path] = body
		answer, ok := answers[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error": {"code": "not_found", "message": "not found"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	swap(t, &hetznerEndpoint, srv.URL)
	return bodies
}

func TestHetznerAdapter_CreatesTheNodeWithNoUserDataAndNoPublicAddress(t *testing.T) {
	bodies := recordHetzner(t, map[string]string{
		"POST /servers": `{"server": {"id": 7, "name": "node", "status": "initializing", "public_net": {}, "private_net": []},
			"action": ` + finishedAction + `, "next_actions": []}`,
	})

	_, err := newHetznerAPI("token").CreateServer(context.Background(), serverSpec{
		Name: "node", ServerType: "cx33", Location: "nbg1", ImageID: 42, NetworkIDs: []int64{3}, FirewallIDs: []int64{4},
	})

	require.NoError(t, err)
	body := bodies["POST /servers"]
	assert.NotContains(t, body, "user_data", "the node's create request carries user data")
	assert.Equal(t, map[string]any{"enable_ipv4": false, "enable_ipv6": false}, body["public_net"], "the node's public net")
	assert.Equal(t, float64(42), body["image"], "the node boots from the snapshot by id")
	assert.Equal(t, []any{float64(3)}, body["networks"])
}

func TestHetznerAdapter_CreatesThePrivateLoadBalancerWithNoPublicInterfaceAndLabelTargets(t *testing.T) {
	bodies := recordHetzner(t, map[string]string{
		"POST /load_balancers": `{"load_balancer": {"id": 9, "name": "api", "public_net": {"enabled": false}, "private_net": [], "services": [], "targets": []},
			"action": ` + finishedAction + `}`,
	})

	_, err := newHetznerAPI("token").CreateLoadBalancer(context.Background(), apiLoadBalancer(ensureRequest{
		subID: testSub, params: clusterParams{appName: testApp, location: "nbg1"},
	}, 3))

	require.NoError(t, err)
	body := bodies["POST /load_balancers"]
	assert.Equal(t, false, body["public_interface"], "the private load balancer's public interface")
	assert.Equal(t, float64(3), body["network"])
	assert.Equal(t, []any{map[string]any{
		"type": "label_selector", "label_selector": map[string]any{"selector": roleSelector(testSub, roleNode)}, "use_private_ip": true,
	}}, body["targets"], "the private load balancer's targets")
}
