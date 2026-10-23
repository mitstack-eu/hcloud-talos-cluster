package main

// The builder embeds the kit's configuration with the block's own manifests, and the gateway runs the kit's cloud-config.

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mitstack-eu/operator-kit/v3/contract"
	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/image"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

type renderedCloudConfig struct {
	MergeHow   []mergeRule    `yaml:"merge_how"`
	WriteFiles []renderedFile `yaml:"write_files"`
	RunCmd     [][]string     `yaml:"runcmd"`
}

// renderedFile is a write_files entry as cloud-init reads it, its content still in the encoding it names.
type renderedFile struct {
	Path        string `yaml:"path"`
	Permissions string `yaml:"permissions"`
	Content     string `yaml:"content"`
	Encoding    string `yaml:"encoding"`
}

func decodeCloudConfig(t *testing.T, data string) renderedCloudConfig {
	t.Helper()
	require.True(t, strings.HasPrefix(data, "#cloud-config\n"), "a cloud-config opens with its header")
	var c renderedCloudConfig
	require.NoError(t, yaml.Unmarshal([]byte(data), &c))
	return c
}

// file returns the file cloud-init writes at path, its content decoded as cloud-init's write_files decodes it.
func (c renderedCloudConfig) file(t *testing.T, path string) renderedFile {
	t.Helper()
	for _, f := range c.WriteFiles {
		if f.Path != path {
			continue
		}
		switch f.Encoding {
		case "":
		case "gz+b64":
			zipped, err := base64.StdEncoding.DecodeString(f.Content)
			require.NoError(t, err, "the base64 of %s", path)
			r, err := gzip.NewReader(bytes.NewReader(zipped))
			require.NoError(t, err, "the gzip of %s", path)
			plain, err := io.ReadAll(r)
			require.NoError(t, err, "the gzip of %s", path)
			f.Content, f.Encoding = string(plain), ""
		default:
			require.Failf(t, "unknown encoding", "%s is in the encoding %q, which this test does not decode", path, f.Encoding)
		}
		return f
	}
	require.Failf(t, "no such file", "the cloud-config writes no %s", path)
	return renderedFile{}
}

// gatewayParts splits the gateway's MIME user data into its cloud-config parts, in order.
func gatewayParts(t *testing.T, data string) []string {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(data))
	require.NoError(t, err)
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/mixed", mediaType)
	reader := multipart.NewReader(msg.Body, params["boundary"])
	var parts []string
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return parts
		}
		require.NoError(t, err)
		assert.Equal(t, `text/cloud-config; charset="us-ascii"`, part.Header.Get("Content-Type"))
		body, err := io.ReadAll(part)
		require.NoError(t, err)
		parts = append(parts, string(body))
	}
}

func subscribedOnce(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	require.NoError(t, e.converge())
	return e
}

// builderConfiguration reads the builder's create request, so it holds once the builder is deleted.
func builderConfiguration(t *testing.T, e *env) []map[string]any {
	t.Helper()
	config := decodeCloudConfig(t, e.hetzner.specFor(t, roleBuilder).UserData).file(t, builderConfigPath)
	return testutil.Documents(t, []byte(config.Content))
}

func inlineManifests(t *testing.T, config []map[string]any) map[string][]map[string]any {
	t.Helper()
	manifests := map[string][]map[string]any{}
	for _, doc := range testutil.Kind(config, "KubeInlineManifestConfig") {
		manifests[doc["name"].(string)] = testutil.Documents(t, []byte(doc["manifest"].(string)))
	}
	return manifests
}

