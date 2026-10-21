package main

// The builder embeds the kit's configuration with the block's own manifests, and the gateway runs the kit's cloud-config.

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/mitstack-eu/operator-kit/v2/contract"
	"github.com/mitstack-eu/talos-cluster-kit/image"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/mitstack-eu/talos-cluster-kit/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

type renderedCloudConfig struct {
	MergeHow   []mergeRule `yaml:"merge_how"`
	WriteFiles []cloudFile `yaml:"write_files"`
	RunCmd     [][]string  `yaml:"runcmd"`
}

func decodeCloudConfig(t *testing.T, data string) renderedCloudConfig {
	t.Helper()
	require.True(t, strings.HasPrefix(data, "#cloud-config\n"), "a cloud-config opens with its header")
	var c renderedCloudConfig
	require.NoError(t, yaml.Unmarshal([]byte(data), &c))
	return c
}

func (c renderedCloudConfig) file(t *testing.T, path string) cloudFile {
	t.Helper()
	for _, f := range c.WriteFiles {
		if f.Path == path {
			return f
		}
	}
	require.Failf(t, "no such file", "the cloud-config writes no %s", path)
	return cloudFile{}
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
	require.NoError(t, e.ensure())
	return e
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

func TestBuilder_TheApplicationSyncsTheClusterRepositoryAndTheEnrolmentNamesThePlatform(t *testing.T) {
	e := subscribedOnce(t)
	config := decodeCloudConfig(t, e.hetzner.userData[e.hetzner.byRole(t, roleBuilder)[0].ID]).file(t, builderConfigPath)

	manifests := map[string][]map[string]any{}
	for _, doc := range testutil.Kind(testutil.Documents(t, []byte(config.Content)), "KubeInlineManifestConfig") {
		manifests[doc["name"].(string)] = testutil.Documents(t, []byte(doc["manifest"].(string)))
	}

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
	require.NoError(t, e.ensure())
	builder := e.hetzner.byRole(t, roleBuilder)[0]
	config := decodeCloudConfig(t, e.hetzner.userData[builder.ID]).file(t, builderConfigPath)
	machine := testutil.Documents(t, []byte(config.Content))[0]
	keys := []string{
		testutil.Field(t, machine, "machine", "token").(string),
		testutil.Field(t, machine, "machine", "ca", "key").(string),
		testutil.Field(t, machine, "cluster", "token").(string),
	}
	e.hetzner.powerOffBuilder(t)
	require.NoError(t, e.ensure())
	e.reports.report(e.now.Add(time.Minute), true)
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
