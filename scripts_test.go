package main

// The builder's, the gateway's and the cluster's scripts, run with bash against fake commands that log each call.

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testImager = "ghcr.io/siderolabs/imager:v1.14.4@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// buildFakes stand in for the builder's commands; docker run leaves one image in the out directory it mounts.
func buildFakes(imagerExit int) map[string]string {
	exit := strconv.Itoa(imagerExit)
	return map[string]string{
		"ip":         "",
		"mountpoint": "",
		"sync":       "",
		"zstd":       `cat "$2"`,
		"dd":         `cat > "${BUILD_DISK}"`,
		"docker": `if [ "$1" = run ]; then
  out=$(printf '%s\n' "$@" | sed -n 's#^\(.*\):/out$#\1#p')
  echo "every key of the cluster"
  [ ` + exit + ` -eq 0 ] || exit ` + exit + `
  printf 'talos image' > "$out/hcloud-amd64.raw.zst"
fi`,
	}
}

func runBuild(t *testing.T, imagerExit int) (out string, err error, tools *testutil.Tools, dir string) {
	t.Helper()
	dir = t.TempDir()
	tools = testutil.NewTools(t, buildFakes(imagerExit))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "proc", "sys", "kernel"), 0o755))
	config := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(config, []byte("machine: {token: secret}"), 0o600))
	env := tools.Env("BUILD_WORK_DIR="+filepath.Join(dir, "work"), "BUILD_DISK="+filepath.Join(dir, "disk"),
		"BUILD_RESOLV_CONF="+filepath.Join(dir, "resolv.conf"), "BUILD_PROC="+filepath.Join(dir, "proc"), "BUILD_PULL_ATTEMPTS=1")
	out, err = testutil.RunBash(t, buildImageScript, env, testImager, config, routerAddress)
	return out, err, tools, dir
}

func TestBuildImage_RoutesThroughTheGatewayBuildsWritesTheDiskAndPowersOff(t *testing.T) {
	out, err, tools, dir := runBuild(t, 0)

	require.NoError(t, err, out)
	calls := tools.Calls(t)
	require.GreaterOrEqual(t, len(calls), 4, "calls: %v", calls)
	assert.Equal(t, "ip route replace default via "+routerAddress, calls[0])
	assert.Equal(t, "docker pull --quiet "+testImager, calls[1])
	assert.Contains(t, calls, "docker run --rm --privileged -v /dev:/dev -v "+filepath.Join(dir, "work")+"/in:/in:ro -v "+
		filepath.Join(dir, "work")+"/out:/out "+testImager+" hcloud --arch amd64 --embedded-config-path /in/config.yaml")
	disk, _ := os.ReadFile(filepath.Join(dir, "disk"))
	assert.Equal(t, "talos image", string(disk), "the disk holds the image")
	sysrq, _ := os.ReadFile(filepath.Join(dir, "proc", "sysrq-trigger"))
	assert.Equal(t, "o\n", string(sysrq), "the builder powers off")
	resolv, _ := os.ReadFile(filepath.Join(dir, "resolv.conf"))
	assert.Equal(t, "nameserver 185.12.64.1\nnameserver 185.12.64.2\n", string(resolv))
	assert.NoFileExists(t, filepath.Join(dir, "config.yaml"), "the configuration outlives the build")
	assert.NotContains(t, out, "every key of the cluster", "imager's output reached the log")
}

func TestBuildImage_AFailedImagerWritesNothingAndKeepsTheBuilderRunning(t *testing.T) {
	out, err, tools, dir := runBuild(t, 1)

	require.Error(t, err)
	assert.Contains(t, out, "imager failed")
	assert.NotContains(t, out, "every key of the cluster", "imager's output reached the log")
	assert.NoFileExists(t, filepath.Join(dir, "disk"), "the disk was written")
	assert.NoFileExists(t, filepath.Join(dir, "proc", "sysrq-trigger"), "the builder powered off")
	for _, call := range tools.Calls(t) {
		assert.False(t, strings.HasPrefix(call, "dd "), "dd ran: %s", call)
	}
}