func TestBuilder_EmbedsAConfigurationThatCarriesTheReportTheCredentialsAndTheApplication(t *testing.T) {
	e := subscribedOnce(t)
	builder := e.hetzner.byRole(t, roleBuilder)[0]

	c := decodeCloudConfig(t, e.hetzner.userData[builder.ID])

	config := c.file(t, builderConfigPath)
	assert.Equal(t, "0600", config.Permissions, "the configuration's permissions")
	docs := testutil.Documents(t, []byte(config.Content))
	var inline []string
	for _, doc := range testutil.Kind(docs, "KubeInlineManifestConfig") {
		inline = append(inline, doc["name"].(string))
	}
	assert.Subset(t, inline, []string{"mitstack-node-report", "mitstack-cluster-tokens", "mitstack-cluster-application"})
	clusterDocs := testutil.Kind(docs, "KubeClusterConfig")
	require.Len(t, clusterDocs, 1, "KubeClusterConfig documents")
	networks, err := e.hetzner.ListNetworks(e.ctx(), subscriptionSelector(testSub))
	require.NoError(t, err)
	require.Len(t, networks, 1)
	endpoint := testutil.Field(t, clusterDocs[0], "endpoint")
	assert.Equal(t, "https://"+e.hetzner.loadBalancer(t, roleAPI).PrivateIPs[networks[0].ID]+":6443", endpoint, "the control-plane endpoint")
	assert.Equal(t, []string{builderScriptPath, image.PinnedImager, builderConfigPath, routerAddress}, c.RunCmd[0], "the build command")
}

func TestBuilder_TheAPIServersCertificateNamesTheAPILoadBalancer(t *testing.T) {
	e := subscribedOnce(t)
	config := decodeCloudConfig(t, e.hetzner.userData[e.hetzner.byRole(t, roleBuilder)[0].ID]).file(t, builderConfigPath)
	networks, err := e.hetzner.ListNetworks(e.ctx(), subscriptionSelector(testSub))
	require.NoError(t, err)
	require.Len(t, networks, 1)
	want := []any{e.hetzner.loadBalancer(t, roleAPI).PrivateIPs[networks[0].ID]}

	docs := testutil.Documents(t, []byte(config.Content))

	assert.Equal(t, want, testutil.Field(t, docs[0], "machine", "certSANs"), "the Talos API's certificate names")
	apiServer := testutil.Kind(docs, "KubeAPIServerConfig")
	require.Len(t, apiServer, 1, "KubeAPIServerConfig documents")
	assert.Equal(t, want, testutil.Field(t, apiServer[0], "certExtraSANs"), "the Kubernetes API's certificate names")
}

func TestBuilder_TheApplicationSyncsTheClusterRepositoryAndTheEnrolmentNamesThePlatform(t *testing.T) {
	e := subscribedOnce(t)

	manifests := inlineManifests(t, builderConfiguration(t, e))

	apps := testutil.Kind(manifests["mitstack-cluster-application"], "Application")
	require.Len(t, apps, 2, "Applications")
	for _, app := range apps {
		assert.Equal(t, "https://github.com/"+e.repo(), testutil.Field(t, app, "spec", "source", "repoURL"), "Application %v's repository", app["metadata"])
	}
	jobs := testutil.Kind(manifests["mitstack-cluster-tokens"], "Job")
	require.Len(t, jobs, 1, "enrolment Jobs")
	container := testutil.Field(t, jobs[0], "spec", "template", "spec", "containers", "0")
	assert.Equal(t, tools.Image, testutil.Field(t, container, "image"), "the enrolment's image")
	assert.Contains(t, testutil.Field(t, container, "env"), map[string]any{"name": "PLATFORM_URL", "value": platformURL})
	cronJobs := testutil.Kind(manifests["mitstack-node-report"], "CronJob")
	require.Len(t, cronJobs, 1, "node report CronJobs")
	report := testutil.Field(t, cronJobs[0], "spec", "jobTemplate", "spec", "template", "spec", "containers", "0")
	assert.Equal(t, tools.Image, testutil.Field(t, report, "image"), "the node report's image")
}

