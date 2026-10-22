package main

// The adapter on the wire: a node with no user data and no public address, a private load balancer with no public
// interface, a server Hetzner cannot place refused, a snapshot started unwaited, and a start read from its action.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mitstack-eu/operator-kit/v3/handler"
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

	_, err := newHetznerAPI("token").CreateServers(context.Background(), serverSpec{
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

func TestHetznerAdapter_CreatesABareLoadBalancerWithItsPublicInterfaceOn(t *testing.T) {
	bodies := recordHetzner(t, map[string]string{
		"POST /load_balancers": `{"load_balancer": {"id": 9, "name": "api", "public_net": {"enabled": true}, "private_net": [], "services": [], "targets": []},
			"action": ` + finishedAction + `}`,
	})

	_, err := newHetznerAPI("token").CreateLoadBalancer(context.Background(), loadBalancerSpec{
		Name: "api", Type: "lb11", Location: "nbg1", PublicInterface: true,
	})

	require.NoError(t, err)
	body := bodies["POST /load_balancers"]
	assert.Equal(t, true, body["public_interface"], "the bare load balancer's public interface")
	for _, field := range []string{"network", "services", "targets"} {
		assert.NotContains(t, body, field, "the bare load balancer's create request")
	}
}

func TestHetznerAdapter_EachLoadBalancerStepSendsItsActionAndWaitsForIt(t *testing.T) {
	const lb = "POST /load_balancers/9/actions/"
	bodies := recordHetzner(t, map[string]string{
		lb + "attach_to_network":        `{"action": ` + finishedAction + `}`,
		lb + "disable_public_interface": `{"action": ` + finishedAction + `}`,
		lb + "add_service":              `{"action": ` + finishedAction + `}`,
		lb + "add_target":               `{"action": ` + finishedAction + `}`,
	})
	api := newHetznerAPI("token")
	ctx := context.Background()

	require.NoError(t, api.AttachLoadBalancerToNetwork(ctx, 9, 3, "10.0.0.3"))
	require.NoError(t, api.DisableLoadBalancerPublicInterface(ctx, 9))
	require.NoError(t, api.AddLoadBalancerService(ctx, 9, lbService{ListenPort: 443, DestinationPort: 30081, ProxyProtocol: true}))
	require.NoError(t, api.AddLoadBalancerTarget(ctx, 9, roleSelector(testSub, roleNode)))

	assert.Equal(t, map[string]any{"network": float64(3), "ip": "10.0.0.3"}, bodies[lb+"attach_to_network"])
	assert.Contains(t, bodies, lb+"disable_public_interface")
	service := bodies[lb+"add_service"]
	assert.Equal(t, "tcp", service["protocol"])
	assert.Equal(t, float64(443), service["listen_port"])
	assert.Equal(t, float64(30081), service["destination_port"])
	assert.Equal(t, true, service["proxyprotocol"])
	assert.Equal(t, map[string]any{"protocol": "tcp", "port": float64(30081), "interval": float64(15), "timeout": float64(10), "retries": float64(3)},
		service["health_check"], "the service's health check")
	assert.Equal(t, map[string]any{
		"type": "label_selector", "label_selector": map[string]any{"selector": roleSelector(testSub, roleNode)}, "use_private_ip": true,
	}, bodies[lb+"add_target"])
}

func TestHetznerAdapter_AnAttachWhoseActionEndsInAnErrorFails(t *testing.T) {
	recordHetzner(t, map[string]string{
		"POST /load_balancers/9/actions/attach_to_network": `{"action": {"id": 2, "command": "attach_to_network", "status": "error", "progress": 100,
			"resources": [], "error": {"code": "ip_not_available", "message": "the IP is not available"}}}`,
	})

	err := newHetznerAPI("token").AttachLoadBalancerToNetwork(context.Background(), 9, 3, "10.0.0.3")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ip_not_available")
}

func TestHetznerAdapter_RefusesAServerHetznerCannotPlaceNamingItsReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`{"error": {"code": "resource_unavailable", "message": "server type cx33 unavailable in nbg1"}}`))
	}))
	t.Cleanup(srv.Close)
	swap(t, &hetznerEndpoint, srv.URL)

	_, err := newHetznerAPI("token").CreateServers(context.Background(), serverSpec{Name: "node", ServerType: "cx33", Location: "nbg1", ImageID: 42})

	var refused *handler.Refused
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, "server-unavailable", refused.Class)
	assert.Contains(t, refused.Detail, "server type cx33 unavailable in nbg1")
}

func TestHetznerAdapter_StartsTheSnapshotWithoutWaitingForItsAction(t *testing.T) {
	recordHetzner(t, map[string]string{
		"POST /servers/7/actions/create_image": `{"image": {"id": 8, "status": "creating"},
			"action": {"id": 3, "command": "create_image", "status": "running", "progress": 0, "resources": []}}`,
	})

	snap, err := newHetznerAPI("token").StartSnapshot(context.Background(), 7, snapshotSpec{Description: "image"})

	require.NoError(t, err, "a wait would poll the running action, which this Hetzner does not serve")
	assert.Equal(t, "creating", snap.Status)
}

func TestHetznerAdapter_AServerHasStartedOnceItsStartActionSucceeded(t *testing.T) {
	for command, want := range map[string]bool{"start_server": true, "create_server": false} {
		recordHetzner(t, map[string]string{
			"GET /servers/7/actions": `{"actions": [{"id": 4, "command": "` + command + `", "status": "success", "progress": 100, "resources": []}]}`,
		})

		started, err := newHetznerAPI("token").ServerStarted(context.Background(), 7)

		require.NoError(t, err)
		assert.Equal(t, want, started, "a server whose finished action is %s", command)
	}
}
