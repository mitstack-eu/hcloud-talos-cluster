package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/mitstack-eu/operator-kit/v2/contract"
	"github.com/mitstack-eu/operator-kit/v2/events"
	"github.com/mitstack-eu/operator-kit/v2/observe"
)

// The platform keeps 50 reports of up to 256 KiB each and answers each with its verdict.
const maxNodeReportsBody = 32 << 20

var nodeReportsHTTP = &http.Client{Timeout: 10 * time.Second}

// nodeReports reads a subscription's node reports; a test swaps it.
var nodeReports observe.NodeReportsAPI = nodeReportsClient{}

// nodeReportsClient lists a subscription's node reports from the platform, with the request's events callback token.
type nodeReportsClient struct{}

func (nodeReportsClient) NodeReports(ctx context.Context, _ contract.Desired, subscriptionID string) ([]observe.NodeReport, error) {
	origin, err := platformOrigin(ctx)
	if err != nil {
		return nil, err
	}
	env, _ := events.EnvelopeFrom(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/v1/subscriptions/"+url.PathEscape(subscriptionID)+"/node-reports", nil)
	if err != nil {
		return nil, fmt.Errorf("building the node reports request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+env.Callback.Token)
	resp, err := nodeReportsHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reading the node reports: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the platform answered %d for the node reports", resp.StatusCode)
	}
	var listed struct {
		Reports []struct {
			ReceivedAt time.Time       `json:"receivedAt"`
			Body       json.RawMessage `json:"body"`
		} `json:"reports"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxNodeReportsBody)).Decode(&listed); err != nil {
		return nil, fmt.Errorf("decoding the node reports: %w", err)
	}
	reports := make([]observe.NodeReport, 0, len(listed.Reports))
	for _, r := range listed.Reports {
		reports = append(reports, observe.NodeReport{ReceivedAt: r.ReceivedAt, Body: r.Body})
	}
	return reports, nil
}

// reportedNode is one node as the kit's node report names it.
type reportedNode struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
}

// reportProblem names why the cluster has not yet reported every node Ready since the node was created, or "".
func reportProblem(ctx context.Context, d contract.Desired, node hcServer) string {
	since := node.Created.UTC().Format(time.RFC3339)
	reports, err := nodeReports.NodeReports(ctx, d, d.SubscriptionID)
	if err != nil {
		return fmt.Sprintf("the node reports since the node's creation at %s cannot be read: %v", since, err)
	}
	newest, err := newestReportSince(reports, node.Created)
	if err != nil {
		return err.Error()
	}
	if newest == nil {
		return "the cluster has sent no node report since the node's creation at " + since
	}
	if len(newest) == 0 {
		return "the newest node report names no node"
	}
	for _, n := range newest {
		if !n.Ready {
			return fmt.Sprintf("the newest node report reads node %q not Ready", n.Name)
		}
	}
	return ""
}

// newestReportSince returns the nodes of the newest report received after since, or nil when there is none.
func newestReportSince(reports []observe.NodeReport, since time.Time) ([]reportedNode, error) {
	var newest *observe.NodeReport
	for i, r := range reports {
		if r.ReceivedAt.After(since) && (newest == nil || r.ReceivedAt.After(newest.ReceivedAt)) {
			newest = &reports[i]
		}
	}
	if newest == nil {
		return nil, nil
	}
	var body struct {
		Nodes []reportedNode `json:"nodes"`
	}
	if err := json.Unmarshal(newest.Body, &body); err != nil {
		return nil, errors.New("the newest node report is not the node report's JSON")
	}
	if body.Nodes == nil {
		body.Nodes = []reportedNode{}
	}
	return body.Nodes, nil
}
