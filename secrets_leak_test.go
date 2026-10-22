package main

// No token a request carries reaches an answer or the /logs buffer, on the success paths or an error that quotes it.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/mitstack-eu/operator-kit/v3/handler"
	"github.com/mitstack-eu/operator-kit/v3/kittest"
	"github.com/mitstack-eu/operator-kit/v3/webhook"
	"github.com/stretchr/testify/require"
)

func hookBody(operation string, e *env) map[string]any {
	return map[string]any{
		"operation":      operation,
		"subscriptionId": testSub,
		"blockId":        blockID,
		"blockVersion":   "0.1.0",
		"orgId":          "org_0123456789ab",
		"appId":          "app_0123456789ab",
		"params":         e.params,
		"secrets": map[string]any{
			"hetzner": map[string]any{"api_token": testHetznerKey},
			"github":  map[string]any{"token": testGitHubToken, "owner": testOwner},
		},
	}
}

// leakingEnv is a block whose first Hetzner call fails with an error quoting both tokens.
func leakingEnv(t *testing.T) (*env, func(*webhook.LogBuffer) (http.Handler, *kittest.Signer)) {
	t.Helper()
	e := newEnv(t)
	e.hetzner.instantBuild = true
	e.reports.report(testStart.Add(time.Hour), true)
	e.hetzner.failNext = errors.New("hetzner rejected the token " + testHetznerKey + " used with " + testGitHubToken)
	return e, func(logs *webhook.LogBuffer) (http.Handler, *kittest.Signer) {
		signer := kittest.NewSigner(t, "kid-hcloud-talos-cluster", 41)
		cfg := handlerConfig(signer.JWKSServer(t), logs)
		cfg.Resource = e.block
		cfg.VerbBudget = 3
		cfg.Sleep = func(context.Context, time.Duration) {}
		h, err := handler.New(cfg)
		require.NoError(t, err)
		return h, signer
	}
}

func TestNoSecretLeak_TheHetznerAndGitHubTokensThroughTheBlock(t *testing.T) {
	e, serve := leakingEnv(t)

	kittest.AssertNoSecretLeak(t, kittest.SecretLeak{
		Secrets: []string{testHetznerKey, testGitHubToken},
		Serve:   serve,
		Requests: []kittest.LeakRequest{
			{Name: "a subscribe whose Hetzner call fails quoting both tokens", Body: hookBody("subscribe", e), Fails: true},
			{Name: "a subscribe that builds the cluster", Body: hookBody("subscribe", e)},
			{Name: "a validate of the built cluster", Body: hookBody("validate", e)},
			{Name: "an unsubscribe", Body: hookBody("unsubscribe", e)},
		},
	})
}

func TestNoSecretLeak_LogsEndpoint(t *testing.T) {
	e, serve := leakingEnv(t)

	kittest.AssertNoSecretLeak(t, kittest.SecretLeak{
		Secrets:  []string{testHetznerKey, testGitHubToken},
		Serve:    serve,
		Requests: []kittest.LeakRequest{{Name: "a subscribe whose Hetzner call fails quoting both tokens", Body: hookBody("subscribe", e), Fails: true}},
	})
}