func TestGateway_RunsTheKitsCloudConfigAndThenBootstrapsTheFirstNodeWithAShortLivedAdminTalosconfig(t *testing.T) {
	e := subscribedOnce(t)
	gw := e.hetzner.byRole(t, roleGateway)[0]

	parts := gatewayParts(t, e.hetzner.userData[gw.ID])

	require.Len(t, parts, 2, "user data parts")
	kit := decodeCloudConfig(t, parts[0])
	kit.file(t, "/usr/local/bin/mitstack-talos-bootstrap")
	bootstrap := decodeCloudConfig(t, parts[1])
	assert.Equal(t, []mergeRule{{Name: "list", Settings: []string{"append"}}, {Name: "dict", Settings: []string{"no_replace", "recurse_list"}}}, bootstrap.MergeHow)
	talosconfig := bootstrap.file(t, bootstrapTalosconfigPath)
	assert.Equal(t, "0600", talosconfig.Permissions)
	cert := adminCertificate(t, talosconfig.Content)
	assert.WithinDuration(t, testStart.Add(adminTalosconfigTTL), cert.NotAfter, time.Minute, "the admin certificate's expiry")
	assert.Equal(t, []string{"systemctl", "start", "--no-block", bootstrapUnit}, bootstrap.RunCmd[0])
	assert.LessOrEqual(t, len(e.hetzner.userData[gw.ID]), 32*1024, "Hetzner's user data limit")
}

func TestGateway_StartsTheMirrorBesideItsFirewallAndPassesPullsThroughIt(t *testing.T) {
	e := subscribedOnce(t)

	kit := decodeCloudConfig(t, gatewayParts(t, e.hetzner.userData[e.hetzner.byRole(t, roleGateway)[0].ID])[0])

	assert.Contains(t, kit.file(t, "/etc/mitstack-gateway/Caddyfile").Content,
		"  route /v2/* {\n    reverse_proxy @pull 127.0.0.1:5000\n    respond 405\n  }\n", "the mirror tries the registry for a pull before refusing")
	assert.JSONEq(t, `{"iptables": false, "ip6tables": false, "bridge": "none"}`, kit.file(t, "/etc/docker/daemon.json").Content,
		"Docker writes iptables rules the gateway's nftables deletes, so the mirror never starts")
}

func TestBuilder_TheNodeRoutesThroughTheNetworksRouterAndKeepsItsDHCPAddress(t *testing.T) {
	e := subscribedOnce(t)

	docs := builderConfiguration(t, e)

	assert.Equal(t, []map[string]any{{"apiVersion": "v1alpha1", "kind": "LinkConfig", "name": "eth0",
		"routes": []any{map[string]any{"gateway": routerAddress}}}}, testutil.Kind(docs, "LinkConfig"), "the node's default route")
	assert.Equal(t, []map[string]any{{"apiVersion": "v1alpha1", "kind": "DHCPv4Config", "name": "eth0"}}, testutil.Kind(docs, "DHCPv4Config"),
		"the node's DHCP, which its LinkConfig would otherwise stop")
}

func TestBuilder_TheClusterRunsKyvernoAndCloudNativePGFromManifestsTheGatewayServes(t *testing.T) {
	e := subscribedOnce(t)

	config := builderConfiguration(t, e)

	served := map[string]any{}
	for _, doc := range testutil.Kind(config, "KubeExternalManifestConfig") {
		served[doc["name"].(string)] = doc["url"]
	}
	for _, name := range []string{"kyverno", "cloudnative-pg"} {
		assert.Equal(t, "https://"+gatewayAddress+"/manifests/"+name+".yaml", served[name], "where the node fetches the %s manifest", name)
	}
}

