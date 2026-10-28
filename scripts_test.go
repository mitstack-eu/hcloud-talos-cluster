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
	"sync"
	"testing"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testImager = "ghcr.io/siderolabs/imager:v1.14.4@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// imagerPeakBytes is the most imager v1.14.4 held in /out building an hcloud image: its whole raw disk, before compressing it.
const imagerPeakBytes = 4426580 * 1024

// testPrivateInterface is the builder's one ethernet interface, its private network's, as a cpx32 names it.
const testPrivateInterface = "enp7s0"

// fakeLoader is the dynamic loader fake ldd names for every program, and fakeLibraries the libraries it names.
const fakeLoader = "ld-linux-x86-64.so.2"

var fakeLibraries = []string{"libtinfo.so.6", "liblzma.so.5", "libc.so.6"}

// buildFakes stand in for the builder's commands, docker run failing as imager does when /out lies on a tmpfs smaller
// than imagerPeakBytes, fsfreeze turning every fake left on the disk into a recorder, sleep recording what the disk and
// the sysrq trigger held when it ran, ip finding no route to the router on the private interface until it has been
// asked more than FAKE_LEASE_AFTER times, and FAKE_FAILING naming one to fail.
func buildFakes(imagerExit int) map[string]string {
	exit := strconv.Itoa(imagerExit)
	return map[string]string{
		"ip": `if [ "$*" = "-4 -o route get ` + routerAddress + `" ]; then
  if [ "$(grep -c '^ip -4 -o route get ' "$(dirname "$0")/calls.log")" -le "${FAKE_LEASE_AFTER:-0}" ]; then
    echo "RTNETLINK answers: Network is unreachable" >&2
    exit 2
  fi
  echo "` + routerAddress + ` dev ` + testPrivateInterface + ` src 10.0.0.4 uid 0"
fi`,
		"mountpoint": "exit 1",
		"mount":      `printf '%s %s\n' "${@: -1}" "${4#size=}" >> "$(dirname "$0")/tmpfs"`,
		"sync":       "",
		"fsfreeze": `find "$BUILD_WORK_DIR" -type f > "$HOME/tmpfs-at-freeze"
for fake in "$HOME"/*; do
  if [ -f "$fake" ] && [ -x "$fake" ] && [ "$fake" != "$0" ]; then
    printf '#!/bin/sh\necho "$0" >> "$HOME/ran-after-freeze"\nexit 1\n' > "$fake"
  fi
done`,
		"ldd": `printf '\tlinux-vdso.so.1 (0x00007ffcc3563000)\n'
case ${1##*/} in
  bash) printf '\tlibtinfo.so.6 => %s/lib/libtinfo.so.6 (0x00007f87e5459000)\n' "$HOME" ;;
  zstd) printf '\tliblzma.so.5 => %s/lib/liblzma.so.5 (0x00007f87e5254000)\n' "$HOME" ;;
esac
[ "${FAKE_FAILING:-}" != ldd ] || printf '\tlibmissing.so.1 => not found\n'
printf '\tlibc.so.6 => %s/lib/libc.so.6 (0x00007f87e4e92000)\n\t%s/` + fakeLoader + ` (0x00005574bf12e000)\n' "$HOME" "$HOME"`,
		fakeLoader: `printf '%s\n' "${LC_ALL-unset}" >> "$HOME/loader-locales"
while [ "$#" -gt 0 ]; do
  case $1 in
    --library-path) shift 2 ;;
    --*) shift ;;
    *) break ;;
  esac
done
exec "$@"`,
		"zstd": `echo "zstd: a diagnostic on stderr" >&2
[ "${FAKE_FAILING:-}" != zstd ] || exit 1
cat "$2"`,
		"dd": `echo "dd: a diagnostic on stderr" >&2
[ "${FAKE_FAILING:-}" != dd ] || exit 1
cat > "${BUILD_DISK}"`,
		"sleep": `printf 'disk %s, sysrq-trigger %s\n' "$(cat "$BUILD_DISK")" "$(cat "$BUILD_PROC/sysrq-trigger" 2> /dev/null || echo unwritten)" \
  >> "$HOME/at-pause"`,
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

func runBuild(t *testing.T, imagerExit int, extraEnv ...string) scriptRun {
	t.Helper()
	return runBuildAgainst(t, newGatewayMirror(t, 0), imagerExit, extraEnv...)
}

// gatewayMirror stands in for the gateway's mirror over TLS, refusing the first unready requests with 503, and
// records for each request whether imager had run and whether the disk had been written.
type gatewayMirror struct {
	server  *httptest.Server
	ca      string
	unready int
	mu      sync.Mutex
	image   string
	disk    string
	seen    []mirrorRequest
}

type mirrorRequest struct {
	path                      string
	imagerHadRun, diskWritten bool
}

func newGatewayMirror(t *testing.T, unready int) *gatewayMirror {
	t.Helper()
	m := &gatewayMirror{unready: unready}
	m.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		_, imagerErr := os.Stat(m.image)
		_, diskErr := os.Stat(m.disk)
		m.seen = append(m.seen, mirrorRequest{path: r.URL.Path, imagerHadRun: imagerErr == nil, diskWritten: diskErr == nil})
		if len(m.seen) <= m.unready {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("kind: List"))
	}))
	t.Cleanup(m.server.Close)
	m.ca = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: m.server.Certificate().Raw}))
	return m
}

