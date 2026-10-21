package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"slices"
	"strings"
	"text/template"

	"github.com/mitstack-eu/talos-cluster-kit/components"
	"github.com/mitstack-eu/talos-cluster-kit/nodeconfig"
	"github.com/mitstack-eu/talos-cluster-kit/report"
)

var (
	//go:embed manifests/cluster-tokens.yaml
	clusterTokensTemplate string
	//go:embed manifests/cluster-application.yaml
	clusterApplicationTemplate string
	//go:embed scripts/cluster-tokens.sh
	clusterTokensScript string
	//go:embed tools-image.txt
	toolsImageFile string
)

// toolsImage is the kit's tools image as this block's CI published it, by digest, or empty until a release pins it.
var toolsImage = func() string { return strings.TrimSpace(toolsImageFile) }

var templateFuncs = template.FuncMap{"indent": indent}

var (
	clusterTokens      = template.Must(template.New("cluster-tokens").Funcs(templateFuncs).Parse(clusterTokensTemplate))
	clusterApplication = template.Must(template.New("cluster-application").Funcs(templateFuncs).Parse(clusterApplicationTemplate))
)

const (
	clusterReportsPath = "/v1/cluster-reports"
	manifestDir        = "manifests/"
)

type manifestInputs struct {
	subID, repoURL, enrolmentToken string
}

// clusterComponents adds the block's manifests inline, so the configuration carries them and the gateway mirrors their image.
func clusterComponents(in manifestInputs) (components.Set, error) {
	set := components.Default()
	reportManifests, err := report.Manifests(report.Inputs{URL: platformURL + clusterReportsPath, SubscriptionID: in.subID, ToolsImage: toolsImage()})
	if err != nil {
		return components.Set{}, err
	}
	data := map[string]string{
		"Namespace":      nodeconfig.TalosAPINamespace,
		"Image":          toolsImage(),
		"Script":         clusterTokensScript,
		"PlatformURL":    platformURL,
		"SubscriptionID": in.subID,
		"RepoURL":        in.repoURL,
		"EnrolmentToken": in.enrolmentToken,
	}
	tokens, err := execute(clusterTokens, data)
	if err != nil {
		return components.Set{}, err
	}
	application, err := execute(clusterApplication, data)
	if err != nil {
		return components.Set{}, err
	}
	set.Inline = append(slices.Clone(set.Inline),
		components.Manifest{Name: "mitstack-node-report", Path: "report/report.yaml", Bytes: reportManifests},
		components.Manifest{Name: "mitstack-cluster-tokens", Path: manifestDir + "cluster-tokens.yaml", Bytes: tokens},
		components.Manifest{Name: "mitstack-cluster-application", Path: manifestDir + "cluster-application.yaml", Bytes: application},
	)
	return set, nil
}

func execute(t *template.Template, data map[string]string) ([]byte, error) {
	var out bytes.Buffer
	if err := t.Execute(&out, data); err != nil {
		return nil, fmt.Errorf("rendering %s: %w", t.Name(), err)
	}
	return out.Bytes(), nil
}

func indent(n int, s string) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = pad + line
		}
	}
	return strings.Join(lines, "\n")
}