func TestBuilder_TheClusterAdmitsOnlyLabelledPodsIsolatesEachAppNamespaceAndAdmitsDatabaseSlices(t *testing.T) {
	e := subscribedOnce(t)

	manifests := inlineManifests(t, builderConfiguration(t, e))

	for _, want := range []struct{ manifest, kind, name string }{
		{"mitstack-require-app-label", "ValidatingPolicy", "mitstack-require-app-label"},
		{"mitstack-generate-default-deny", "GeneratingPolicy", "mitstack-generate-default-deny"},
		{"cloudnative-pg-namespace", "Namespace", "cnpg-system"},
		{"mitstack-database-slices", "CiliumClusterwideNetworkPolicy", "mitstack-database-slices"},
	} {
		docs := testutil.Kind(manifests[want.manifest], want.kind)
		if assert.Len(t, docs, 1, "%s documents in the inline manifest %s", want.kind, want.manifest) {
			assert.Equal(t, want.name, testutil.Field(t, docs[0], "metadata", "name"), "the %s in %s", want.kind, want.manifest)
		}
	}
}

type workload struct {
	namespace, name string
	podLabels       map[string]any
}

func workloads(t *testing.T, docs []map[string]any) []workload {
	t.Helper()
	var found []workload
	for _, doc := range docs {
		var template any
		switch doc["kind"] {
		case "Job", "Deployment", "DaemonSet", "StatefulSet":
			template = testutil.Field(t, doc, "spec", "template")
		case "CronJob":
			template = testutil.Field(t, doc, "spec", "jobTemplate", "spec", "template")
		default:
			continue
		}
		metadata, _ := template.(map[string]any)["metadata"].(map[string]any)
		labels, _ := metadata["labels"].(map[string]any)
		found = append(found, workload{
			namespace: testutil.Field(t, doc, "metadata", "namespace").(string),
			name:      testutil.Field(t, doc, "metadata", "name").(string),
			podLabels: labels,
		})
	}
	return found
}

// exemptNamespaces are the namespaces a policy's namespaceSelector leaves out by name.
func exemptNamespaces(t *testing.T, policy map[string]any) []string {
	t.Helper()
	var exempt []string
	for _, expr := range testutil.Field(t, policy, "spec", "matchConstraints", "namespaceSelector", "matchExpressions").([]any) {
		if m := expr.(map[string]any); m["key"] == "kubernetes.io/metadata.name" && m["operator"] == "NotIn" {
			for _, ns := range m["values"].([]any) {
				exempt = append(exempt, ns.(string))
			}
		}
	}
	return exempt
}

func TestBuilder_TheBlocksOwnPodsPassTheAppLabelPolicyAndKeepTheirEgress(t *testing.T) {
	e := subscribedOnce(t)
	manifests := inlineManifests(t, builderConfiguration(t, e))
	labelPolicies := testutil.Kind(manifests["mitstack-require-app-label"], "ValidatingPolicy")
	require.Len(t, labelPolicies, 1, "the app label policy")
	denyPolicies := testutil.Kind(manifests["mitstack-generate-default-deny"], "GeneratingPolicy")
	require.Len(t, denyPolicies, 1, "the default-deny policy")

	exempt := exemptNamespaces(t, labelPolicies[0])
	denyCondition := testutil.Field(t, denyPolicies[0], "spec", "matchConditions", "0", "expression").(string)
	own := append(workloads(t, manifests["mitstack-node-report"]), workloads(t, manifests["mitstack-cluster-tokens"])...)

	require.NotEmpty(t, own, "the block's workloads")
	for _, w := range own {
		if w.podLabels["mitstack.dev/app"] == nil {
			assert.Contains(t, exempt, w.namespace, "%s/%s's pods carry no mitstack.dev/app label, so their namespace must be exempt", w.namespace, w.name)
		}
		assert.Contains(t, denyCondition, "'"+w.namespace+"'", "%s/%s reaches the platform, so its namespace must get no default-deny", w.namespace, w.name)
	}
}

func TestBuilder_TheNodeReportMayReadCloudNativePGsClustersDatabasesAndRoles(t *testing.T) {
	e := subscribedOnce(t)

	roles := testutil.Kind(inlineManifests(t, builderConfiguration(t, e))["mitstack-node-report"], "ClusterRole")

	require.Len(t, roles, 1, "the node report's ClusterRoles")
	assert.Contains(t, testutil.Field(t, roles[0], "rules"), map[string]any{
		"apiGroups": []any{"postgresql.cnpg.io"},
		"resources": []any{"clusters", "databases", "databaseroles"},
		"verbs":     []any{"get", "list"},
	}, "the node report's read of CloudNativePG's objects")
}