// watch names where imager leaves the run's image, and the run's disk.
func (m *gatewayMirror) watch(image, disk string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.image, m.disk = image, disk
}

func (m *gatewayMirror) requests() []mirrorRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.seen)
}

// mirrorPath is the served manifest a test's builder asks the mirror for.
const mirrorPath = "/manifests/last.yaml"

// runBuildAgainst runs the build against mirror, trusting the CA the mirror's certificate is issued under.
func runBuildAgainst(t *testing.T, mirror *gatewayMirror, imagerExit int, extraEnv ...string) scriptRun {
	t.Helper()
	dir := t.TempDir()
	tools := testutil.NewTools(t, buildFakes(imagerExit))
	require.NoError(t, os.MkdirAll(filepath.Join(tools.Dir, "lib"), 0o755))
	for _, library := range fakeLibraries {
		require.NoError(t, os.WriteFile(filepath.Join(tools.Dir, "lib", library), []byte(library), 0o644))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "proc"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "net", testPrivateInterface), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "net", "lo"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "net", "docker0"), 0o755))
	config := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(config, []byte("machine: {token: secret}"), 0o600))
	caPath := filepath.Join(dir, "gateway-ca.crt")
	require.NoError(t, os.WriteFile(caPath, []byte(mirror.ca), 0o644))
	mirror.watch(filepath.Join(dir, "out", "hcloud-amd64.raw.zst"), filepath.Join(dir, "disk"))
	env := tools.Env(append([]string{"BUILD_WORK_DIR=" + filepath.Join(dir, "work"), "BUILD_OUT_DIR=" + filepath.Join(dir, "out"),
		"BUILD_DISK=" + filepath.Join(dir, "disk"), "BUILD_RESOLV_CONF=" + filepath.Join(dir, "resolv.conf"),
		"BUILD_PROC=" + filepath.Join(dir, "proc"), "BUILD_PULL_ATTEMPTS=1", "BUILD_NET_CLASS=" + filepath.Join(dir, "net"),
		"BUILD_ADDRESS_WAIT=5", "BUILD_MIRROR_DELAY=1"}, extraEnv...)...)
	out, err := testutil.RunBash(t, buildImageScript, env, testImager, config, routerAddress, mirror.server.URL+mirrorPath, caPath)
	return scriptRun{out: out, err: err, tools: tools, dir: dir}
}

// loaderRun is one program the build ran through the loader, with the directory it took the libraries from, and its
// index among the calls.
type loaderRun struct {
	program, libraries string
	inhibitsCache      bool
	call               int
}

func (r scriptRun) loaderRuns(t *testing.T) []loaderRun {
	t.Helper()
	var runs []loaderRun
	for i, call := range r.tools.Calls(t) {
		args, ok := strings.CutPrefix(call, fakeLoader+" ")
		if !ok {
			continue
		}
		run := loaderRun{call: i}
		fields := strings.Fields(args)
		for j := 0; j < len(fields) && run.program == ""; j++ {
			switch {
			case fields[j] == "--inhibit-cache":
				run.inhibitsCache = true
			case fields[j] == "--library-path":
				require.Less(t, j+1, len(fields), "--library-path without a path: %s", call)
				j++
				run.libraries = fields[j]
			case !strings.HasPrefix(fields[j], "--"):
				run.program = fields[j]
			}
		}
		runs = append(runs, run)
	}
	return runs
}

