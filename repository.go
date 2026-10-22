package main

// The kit's ghapi.Client has no topics or contents call, so these calls go to its allowlisted base directly.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mitstack-eu/operator-kit/v3/contract"
	"github.com/mitstack-eu/operator-kit/v3/ghapi"
	"github.com/mitstack-eu/operator-kit/v3/handler"
	"github.com/mitstack-eu/operator-kit/v3/observe"
	"github.com/mitstack-eu/operator-kit/v3/safe"
)

const (
	clusterRepoTopic     = "mitstack-" + blockID
	githubAPIVersion     = "2022-11-28"
	githubTimeout        = 30 * time.Second
	maxGitHubAnswerBytes = 64 << 10
	defaultGitHubAPI     = "https://" + ghapi.APIGitHubHost

	// seedMarkerFile is the file every seed writes, which the seed commit's observer reads.
	seedMarkerFile = "cluster/README.md"
)

// seedFiles open the two directories the cluster's Argo CD Applications sync, so each syncs from its first commit.
var seedFiles = map[string]string{
	seedMarkerFile: "# cluster\n\nArgo CD applies every manifest in this directory to the cluster.\n",
	"repository-credentials/README.md": "# repository-credentials\n\n" +
		"Each app slice commits its repository Secret and Role here, and the cluster's token renewal writes the Secret's password.\n",
}

var repoTopicRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,49}$`)

// clusterRepo is the private repository Argo CD syncs the cluster from.
type clusterRepo struct {
	owner, name string
}

func repoFor(owner, appName string) clusterRepo {
	return clusterRepo{owner: owner, name: appName + "-cluster"}
}

func (c clusterRepo) fullName() string { return c.owner + "/" + c.name }

func (c clusterRepo) url() string { return "https://github.com/" + c.fullName() }

type githubAccess struct {
	client *ghapi.Client
	repo   clusterRepo
	subID  string
}

// newGitHubClient is a variable so a test can point it at a fake whose base the allowlist names.
var newGitHubClient = func(base, token string) *ghapi.Client {
	return ghapi.NewClient(base, token, &http.Client{Timeout: githubTimeout})
}

// dialGitHub refuses, before any request, a request whose GitHub connection cannot be acted on.
func dialGitHub(d contract.Desired, appName string) (*githubAccess, error) {
	token := d.Secrets["github"]["token"]
	if token == "" {
		return nil, handler.Refuse("missing-github-access",
			"the GitHub connection is missing: secrets.github.token is empty, so the cluster repository cannot be read, created or deleted; reconnect GitHub for this app and retry")
	}
	owner := d.Secrets["github"]["owner"]
	if owner == "" {
		return nil, handler.Refuse("missing-github-access",
			"the GitHub connection names no owner: secrets.github.owner is empty, so the cluster repository has no account to live in; reconnect GitHub for this app and retry")
	}
	repo := repoFor(owner, appName)
	if _, err := ghapi.SafeRepo(repo.fullName()); err != nil {
		return nil, handler.Refuse("invalid-param", "the cluster repository name is not usable: %v", err)
	}
	if topic := subscriptionRepoTopic(d.SubscriptionID); !repoTopicRe.MatchString(topic) {
		return nil, handler.Refuse("invalid-subscription",
			"the subscription id cannot form the repository topic %s, which GitHub takes only as at most 50 lowercase letters, digits and hyphens", topic)
	}
	endpoint := d.Instances["github"].Endpoint
	if endpoint == "" {
		endpoint = defaultGitHubAPI
	}
	base, err := ghapi.Base(endpoint, strings.Split(os.Getenv(ghapi.HostsEnv), ",")...)
	if err != nil {
		return nil, handler.Refuse("untrusted-api-base", "github api base rejected: %v", err)
	}
	return &githubAccess{client: newGitHubClient(base.String(), token), repo: repo, subID: d.SubscriptionID}, nil
}

// subscriptionRepoTopic lowercases first, since GitHub takes only lowercase topics and the kit's helper keeps case.
func subscriptionRepoTopic(subID string) string {
	return observe.GitHubSubscriptionTopic(strings.ToLower(subID))
}

func repositoryMark(subID string) []string {
	return []string{clusterRepoTopic, subscriptionRepoTopic(subID)}
}

// carriesMark requires both topics, since Ensure adopts, Observe counts and unsubscribe deletes only a whole mark.
func carriesMark(topics []string, subID string) bool {
	for _, want := range repositoryMark(subID) {
		if !slices.Contains(topics, want) {
			return false
		}
	}
	return true
}

type repoRead struct {
	exists, marked bool
}

// readRepo reads the topics from their own endpoint, since GitHub leaves them out of the repository.
func (g *githubAccess) readRepo(ctx context.Context, subID string) (repoRead, error) {
	_, found, err := g.getIfFound(ctx, "/repos/"+g.repo.fullName(), g.repo.fullName())
	if err != nil || !found {
		return repoRead{}, err
	}
	topics, found, err := g.readTopics(ctx)
	if err != nil || !found {
		return repoRead{}, err
	}
	return repoRead{exists: true, marked: carriesMark(topics, subID)}, nil
}

func (g *githubAccess) readTopics(ctx context.Context) ([]string, bool, error) {
	raw, found, err := g.getIfFound(ctx, "/repos/"+g.repo.fullName()+"/topics", "the topics of "+g.repo.fullName())
	if err != nil || !found {
		return nil, found, err
	}
	var topics struct {
		Names []string `json:"names"`
	}
	if err := json.Unmarshal(raw, &topics); err != nil {
		return nil, false, fmt.Errorf("reading the topics of %s: %w", g.repo.fullName(), err)
	}
	return topics.Names, true, nil
}

func (g *githubAccess) getIfFound(ctx context.Context, path, what string) ([]byte, bool, error) {
	status, raw, err := g.call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, false, err
	}
	switch status {
	case http.StatusOK:
		return raw, true, nil
	case http.StatusNotFound:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("reading %s: github returned %d", what, status)
	}
}

// createRepo skips a read, so GitHub's 422 stops a repository that appeared after readRepo being marked.
func (g *githubAccess) createRepo(ctx context.Context) error {
	status, raw, err := g.call(ctx, http.MethodPost, "/orgs/"+url.PathEscape(g.repo.owner)+"/repos", map[string]any{
		"name": g.repo.name, "private": true, "auto_init": true,
	})
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("creating the cluster repository %s: github returned %d: %s",
			g.repo.fullName(), status, safe.LogSafe(string(raw), g.client.Token))
	}
	return nil
}

// markRepo writes the mark beside the topics already there, since GitHub replaces the whole list.
func (g *githubAccess) markRepo(ctx context.Context, subID string) error {
	topics, found, err := g.readTopics(ctx)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("marking the cluster repository %s: github answered 404 for its topics", g.repo.fullName())
	}
	for _, topic := range repositoryMark(subID) {
		if !slices.Contains(topics, topic) {
			topics = append(topics, topic)
		}
	}
	status, raw, err := g.call(ctx, http.MethodPut, "/repos/"+g.repo.fullName()+"/topics", map[string]any{"names": topics})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("marking the cluster repository %s: github returned %d: %s",
			g.repo.fullName(), status, safe.LogSafe(string(raw), g.client.Token))
	}
	return nil
}

func (g *githubAccess) holdsSeed(ctx context.Context) (bool, error) {
	_, found, err := g.getIfFound(ctx, "/repos/"+g.repo.fullName()+"/contents/"+seedMarkerFile, seedMarkerFile+" in "+g.repo.fullName())
	return found, err
}

// seedRepo writes only the files not there yet, since one there is an earlier seed or the owner's own commit.
func (g *githubAccess) seedRepo(ctx context.Context, files map[string]string) error {
	for _, path := range slices.Sorted(maps.Keys(files)) {
		if err := g.seedFile(ctx, path, files[path]); err != nil {
			return err
		}
	}
	return nil
}

func (g *githubAccess) seedFile(ctx context.Context, path, content string) error {
	contents := "/repos/" + g.repo.fullName() + "/contents/" + path
	_, found, err := g.getIfFound(ctx, contents, path+" in "+g.repo.fullName())
	if err != nil || found {
		return err
	}
	status, raw, err := g.call(ctx, http.MethodPut, contents, map[string]any{
		"message": "Seed " + path + "\n\n" + observe.GitHubSubscriptionTrailer(g.subID),
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	})
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return fmt.Errorf("seeding %s in the cluster repository %s: github returned %d: %s",
			path, g.repo.fullName(), status, safe.LogSafe(string(raw), g.client.Token))
	}
	return nil
}

func (g *githubAccess) deleteRepo(ctx context.Context) error {
	if err := g.client.DeleteRepo(ctx, g.repo.owner, g.repo.name); err != nil {
		return fmt.Errorf("deleting the cluster repository %s: %w", g.repo.fullName(), err)
	}
	return nil
}

// call reads at most maxGitHubAnswerBytes of the body, so an answer of any size is read in bounded memory.
func (g *githubAccess) call(ctx context.Context, method, path string, payload any) (int, []byte, error) {
	req, err := g.newRequest(ctx, method, path, payload)
	if err != nil {
		return 0, nil, err
	}
	httpClient := g.client.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("github %s %s: github unreachable: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxGitHubAnswerBytes))
	if err != nil {
		return 0, nil, fmt.Errorf("github %s %s: reading the answer: %w", method, path, err)
	}
	return resp.StatusCode, raw, nil
}

func (g *githubAccess) newRequest(ctx context.Context, method, path string, payload any) (*http.Request, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encoding github %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.client.Base+path, body)
	if err != nil {
		return nil, fmt.Errorf("building github %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+g.client.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}