func TestGateway_MirrorsCloudNativePGAndKyvernoByDigestAndFetchesTheirManifestsFromTheKitRelease(t *testing.T) {
	e := subscribedOnce(t)
	release, err := kitRelease()
	require.NoError(t, err)

	kit := decodeCloudConfig(t, gatewayParts(t, e.hetzner.userData[e.hetzner.byRole(t, roleGateway)[0].ID])[0])

	mirrored := map[string]string{}
	for _, ref := range strings.Fields(kit.file(t, "/etc/mitstack-gateway/images.txt").Content) {
		image, err := components.ParseImage(ref)
		require.NoError(t, err, "the mirror's image list")
		mirrored[image.Name] = image.Digest
	}
	for _, repo := range []string{
		"ghcr.io/cloudnative-pg/cloudnative-pg", "ghcr.io/cloudnative-pg/postgresql",
		"ghcr.io/kyverno/kyverno", "ghcr.io/kyverno/kyvernopre", "ghcr.io/kyverno/background-controller",
	} {
		assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, mirrored[repo], "the mirror's digest for %s", repo)
	}
	fetched := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(kit.file(t, "/etc/mitstack-gateway/manifests.txt").Content), "\n") {
		fields := strings.Fields(line)
		require.Len(t, fields, 3, "manifest line %q is <file> <sha256> <url>", line)
		fetched[fields[0]] = fields[2]
	}
	assert.Equal(t, gateway.ReleaseURL(release)+"/components/kyverno/kyverno.yaml", fetched["kyverno.yaml"], "where the gateway fetches Kyverno")
	assert.Equal(t, gateway.ReleaseURL(release)+"/components/cloudnative-pg/cloudnative-pg.yaml", fetched["cloudnative-pg.yaml"], "where the gateway fetches CloudNativePG")
}

func TestCluster_KeepsEachClaimOnItsNodesUserVolumeThroughTheLocalPathProvisioner(t *testing.T) {
	e := subscribedOnce(t)
	release, err := kitRelease()
	require.NoError(t, err)

	config := builderConfiguration(t, e)
	kit := decodeCloudConfig(t, gatewayParts(t, e.hetzner.userData[e.hetzner.byRole(t, roleGateway)[0].ID])[0])

	assert.Contains(t, testutil.Kind(config, "UserVolumeConfig"),
		map[string]any{"apiVersion": "v1alpha1", "kind": "UserVolumeConfig", "name": components.LocalPathVolume, "volumeType": "directory"})
	assert.Contains(t, strings.Fields(kit.file(t, "/etc/mitstack-gateway/manifests.txt").Content),
		gateway.ReleaseURL(release)+"/components/local-path-provisioner/local-path-provisioner.yaml", "where the gateway fetches the provisioner")
	assert.Subset(t, mirroredImageNames(t, kit), []string{"docker.io/rancher/local-path-provisioner", "docker.io/library/busybox"}, "the provisioner's and its helper's images")
}

func mirroredImageNames(t *testing.T, kit renderedCloudConfig) []string {
	t.Helper()
	var names []string
	for _, ref := range strings.Fields(kit.file(t, "/etc/mitstack-gateway/images.txt").Content) {
		image, err := components.ParseImage(ref)
		require.NoError(t, err, "the mirror's image list")
		names = append(names, image.Name)
	}
	return names
}