// tmpfsAtFreeze are the files in the build tmpfs when the root filesystem froze.
func (r scriptRun) tmpfsAtFreeze(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.tools.Dir, "tmpfs-at-freeze"))
	require.NoError(t, err, "the build never froze its root filesystem")
	return strings.Fields(string(data))
}

// recorded is what the fakes wrote to the named record, or nothing when none wrote to it.
func (r scriptRun) recorded(name string) string {
	data, _ := os.ReadFile(filepath.Join(r.tools.Dir, name))
	return string(data)
}

func TestBuildImage_RoutesThroughTheGatewayBuildsWritesTheDiskAndPowersOff(t *testing.T) {
	run := runBuild(t, 0)

	require.NoError(t, run.err, run.out)
	dir := run.dir
	calls := run.tools.Calls(t)
	route := slices.Index(calls, "ip route replace default via "+routerAddress)
	require.GreaterOrEqual(t, route, 0, "calls: %v", calls)
	require.Less(t, route+1, len(calls), "calls: %v", calls)
	assert.Equal(t, "docker pull --quiet "+testImager, calls[route+1])
	assert.Contains(t, calls, "docker run --rm --privileged -v /dev:/dev -v "+filepath.Join(dir, "work")+"/in:/in:ro -v "+
		filepath.Join(dir, "out")+":/out "+testImager+" hcloud --arch amd64 --embedded-config-path /in/config.yaml")
	disk, _ := os.ReadFile(filepath.Join(dir, "disk"))
	assert.Equal(t, "talos image", string(disk), "the disk holds the image")
	sysrq, _ := os.ReadFile(filepath.Join(dir, "proc", "sysrq-trigger"))
	assert.Equal(t, "o", string(sysrq), "the builder powers off with the single character the sysrq trigger reads")
	resolv, _ := os.ReadFile(filepath.Join(dir, "resolv.conf"))
	assert.Equal(t, "nameserver 185.12.64.1\nnameserver 185.12.64.2\n", string(resolv))
	assert.NoFileExists(t, filepath.Join(dir, "config.yaml"), "the configuration outlives the build")
	assert.NotContains(t, run.out, "every key of the cluster", "imager's output reached the log")
}

func TestBuildImage_WritesTheDiskOnlyOnceTheGatewaysMirrorAnswersAndBuildsWhileItFills(t *testing.T) {
	mirror := newGatewayMirror(t, 2)

	run := runBuildAgainst(t, mirror, 0)

	require.NoError(t, run.err, run.out)
	requests := mirror.requests()
	require.Len(t, requests, 3, "the build asks the mirror until it answers, and no more")
	for i, r := range requests {
		assert.Equal(t, mirrorPath, r.path, "request %d", i)
		assert.True(t, r.imagerHadRun, "request %d came before imager ran, so the build waited on the mirror instead of beside it", i)
		assert.False(t, r.diskWritten, "request %d came after the disk was written", i)
	}
	disk, _ := os.ReadFile(filepath.Join(run.dir, "disk"))
	assert.Equal(t, "talos image", string(disk), "the disk holds the image")
	sysrq, _ := os.ReadFile(filepath.Join(run.dir, "proc", "sysrq-trigger"))
	assert.Equal(t, "o", string(sysrq), "the builder powers off once the mirror answers")
}

func TestBuildImage_AMirrorThatNeverAnswersLeavesTheDiskUnwrittenAndTheBuilderRunning(t *testing.T) {
	mirror := newGatewayMirror(t, 1000)

	run := runBuildAgainst(t, mirror, 0, "BUILD_MIRROR_ATTEMPTS=1")

	require.Error(t, run.err)
	assert.Contains(t, run.out, "the gateway's mirror at "+mirror.server.URL+mirrorPath+" did not answer")
	assert.Len(t, mirror.requests(), 2, "the build asks once and retries BUILD_MIRROR_ATTEMPTS times")
	assert.NotContains(t, run.tools.Calls(t), "fsfreeze -f /", "the root filesystem froze before the mirror answered")
	assert.NoFileExists(t, filepath.Join(run.dir, "disk"), "the disk was written before the mirror answered")
	assert.NoFileExists(t, filepath.Join(run.dir, "proc", "sysrq-trigger"), "the builder powered off before the mirror answered")
}

