package main

// The builder's, the gateway's and the cluster's scripts, run with bash against fake commands that log each call.

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testImager = "ghcr.io/siderolabs/imager:v1.14.4@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// imagerPeakBytes is the most imager v1.14.4 held in /out building an hcloud image: its whole raw disk, before compressing it.
const imagerPeakBytes = 4426580 * 1024

// buildFakes stand in for the builder's commands, mount recording each tmpfs and docker run failing as imager does when
// /out lies on a tmpfs smaller than imagerPeakBytes.
func buildFakes(imagerExit int) map[string]string {
	exit := strconv.Itoa(imagerExit)
	return map[string]string{
		"ip":         "",
		"mountpoint": "exit 1",
		"mount":      `printf '%s %s\n' "${@: -1}" "${4#size=}" >> "$(dirname "$0")/tmpfs"`,
		"sync":       "",
		"zstd":       `cat "$2"`,
		"dd":         `cat > "${BUILD_DISK}"`,
		"docker": `if [ "$1" = run ]; then
  out=$(printf '%s\n' "$@" | sed -n 's#^\(.*\):/out$#\1#p')
  echo "every key of the cluster"
  [ ` + exit + ` -eq 0 ] || exit ` + exit + `
  while read -r target size; do
    case "$out/" in "$target"/*)
      [ "$(numfmt --from=iec "${size^^}")" -ge ` + strconv.Itoa(imagerPeakBytes) + ` ] ||
        { echo "write /out/hcloud-amd64.raw: no space left on device"; exit 7; } ;;
    esac
  done < <(cat "$(dirname "$0")/tmpfs" 2> /dev/null)
  printf 'talos image' > "$out/hcloud-amd64.raw.zst"
fi`,
	}
}

// tmpfsTargets are the directories the build mounted a tmpfs on.
func (r scriptRun) tmpfsTargets(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.tools.Dir, "tmpfs"))
	require.NoError(t, err, "the build mounted no tmpfs")
	var targets []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		targets = append(targets, strings.Fields(line)[0])
	}
	return targets
}

// scriptRun is one run of a script: its output and exit error, its fakes' calls, and its scratch directory.
type scriptRun struct {
	out   string
	err   error
	tools *testutil.Tools
	dir   string
}

func runBuild(t *testing.T, imagerExit int) scriptRun {
	t.Helper()
	dir := t.TempDir()
	tools := testutil.NewTools(t, buildFakes(imagerExit))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "proc", "sys", "kernel"), 0o755))
	config := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(config, []byte("machine: {token: secret}"), 0o600))
	env := tools.Env("BUILD_WORK_DIR="+filepath.Join(dir, "work"), "BUILD_OUT_DIR="+filepath.Join(dir, "out"),
		"BUILD_DISK="+filepath.Join(dir, "disk"), "BUILD_RESOLV_CONF="+filepath.Join(dir, "resolv.conf"),
		"BUILD_PROC="+filepath.Join(dir, "proc"), "BUILD_PULL_ATTEMPTS=1")
	out, err := testutil.RunBash(t, buildImageScript, env, testImager, config, routerAddress)
	return scriptRun{out: out, err: err, tools: tools, dir: dir}
}

func TestBuildImage_RoutesThroughTheGatewayBuildsWritesTheDiskAndPowersOff(t *testing.T) {
	run := runBuild(t, 0)

	require.NoError(t, run.err, run.out)
	dir := run.dir
	calls := run.tools.Calls(t)
	require.GreaterOrEqual(t, len(calls), 4, "calls: %v", calls)
	assert.Equal(t, "ip route replace default via "+routerAddress, calls[0])
	assert.Equal(t, "docker pull --quiet "+testImager, calls[1])
	assert.Contains(t, calls, "docker run --rm --privileged -v /dev:/dev -v "+filepath.Join(dir, "work")+"/in:/in:ro -v "+
		filepath.Join(dir, "out")+":/out "+testImager+" hcloud --arch amd64 --embedded-config-path /in/config.yaml")
	disk, _ := os.ReadFile(filepath.Join(dir, "disk"))
	assert.Equal(t, "talos image", string(disk), "the disk holds the image")
	sysrq, _ := os.ReadFile(filepath.Join(dir, "proc", "sysrq-trigger"))
	assert.Equal(t, "o\n", string(sysrq), "the builder powers off")
	resolv, _ := os.ReadFile(filepath.Join(dir, "resolv.conf"))
	assert.Equal(t, "nameserver 185.12.64.1\nnameserver 185.12.64.2\n", string(resolv))
	assert.NoFileExists(t, filepath.Join(dir, "config.yaml"), "the configuration outlives the build")
	assert.NotContains(t, run.out, "every key of the cluster", "imager's output reached the log")
}