func TestCluster_PutsEveryPodInIstiosAmbientMeshAndRefusesACallWithoutAWorkloadIdentity(t *testing.T) {
	e := subscribedOnce(t)
	release, err := kitRelease()
	require.NoError(t, err)

	manifests := inlineManifests(t, builderConfiguration(t, e))
	kit := decodeCloudConfig(t, gatewayParts(t, e.hetzner.userData[e.hetzner.byRole(t, roleGateway)[0].ID])[0])

	fetched := strings.Fields(kit.file(t, "/etc/mitstack-gateway/manifests.txt").Content)
	for _, file := range []string{"istio-base", "istiod", "istio-cni", "ztunnel"} {
		assert.Contains(t, fetched, gateway.ReleaseURL(release)+"/components/istio/"+file+".yaml", "where the gateway fetches %s", file)
	}
	assert.Subset(t, mirroredImageNames(t, kit), []string{"docker.io/istio/pilot", "docker.io/istio/install-cni", "docker.io/istio/ztunnel"}, "Istio's images")
	strict := testutil.Kind(manifests["mitstack-strict-mtls"], "PeerAuthentication")
	require.Len(t, strict, 1, "the mesh-wide policy")
	assert.Equal(t, "STRICT", testutil.Field(t, strict[0], "spec", "mtls", "mode"))
	for _, policy := range []string{"mitstack-require-mesh", "mitstack-require-strict-mtls"} {
		assert.Len(t, testutil.Kind(manifests[policy], "ValidatingPolicy"), 1, "the admission policy %s", policy)
	}
}