func TestBuildImage_TrustsTheMirrorOnlyUnderTheGatewaysCA(t *testing.T) {
	mirror := newGatewayMirror(t, 0)
	other, err := gateway.NewCA(time.Now(), time.Hour)
	require.NoError(t, err)
	mirror.ca = string(other.Cert)

	run := runBuildAgainst(t, mirror, 0, "BUILD_MIRROR_ATTEMPTS=0")

	require.Error(t, run.err)
	assert.Empty(t, mirror.requests(), "the build sent a request to a mirror whose certificate the gateway's CA did not issue")
	assert.NoFileExists(t, filepath.Join(run.dir, "disk"), "the disk was written on an untrusted mirror's answer")
}

func TestBuildImage_WaitsForThePrivateInterfacesLeaseBeforeRoutingThroughTheGateway(t *testing.T) {
	run := runBuild(t, 0, "FAKE_LEASE_AFTER=3")

	require.NoError(t, run.err, run.out)
	calls := run.tools.Calls(t)
	var asked []int
	for i, call := range calls {
		if call == "ip -4 -o route get "+routerAddress {
			asked = append(asked, i)
		}
	}
	route := slices.Index(calls, "ip route replace default via "+routerAddress)
	require.Len(t, asked, 4, "the build asks for the route to the router until the lease gives one: %v", calls)
	assert.Greater(t, route, asked[3], "the build routes through the gateway before the private interface has its lease: %v", calls)
}

func TestBuildImage_APrivateInterfaceWithNoAddressFailsTheBuildNamingIt(t *testing.T) {
	run := runBuild(t, 0, "FAKE_LEASE_AFTER=1000")

	require.Error(t, run.err, run.out)
	assert.Contains(t, run.out, testPrivateInterface, "the failure does not name the interface that got no address")
	calls := run.tools.Calls(t)
	assert.NotContains(t, calls, "ip route replace default via "+routerAddress, "the build routed through a gateway it cannot reach")
	assert.False(t, slices.ContainsFunc(calls, func(call string) bool { return strings.HasPrefix(call, "docker ") }),
		"the build went on to pull imager: %v", calls)
	assert.NoFileExists(t, filepath.Join(run.dir, "proc", "sysrq-trigger"), "the builder powered off")
}

func TestBuildImage_ABuilderWithoutExactlyOneEthernetInterfaceFailsBeforeRouting(t *testing.T) {
	for name, links := range map[string][]string{"none": {"lo"}, "two": {testPrivateInterface, "enp8s0"}} {
		t.Run(name, func(t *testing.T) {
			netClass := t.TempDir()
			for _, link := range links {
				require.NoError(t, os.MkdirAll(filepath.Join(netClass, link), 0o755))
			}

			run := runBuild(t, 0, "BUILD_NET_CLASS="+netClass)

			require.Error(t, run.err, run.out)
			assert.Contains(t, run.out, netClass, "the failure does not name where it looked for the interface")
			calls := run.tools.Calls(t)
			assert.NotContains(t, calls, "ip route replace default via "+routerAddress, "the build routed without knowing its interface")
			assert.False(t, slices.ContainsFunc(calls, func(call string) bool { return strings.HasPrefix(call, "docker ") }),
				"the build went on to pull imager: %v", calls)
		})
	}
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
	freeze := slices.Index(calls, "fsfreeze -f /")
	dd := slices.IndexFunc(calls, func(call string) bool { return strings.HasPrefix(call, "dd ") })
	assert.True(t, freeze >= 0 && freeze < dd, "the root filesystem, removed output included, is frozen before dd overwrites the disk: %v", calls)
	assert.NoDirExists(t, filepath.Join(run.dir, "out"), "imager's output outlives the build on the disk dd overwrites")
}