func TestBuildImage_WritesTheDiskFromATmpfsNeverFromTheDiskItOverwrites(t *testing.T) {
	run := runBuild(t, 0)

	require.NoError(t, run.err, run.out)
	var reads []string
	for _, call := range run.tools.Calls(t) {
		if strings.HasPrefix(call, "zstd ") {
			reads = append(reads, strings.Fields(call)[2])
		}
	}
	require.Len(t, reads, 1, "calls: %v", run.tools.Calls(t))
	assert.True(t, slices.ContainsFunc(run.tmpfsTargets(t), func(target string) bool {
		return strings.HasPrefix(reads[0], target+"/")
	}), "dd writes the image read from %s, outside every tmpfs", reads[0])
	calls := run.tools.Calls(t)
	sync := slices.Index(calls, "sync ")
	dd := slices.IndexFunc(calls, func(call string) bool { return strings.HasPrefix(call, "dd ") })
	assert.True(t, sync >= 0 && sync < dd, "the removed output is synced before dd overwrites the disk: %v", calls)
	assert.NoDirExists(t, filepath.Join(run.dir, "out"), "imager's output outlives the build on the disk dd overwrites")
}

func TestBuildImage_AFailedImagerWritesNothingAndKeepsTheBuilderRunning(t *testing.T) {
	run := runBuild(t, 1)

	require.Error(t, run.err)
	assert.Contains(t, run.out, "imager failed")
	assert.NotContains(t, run.out, "every key of the cluster", "imager's output reached the log")
	assert.NoFileExists(t, filepath.Join(run.dir, "disk"), "the disk was written")
	assert.NoFileExists(t, filepath.Join(run.dir, "proc", "sysrq-trigger"), "the builder powered off")
	for _, call := range run.tools.Calls(t) {
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

func runClusterTokens(t *testing.T, srv *httptest.Server, verb, token string, extraEnv ...string) scriptRun {
	t.Helper()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.crt")
	require.NoError(t, os.WriteFile(ca, certPEM(srv), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "token"), []byte(token), 0o600))
	tools := testutil.NewTools(t, map[string]string{"kubectl": `case $1 in
  apply) cat > /dev/null ;;
  get) [ "${SECRETS_PRESENT:-no}" = yes ] ;;
  *) cat ;;
esac`})
	env := tools.Env("PLATFORM_URL="+srv.URL, "SUBSCRIPTION_ID="+testSub, "REPO_URL=https://github.com/"+testOwner+"/shop-cluster",
		"TOKEN_FILE="+filepath.Join(dir, "token"), "CURL_CA_BUNDLE="+ca, "KUBE_API="+srv.URL, "KUBE_ACCOUNT_DIR="+dir, "CALL_ATTEMPTS=1")
	out, err := testutil.RunBash(t, clusterTokensScript, append(env, extraEnv...), verb)
	return scriptRun{out: out, err: err, tools: tools, dir: dir}
}

func TestClusterTokens_EnrolSpendsTheEnrolmentTokenAndWritesBothSecrets(t *testing.T) {
	srv, seen := platform(t, http.StatusOK)

	run := runClusterTokens(t, srv, "enrol", "enrolment-token")

	require.NoError(t, run.err, run.out)
	assert.Equal(t, []string{"POST /v1/cluster-enrolments Bearer enrolment-token"}, *seen)
	var created []string
	for _, call := range run.tools.Calls(t) {
		if strings.HasPrefix(call, "kubectl create secret") {
			created = append(created, strings.Fields(call)[4]+" in "+strings.Fields(call)[6])
		}
	}
	assert.Equal(t, []string{"mitstack-report-token in mitstack-node", "cluster-repository in argocd"}, created)
}

func TestClusterTokens_EnrolTreatsASpentTokenAsEnrolledOnlyWhenItsSecretsExist(t *testing.T) {
	srv, _ := platform(t, http.StatusConflict)

	present := runClusterTokens(t, srv, "enrol", "enrolment-token", "SECRETS_PRESENT=yes")
	missing := runClusterTokens(t, srv, "enrol", "enrolment-token", "SECRETS_PRESENT=no")

	require.NoError(t, present.err, present.out)
	assert.Contains(t, present.out, "enrolled already")
	require.Error(t, missing.err, "a spent token with no Secrets must fail the Job")
	assert.Contains(t, missing.out, "mitstack-report-token")
}

func TestClusterTokens_RenewPatchesEachAppSlicesRepositorySecret(t *testing.T) {
	srv, seen := platform(t, http.StatusOK)

	run := runClusterTokens(t, srv, "renew", "report-token")

	require.NoError(t, run.err, run.out)
	assert.Equal(t, []string{
		"POST /v1/cluster-repository-tokens Bearer report-token",
		"PATCH /api/v1/namespaces/argocd/secrets/app-repository-orbit Bearer report-token",
	}, *seen)
}

func certPEM(srv *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}