func adminCertificate(t *testing.T, talosconfig string) *x509.Certificate {
	t.Helper()
	var cfg struct {
		Contexts map[string]struct {
			Crt string `yaml:"crt"`
		} `yaml:"contexts"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(talosconfig), &cfg))
	raw, err := base64.StdEncoding.DecodeString(cfg.Contexts["admin"].Crt)
	require.NoError(t, err)
	block, _ := pem.Decode(raw)
	require.NotNil(t, block, "the admin certificate is PEM")
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return cert
}

func TestInstall_KeepsTheClustersKeysOutOfTheOutputsAndTheStatus(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.converge())
	builder := e.hetzner.byRole(t, roleBuilder)[0]
	config := decodeCloudConfig(t, e.hetzner.userData[builder.ID]).file(t, builderConfigPath)
	machine := testutil.Documents(t, []byte(config.Content))[0]
	keys := []string{
		testutil.Field(t, machine, "machine", "token").(string),
		testutil.Field(t, machine, "machine", "ca", "key").(string),
		testutil.Field(t, machine, "cluster", "token").(string),
	}
	e.hetzner.instantBuild = true
	e.hetzner.powerOffBuilder(t)
	require.NoError(t, e.converge())
	e.reports.report(e.now.Add(time.Minute), true, true, true)
	e.now = e.now.Add(2 * time.Minute)

	obs := e.observe()

	require.Equal(t, contract.Present, obs.State, "detail: %s", obs.StatusDetail)
	for _, key := range keys {
		for name, value := range obs.Outputs {
			assert.NotContains(t, value, key, "output %s carries a cluster key", name)
		}
		assert.NotContains(t, obs.StatusDetail, key, "the status carries a cluster key")
	}
}

func TestBuilder_UserDataIsOneCloudConfigDocumentWithinHetznersLimit(t *testing.T) {
	e := subscribedOnce(t)
	data := e.hetzner.userData[e.hetzner.byRole(t, roleBuilder)[0].ID]

	decoder := yaml.NewDecoder(strings.NewReader(data))
	var docs []map[string]any
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		docs = append(docs, doc)
	}

	t.Logf("the builder's user data is %d bytes", len(data))
	assert.LessOrEqual(t, len(data), hetznerUserDataLimit, "the builder's user data is %d bytes; Hetzner accepts %d", len(data), hetznerUserDataLimit)
	assert.True(t, strings.HasPrefix(data, "#cloud-config\n"), "the builder's user data opens with the cloud-config header")
	assert.Len(t, docs, 1, "YAML documents in the builder's user data")
}

func TestBuilder_UserDataOverHetznersLimitFailsTheRender(t *testing.T) {
	incompressible := make([]byte, 2*hetznerUserDataLimit)
	_, err := rand.Read(incompressible)
	require.NoError(t, err)

	_, err = builderUserData(incompressible)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "over Hetzner's 32768")
}

func TestBuilder_TheNodeReportDeclaresItsContractAndCarriesNoneOfTheVerdictsChecks(t *testing.T) {
	posted := runNodeReport(t, nodeReportScript(t))

	assert.Equal(t, map[string]any{"contract": "node-report/v1", "names": []any{}}, posted["checks"], "the node report's checks member")
}

func TestBuilder_TheNodeReportPostsAnErrorNamingTheReadThatFailed(t *testing.T) {
	posted, out, err := postNodeReport(t, nodeReportScript(t), `[ "$*" != "get nodes -o json" ] || { echo 'error: forbidden' >&2; exit 1; }; echo '{"items": []}'`)

	require.Error(t, err, "the node report exits non-zero once it has posted the error")
	require.NotNil(t, posted, "the node report posted nothing: %s", out)
	assert.Equal(t, map[string]any{"read": "kubectl get nodes", "exitStatus": float64(1), "stderr": "error: forbidden\n"}, posted["error"])
}

func TestBuilder_TheNodeReportNamesEachKindItReadWholeAndMayReadNoSecret(t *testing.T) {
	e := subscribedOnce(t)
	roles := testutil.Kind(inlineManifests(t, builderConfiguration(t, e))["mitstack-node-report"], "ClusterRole")

	posted := runNodeReport(t, nodeReportScript(t))

	assert.IsType(t, "", posted["madeAt"], "the node report's madeAt")
	assert.Equal(t, []any{}, posted["subscriptionResources"], "a cluster with no labelled objects lists none")
	kinds, _ := posted["subscriptionResourceKinds"].([]any)
	assert.Subset(t, kinds, []any{"namespace", "deployment.apps", "appproject.argoproj.io", "externalsecret.external-secrets.io"},
		"the kinds the node report read whole")
	assert.NotContains(t, posted, "subscriptionResourcesComplete")
	require.Len(t, roles, 1, "the node report's ClusterRoles")
	for _, rule := range testutil.Field(t, roles[0], "rules").([]any) {
		assert.NotContains(t, testutil.Field(t, rule, "resources"), "secrets", "rule %v grants a verb on secrets", rule)
	}
}

func TestBuilder_EveryNodeEncryptsStateAndEphemeralUnderItsNodeIDKeyAndTheReportNamesIt(t *testing.T) {
	e := subscribedOnce(t)
	volumes := map[string]any{}
	for _, doc := range testutil.Kind(builderConfiguration(t, e), "VolumeConfig") {
		volumes[doc["name"].(string)] = doc["encryption"]
	}

	posted := runNodeReport(t, nodeReportScript(t))

	assert.Equal(t, map[string]any{
		"STATE":     map[string]any{"provider": "luks2", "keys": []any{map[string]any{"slot": 0, "nodeID": map[string]any{}}}},
		"EPHEMERAL": map[string]any{"provider": "luks2", "keys": []any{map[string]any{"slot": 0, "nodeID": map[string]any{}, "lockToState": true}}},
	}, volumes, "the encryption of each system volume in the image's configuration")
	assert.Contains(t, posted, "systemVolumes", "the node report names each node's volume encryption")
}

// nodeReportScript returns the script the builder's node report CronJob runs.
func nodeReportScript(t *testing.T) string {
	t.Helper()
	cronJobs := testutil.Kind(inlineManifests(t, builderConfiguration(t, subscribedOnce(t)))["mitstack-node-report"], "CronJob")
	require.Len(t, cronJobs, 1, "node report CronJobs")
	return testutil.Field(t, cronJobs[0], "spec", "jobTemplate", "spec", "template", "spec", "containers", "0", "command", "2").(string)
}

// runNodeReport runs script against a cluster with no objects and returns the body the platform received.
func runNodeReport(t *testing.T, script string) map[string]any {
	t.Helper()
	posted, out, err := postNodeReport(t, script, `echo '{"items": []}'`)

	require.NoError(t, err, out)
	require.NotNil(t, posted, "the node report posted nothing: %s", out)
	return posted
}

// postNodeReport runs script with the given kubectl stub and returns the body the platform received, or nil.
func postNodeReport(t *testing.T, script, kubectl string) (map[string]any, string, error) {
	t.Helper()
	posted := make(chan map[string]any, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		posted <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	ca, token := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(ca, certPEM(srv), 0o644))
	require.NoError(t, os.WriteFile(token, []byte("report-token"), 0o600))
	tools := testutil.NewTools(t, map[string]string{
		"kubectl":  kubectl,
		"talosctl": `echo 'NODE ID HOSTNAME PEER-URLS CLIENT-URLS LEARNER'`,
	})

	out, err := testutil.RunBash(t, script, tools.Env("REPORT_URL="+srv.URL, "SUBSCRIPTION_ID="+testSub, "HOST_IP=10.0.0.3",
		"REPORT_TOKEN_FILE="+token, "CURL_CA_BUNDLE="+ca))

	select {
	case body := <-posted:
		return body, out, err
	default:
		return nil, out, err
	}
}

// recordedToolsImage stands in for tools.Image, which every kit release pins anew.
const recordedToolsImage = "ghcr.io/mitstack-eu/talos-cluster-kit-tools:recorded@sha256:0000000000000000000000000000000000000000000000000000000000000000"

func TestCluster_RendersTheRecordedComponentsMirrorAndBuilderApartFromTheNodeReport(t *testing.T) {
	swap(t, &toolsImage, func() string { return recordedToolsImage })

	got := defaultClusterRecord(t)

	testutil.Golden(t, "testdata/default-cluster.golden", got)
}

// defaultClusterRecord lists the component set, the gateway's mirror and the builder's cloud-config and documents, one per line.
func defaultClusterRecord(t *testing.T) []byte {
	t.Helper()
	set, err := clusterComponents(manifestInputs{subID: testSub, repoURL: "https://github.com/" + testOwner + "/shop-cluster", enrolmentToken: "enrolment-token"})
	require.NoError(t, err)
	e := subscribedOnce(t)
	var out strings.Builder
	record := func(fields ...string) {
		out.WriteString(strings.Join(slices.DeleteFunc(fields, func(f string) bool { return f == "" }), " ") + "\n")
	}
	record("versions", set.Versions.Talos, set.Versions.Kubernetes)
	for _, m := range set.Served {
		record("served", m.Name, m.Path, m.SHA256())
	}
	for _, m := range set.Inline {
		digest := m.SHA256()
		if m.Name == "mitstack-node-report" {
			digest = "unrecorded"
		}
		record("inline", m.Name, m.Path, digest)
	}
	gw := decodeCloudConfig(t, gatewayParts(t, e.hetzner.userData[e.hetzner.byRole(t, roleGateway)[0].ID])[0])
	for _, ref := range strings.Fields(gw.file(t, "/etc/mitstack-gateway/images.txt").Content) {
		record("mirror", ref)
	}
	builder := decodeCloudConfig(t, e.hetzner.userData[e.hetzner.byRole(t, roleBuilder)[0].ID])
	for _, f := range builder.WriteFiles {
		record("builder-file", f.Path, f.Permissions, f.Encoding)
	}
	for _, cmd := range builder.RunCmd {
		record(append([]string{"builder-run"}, cmd...)...)
	}
	for _, doc := range builderConfiguration(t, e) {
		kind, _ := doc["kind"].(string)
		version, _ := doc["version"].(string)
		name, _ := doc["name"].(string)
		record("document", cmp.Or(kind, version), name)
	}
	return []byte(out.String())
}
