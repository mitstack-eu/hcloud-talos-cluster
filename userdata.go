package main

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/base64"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"strconv"
	"time"

	"github.com/mitstack-eu/talos-cluster-kit/gateway"
	"github.com/mitstack-eu/talos-cluster-kit/image"
	"go.yaml.in/yaml/v4"
)

var (
	//go:embed scripts/build-image.sh
	buildImageScript string
	//go:embed scripts/bootstrap-first-node.sh
	bootstrapFirstNodeScript string
)

const (
	cloudConfigHeader = "#cloud-config\n"
	userDataBoundary  = "mitstack-hcloud-talos-cluster"

	builderConfigPath    = "/root/mitstack-build/config.yaml"
	builderScriptPath    = "/usr/local/bin/mitstack-build-image"
	builderGatewayCAPath = "/root/mitstack-build/gateway-ca.crt"

	bootstrapTalosconfigPath = "/etc/mitstack-gateway/admin-talosconfig"
	bootstrapScriptPath      = "/usr/local/bin/mitstack-bootstrap-first-node"
	bootstrapUnit            = "mitstack-bootstrap-first-node.service"
	// subnetPrefix is how every address of the cluster's subnet begins, which the bootstrap looks for its node among.
	subnetPrefix = "10.0.0."
)

type cloudFile struct {
	Path        string `yaml:"path"`
	Permissions string `yaml:"permissions"`
	Content     string `yaml:"content"`
	Encoding    string `yaml:"encoding,omitempty"`
}

// mergeRule tells cloud-init to append this part's lists to the kit's, so neither part replaces the other's files or commands.
type mergeRule struct {
	Name     string   `yaml:"name"`
	Settings []string `yaml:"settings"`
}

type cloudConfig struct {
	MergeHow   []mergeRule `yaml:"merge_how,omitempty"`
	WriteFiles []cloudFile `yaml:"write_files"`
	RunCmd     [][]string  `yaml:"runcmd"`
}

func (c cloudConfig) render() (string, error) {
	out, err := yaml.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encoding a cloud-config: %w", err)
	}
	return cloudConfigHeader + string(out), nil
}

// builderUserData holds every key of the cluster, which is why the builder is deleted once its disk is a snapshot.
// The configuration and the script are gzipped, since as text they pass Hetzner's user data limit, and a result still
// over it is refused. The builder writes its disk once mirrorURL answers under gatewayCA.
func builderUserData(machineConfig, gatewayCA []byte, mirrorURL string) (string, error) {
	config, err := gzipBase64(machineConfig)
	if err != nil {
		return "", err
	}
	script, err := gzipBase64([]byte(buildImageScript))
	if err != nil {
		return "", err
	}
	out, err := cloudConfig{
		WriteFiles: []cloudFile{
			{Path: builderConfigPath, Permissions: "0600", Content: config, Encoding: gzipBase64Encoding},
			{Path: builderScriptPath, Permissions: "0755", Content: script, Encoding: gzipBase64Encoding},
			{Path: builderGatewayCAPath, Permissions: "0644", Content: string(gatewayCA)},
		},
		RunCmd: [][]string{{builderScriptPath, image.PinnedImager, builderConfigPath, routerAddress, mirrorURL, builderGatewayCAPath}},
	}.render()
	if err != nil {
		return "", err
	}
	if len(out) > gateway.UserDataLimit {
		return "", fmt.Errorf("the builder's user data is %d bytes, over Hetzner's %d", len(out), gateway.UserDataLimit)
	}
	return out, nil
}

// gzipBase64Encoding is the write_files encoding cloud-init decodes as base64 and then gunzips.
const gzipBase64Encoding = "gz+b64"

func gzipBase64(content []byte) (string, error) {
	var zipped bytes.Buffer
	w, err := gzip.NewWriterLevel(&zipped, gzip.BestCompression)
	if err != nil {
		return "", fmt.Errorf("compressing the builder's user data: %w", err)
	}
	if _, err := w.Write(content); err != nil {
		return "", fmt.Errorf("compressing the builder's user data: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("compressing the builder's user data: %w", err)
	}
	return base64.StdEncoding.EncodeToString(zipped.Bytes()), nil
}

// firstNodeBootstrap is the gateway's one etcd bootstrap: an admin talosconfig and the time it expires.
type firstNodeBootstrap struct {
	talosconfig []byte
	deadline    time.Time
}

func (b firstNodeBootstrap) cloudConfig() (string, error) {
	unit := "[Unit]\nDescription=Bootstraps etcd on the cluster's first node once it boots\n" +
		"[Service]\nType=oneshot\nExecStart=" + bootstrapScriptPath + " " + subnetPrefix + " " + gatewayAddress + " " +
		bootstrapTalosconfigPath + " " + strconv.FormatInt(b.deadline.Unix(), 10) + "\n"
	return cloudConfig{
		MergeHow: []mergeRule{{Name: "list", Settings: []string{"append"}}, {Name: "dict", Settings: []string{"no_replace", "recurse_list"}}},
		WriteFiles: []cloudFile{
			{Path: bootstrapTalosconfigPath, Permissions: "0600", Content: string(b.talosconfig)},
			{Path: bootstrapScriptPath, Permissions: "0755", Content: bootstrapFirstNodeScript},
			{Path: "/etc/systemd/system/" + bootstrapUnit, Permissions: "0644", Content: unit},
		},
		RunCmd: [][]string{{"systemctl", "start", "--no-block", bootstrapUnit}},
	}.render()
}

// gatewayUserData joins the kit's cloud-config and the bootstrap's as two MIME parts, which cloud-init merges in order,
// and refuses a result past Hetzner's user data limit.
func gatewayUserData(kitPart []byte, bootstrap firstNodeBootstrap) (string, error) {
	bootstrapPart, err := bootstrap.cloudConfig()
	if err != nil {
		return "", err
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.SetBoundary(userDataBoundary); err != nil {
		return "", fmt.Errorf("setting the user data boundary: %w", err)
	}
	for _, part := range []struct{ name, content string }{{"gateway.yaml", string(kitPart)}, {"bootstrap.yaml", bootstrapPart}} {
		pw, err := w.CreatePart(textproto.MIMEHeader{
			"Content-Type":        {`text/cloud-config; charset="us-ascii"`},
			"Content-Disposition": {`attachment; filename="` + part.name + `"`},
		})
		if err != nil {
			return "", fmt.Errorf("writing the user data part %s: %w", part.name, err)
		}
		if _, err := pw.Write([]byte(part.content)); err != nil {
			return "", fmt.Errorf("writing the user data part %s: %w", part.name, err)
		}
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("closing the user data: %w", err)
	}
	out := "Content-Type: multipart/mixed; boundary=\"" + userDataBoundary + "\"\nMIME-Version: 1.0\n\n" + body.String()
	if len(out) > gateway.UserDataLimit {
		return "", fmt.Errorf("the gateway's user data is %d bytes, over Hetzner's %d", len(out), gateway.UserDataLimit)
	}
	return out, nil
}