func TestBuildImage_FreezesTheRootThenOverwritesAndPowersOffFromTheTmpfsAlone(t *testing.T) {
	run := runBuild(t, 0)

	require.NoError(t, run.err, run.out)
	calls := run.tools.Calls(t)
	freeze := slices.Index(calls, "fsfreeze -f /")
	require.GreaterOrEqual(t, freeze, 0, "the root filesystem is never frozen: %v", calls)
	runs := run.loaderRuns(t)
	require.Len(t, runs, 4, "the loader runs bash, then zstd and dd, then sleep: %v", calls)
	assert.Equal(t, "bash", filepath.Base(runs[0].program), "the final stage runs in bash: %v", runs)
	assert.ElementsMatch(t, []string{"zstd", "dd"}, []string{filepath.Base(runs[1].program), filepath.Base(runs[2].program)},
		"the final stage pipes zstd into dd: %v", runs)
	assert.Equal(t, "sleep", filepath.Base(runs[3].program), "the final stage pauses after the write: %v", runs)
	atFreeze := run.tmpfsAtFreeze(t)
	for _, loaded := range runs {
		assert.Greater(t, loaded.call, freeze, "%s ran before the freeze: %v", loaded.program, calls)
		assert.Contains(t, atFreeze, loaded.program, "%s was not copied into the tmpfs before the freeze", loaded.program)
		assert.True(t, loaded.inhibitsCache, "the loader may read /etc/ld.so.cache from the disk for %s", loaded.program)
		for _, library := range fakeLibraries {
			assert.Contains(t, atFreeze, filepath.Join(loaded.libraries, library),
				"%s loads its libraries from %s, which held no copy of %s at the freeze", loaded.program, loaded.libraries, library)
		}
	}
	assert.Equal(t, "C\nC\nC\nC\n", run.recorded("loader-locales"),
		"bash, zstd, dd and sleep do not all run in the C locale, which reads no locale file")
	assert.Empty(t, run.recorded("ran-after-freeze"), "programs on the disk ran after the freeze")
	assert.Contains(t, calls, "dd of="+filepath.Join(run.dir, "disk")+" bs=4M oflag=direct conv=fsync status=none")
	assert.False(t, slices.ContainsFunc(calls, func(call string) bool { return strings.HasPrefix(call, "sync ") }),
		"sync ran, which the freeze and conv=fsync make redundant: %v", calls)
	sysrq, _ := os.ReadFile(filepath.Join(run.dir, "proc", "sysrq-trigger"))
	assert.Equal(t, "o", string(sysrq), "the final stage powers the builder off")
	assert.NotContains(t, run.out, "a diagnostic on stderr", "the final stage printed after the freeze")
}

func TestBuildImage_PausesASecondBetweenTheImageWriteAndThePowerOff(t *testing.T) {
	run := runBuild(t, 0)

	require.NoError(t, run.err, run.out)
	assert.Contains(t, run.tools.Calls(t), "sleep 1", "the final stage does not pause for one second")
	assert.Equal(t, "disk talos image, sysrq-trigger unwritten\n", run.recorded("at-pause"),
		"the pause does not come once, after dd wrote the whole image and before the power-off")
}

func TestBuildImage_AFailedOverwriteAfterTheFreezeKeepsTheBuilderRunning(t *testing.T) {
	for _, failing := range []string{"zstd", "dd"} {
		t.Run("a failing "+failing, func(t *testing.T) {
			run := runBuild(t, 0, "FAKE_FAILING="+failing)

			require.Error(t, run.err, run.out)
			assert.Contains(t, run.tools.Calls(t), "fsfreeze -f /", "the build failed before the final stage")
			assert.NoFileExists(t, filepath.Join(run.dir, "proc", "sysrq-trigger"),
				"the builder powered off after a failed overwrite, so the block would snapshot a broken disk")
			assert.NotContains(t, run.out, "a diagnostic on stderr", "the final stage printed after the freeze")
		})
	}
}

func TestBuildImage_ALibraryLddCannotResolveFailsTheBuildBeforeTheFreeze(t *testing.T) {
	run := runBuild(t, 0, "FAKE_FAILING=ldd")

	require.Error(t, run.err)
	assert.Contains(t, run.out, "libmissing.so.1", "the failure does not name the library ldd could not resolve")
	assert.NotContains(t, run.tools.Calls(t), "fsfreeze -f /", "the root filesystem froze with a library the final stage cannot load")
	assert.NoFileExists(t, filepath.Join(run.dir, "proc", "sysrq-trigger"), "the builder powered off")
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