func TestBootstrapFirstNode_BootstrapsTheAddressWhoseTalosAPIProvesItIsTheClusters(t *testing.T) {
	dir := t.TempDir()
	flows := filepath.Join(dir, "flows.log")
	require.NoError(t, os.WriteFile(flows, []byte(
		"[NEW] udp 17 30 src=10.0.0.6 dst=185.12.64.1 sport=1 dport=53\n"+
			"[NEW] tcp 6 120 src=10.0.0.7 dst=10.0.0.2 sport=2 dport=443\n"+
			"[NEW] tcp 6 120 src=10.0.0.2 dst=10.0.0.7 sport=3 dport=50000\n"), 0o644))
	talosconfig := filepath.Join(dir, "admin-talosconfig")
	require.NoError(t, os.WriteFile(talosconfig, []byte("admin"), 0o600))
	tools := testutil.NewTools(t, map[string]string{
		"talosctl":                 `[ "$4" = 10.0.0.7 ]`,
		"mitstack-talos-bootstrap": `rm -f "$4"`,
	})
	env := tools.Env("BOOTSTRAP_FLOWS_LOG="+flows, "BOOTSTRAP_POLL_SECONDS=0")

	out, err := testutil.RunBash(t, bootstrapFirstNodeScript, env, subnetPrefix, gatewayAddress, talosconfig, "9999999999")

	require.NoError(t, err, out)
	var bootstraps []string
	for _, call := range tools.Calls(t) {
		if strings.HasPrefix(call, "mitstack-talos-bootstrap") {
			bootstraps = append(bootstraps, call)
		}
	}
	require.Len(t, bootstraps, 1, "bootstraps: %v", tools.Calls(t))
	assert.Contains(t, bootstraps[0], "--node 10.0.0.7 --talosconfig ")
	assert.NoFileExists(t, talosconfig, "the admin talosconfig outlives the bootstrap")
}

func TestBootstrapFirstNode_GivesUpAndDeletesTheTalosconfigOnceItExpires(t *testing.T) {
	dir := t.TempDir()
	talosconfig := filepath.Join(dir, "admin-talosconfig")
	require.NoError(t, os.WriteFile(talosconfig, []byte("admin"), 0o600))
	tools := testutil.NewTools(t, map[string]string{"talosctl": "exit 1", "mitstack-talos-bootstrap": ""})

	out, err := testutil.RunBash(t, bootstrapFirstNodeScript, tools.Env("BOOTSTRAP_FLOWS_LOG="+filepath.Join(dir, "none")),
		subnetPrefix, gatewayAddress, talosconfig, "1")

	require.Error(t, err)
	assert.Contains(t, out, "before the talosconfig expired")
	assert.NoFileExists(t, talosconfig)
}

// platform plays the enrolment and renewal routes and the Kubernetes API's Secret patch over one TLS server.
func platform(t *testing.T, enrolStatus int) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v1/cluster-enrolments":
			w.WriteHeader(enrolStatus)
			_ = json.NewEncoder(w).Encode(map[string]string{"reportToken": "report-token", "repositoryToken": "repository-token"})
		case "/v1/cluster-repository-tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"repositoryToken": "fresh-token",
				"appRepositories": []map[string]string{{"project": "app-orbit", "repoUrl": "https://github.com/acme/orbit", "repositoryToken": "orbit-token"}},
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func runClusterTokens(t *testing.T, srv *httptest.Server, verb, token string) (string, error, *testutil.Tools) {
	t.Helper()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.crt")
	require.NoError(t, os.WriteFile(ca, certPEM(srv), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "token"), []byte(token), 0o600))
	tools := testutil.NewTools(t, map[string]string{"kubectl": `[ "$1" = apply ] && cat > /dev/null || cat`})
	env := tools.Env("PLATFORM_URL="+srv.URL, "SUBSCRIPTION_ID="+testSub, "REPO_URL=https://github.com/"+testOwner+"/shop-cluster",
		"TOKEN_FILE="+filepath.Join(dir, "token"), "CURL_CA_BUNDLE="+ca, "KUBE_API="+srv.URL, "KUBE_ACCOUNT_DIR="+dir, "CALL_ATTEMPTS=1")
	out, err := testutil.RunBash(t, clusterTokensScript, env, verb)
	return out, err, tools
}

func TestClusterTokens_EnrolSpendsTheEnrolmentTokenAndWritesBothSecrets(t *testing.T) {
	srv, seen := platform(t, http.StatusOK)

	out, err, tools := runClusterTokens(t, srv, "enrol", "enrolment-token")

	require.NoError(t, err, out)
	assert.Equal(t, []string{"POST /v1/cluster-enrolments Bearer enrolment-token"}, *seen)
	var created []string
	for _, call := range tools.Calls(t) {
		if strings.HasPrefix(call, "kubectl create secret") {
			created = append(created, strings.Fields(call)[4]+" in "+strings.Fields(call)[6])
		}
	}
	assert.Equal(t, []string{"mitstack-report-token in mitstack-node", "cluster-repository in argocd"}, created)
}

func TestClusterTokens_EnrolTreatsASpentTokenAsEnrolled(t *testing.T) {
	srv, _ := platform(t, http.StatusConflict)

	out, err, _ := runClusterTokens(t, srv, "enrol", "enrolment-token")

	require.NoError(t, err, out)
	assert.Contains(t, out, "enrolled already")
}

func TestClusterTokens_RenewPatchesEachAppSlicesRepositorySecret(t *testing.T) {
	srv, seen := platform(t, http.StatusOK)

	out, err, _ := runClusterTokens(t, srv, "renew", "report-token")

	require.NoError(t, err, out)
	assert.Equal(t, []string{
		"POST /v1/cluster-repository-tokens Bearer report-token",
		"PATCH /api/v1/namespaces/argocd/secrets/app-repository-orbit Bearer report-token",
	}, *seen)
}

func certPEM(srv *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}
