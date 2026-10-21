package main

// The shared fakes: Hetzner in memory behind hetznerAPI, GitHub over HTTP, and the platform's node reports.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mitstack-eu/operator-kit/v2/contract"
	"github.com/mitstack-eu/operator-kit/v2/events"
	"github.com/mitstack-eu/operator-kit/v2/ghapi"
	"github.com/mitstack-eu/operator-kit/v2/observe"
	"github.com/mitstack-eu/talos-cluster-kit/testutil"
	"github.com/stretchr/testify/require"
)

const (
	testSub         = "sub_0123456789ab"
	testApp         = "shop"
	testOwner       = "acme"
	testHetznerKey  = "hetzner-token-for-tests"
	testGitHubToken = "github-token-for-tests"
	testOrigin      = "https://api.mitstack.test"
)

var testStart = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// fakeHetzner holds every resource in memory and records each create, as a project would.
type fakeHetzner struct {
	mu            sync.Mutex
	nextID        int64
	nextAddress   int
	servers       map[int64]*hcServer
	userData      map[int64]string
	networks      map[int64]*hcNetwork
	firewalls     map[int64]*hcFirewall
	loadBalancers map[int64]*hcLoadBalancer
	snapshots     map[int64]*hcSnapshot
	serverSpecs   []serverSpec
	lbSpecs       []loadBalancerSpec
	now           func() time.Time
	// instantBuild powers each builder off as it is created, as if its build had finished at once.
	instantBuild bool
	// failNext, when set, fails the next server list with it.
	failNext error
	// failDelete, when set, fails the next server delete with it.
	failDelete error
	// failPowerOn, when set, fails the next power-on with it.
	failPowerOn error
	// lateLBAddress leaves a new load balancer's private address out of the create answer, as Hetzner may.
	lateLBAddress bool
}

func newFakeHetzner(now func() time.Time) *fakeHetzner {
	return &fakeHetzner{
		nextID: 1000, nextAddress: 5, now: now,
		servers: map[int64]*hcServer{}, userData: map[int64]string{}, networks: map[int64]*hcNetwork{},
		firewalls: map[int64]*hcFirewall{}, loadBalancers: map[int64]*hcLoadBalancer{}, snapshots: map[int64]*hcSnapshot{},
	}
}

func (f *fakeHetzner) id() int64 {
	f.nextID++
	return f.nextID
}

// address hands out the subnet's next free address, as Hetzner does for a server attached at creation.
func (f *fakeHetzner) address() string {
	f.nextAddress++
	return fmt.Sprintf("10.0.0.%d", f.nextAddress)
}

// matches reads a selector of comma-separated key=value terms, the only form the block sends.
func matches(labels map[string]string, selector string) bool {
	for _, term := range strings.Split(selector, ",") {
		key, value, _ := strings.Cut(term, "=")
		if labels[key] != value {
			return false
		}
	}
	return true
}

func selected[T any](items map[int64]*T, labels func(*T) map[string]string, selector string) []T {
	var out []T
	for _, id := range slices.Sorted(maps.Keys(items)) {
		if matches(labels(items[id]), selector) {
			out = append(out, *items[id])
		}
	}
	return out
}

func (f *fakeHetzner) ListServers(_ context.Context, sel string) ([]hcServer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failNext; err != nil {
		f.failNext = nil
		return nil, err
	}
	return selected(f.servers, func(s *hcServer) map[string]string { return s.Labels }, sel), nil
}

func (f *fakeHetzner) CreateServer(_ context.Context, spec serverSpec) (hcServer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &hcServer{
		ID: f.id(), Name: spec.Name, Location: spec.Location, ServerType: spec.ServerType, Status: "running",
		Labels: maps.Clone(spec.Labels), PrivateIPs: map[int64]string{}, FirewallIDs: slices.Clone(spec.FirewallIDs),
		ImageID: spec.ImageID, Created: f.now(),
	}
	if spec.Stopped || (f.instantBuild && spec.Labels[keyRole] == string(roleBuilder)) {
		s.Status = "off"
	}
	if spec.PublicIPv4 {
		s.PublicIPv4 = fmt.Sprintf("203.0.113.%d", s.ID%250)
	}
	for _, n := range spec.NetworkIDs {
		s.PrivateIPs[n] = f.address()
	}
	f.servers[s.ID], f.userData[s.ID] = s, spec.UserData
	f.serverSpecs = append(f.serverSpecs, spec)
	return *s, nil
}

func (f *fakeHetzner) AttachServerToNetwork(_ context.Context, serverID, networkID int64, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.servers[serverID].PrivateIPs[networkID] = ip
	return nil
}

func (f *fakeHetzner) PowerOnServer(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failPowerOn; err != nil {
		f.failPowerOn = nil
		return err
	}
	f.servers[id].Status = "running"
	return nil
}

func (f *fakeHetzner) DeleteServer(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failDelete; err != nil {
		f.failDelete = nil
		return err
	}
	delete(f.servers, id)
	return nil
}

func (f *fakeHetzner) ListNetworks(_ context.Context, sel string) ([]hcNetwork, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return selected(f.networks, func(n *hcNetwork) map[string]string { return n.Labels }, sel), nil
}

func (f *fakeHetzner) CreateNetwork(_ context.Context, spec networkSpec) (hcNetwork, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := &hcNetwork{ID: f.id(), Name: spec.Name, IPRange: spec.IPRange, Labels: maps.Clone(spec.Labels), Routes: spec.Routes}
	f.networks[n.ID] = n
	return *n, nil
}

func (f *fakeHetzner) DeleteNetwork(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.networks, id)
	return nil
}

func (f *fakeHetzner) ListFirewalls(_ context.Context, sel string) ([]hcFirewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return selected(f.firewalls, func(fw *hcFirewall) map[string]string { return fw.Labels }, sel), nil
}

func (f *fakeHetzner) CreateFirewall(_ context.Context, spec firewallSpec) (hcFirewall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fw := &hcFirewall{ID: f.id(), Name: spec.Name, Labels: maps.Clone(spec.Labels)}
	f.firewalls[fw.ID] = fw
	return *fw, nil
}

func (f *fakeHetzner) DeleteFirewall(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.firewalls, id)
	return nil
}

func (f *fakeHetzner) ListLoadBalancers(_ context.Context, sel string) ([]hcLoadBalancer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return selected(f.loadBalancers, func(lb *hcLoadBalancer) map[string]string { return lb.Labels }, sel), nil
}

func (f *fakeHetzner) CreateLoadBalancer(_ context.Context, spec loadBalancerSpec) (hcLoadBalancer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lb := &hcLoadBalancer{
		ID: f.id(), Name: spec.Name, Location: spec.Location, Labels: maps.Clone(spec.Labels), PublicInterface: spec.PublicInterface,
		PrivateIPs: map[int64]string{spec.NetworkID: f.address()}, Services: spec.Services, TargetSelectors: []string{spec.TargetSelector},
	}
	if spec.PublicInterface {
		lb.PublicIPv4 = fmt.Sprintf("198.51.100.%d", lb.ID%250)
	}
	f.loadBalancers[lb.ID] = lb
	f.lbSpecs = append(f.lbSpecs, spec)
	answer := *lb
	if f.lateLBAddress {
		answer.PrivateIPs = map[int64]string{}
	}
	return answer, nil
}

func (f *fakeHetzner) DeleteLoadBalancer(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.loadBalancers, id)
	return nil
}

func (f *fakeHetzner) ListSnapshots(_ context.Context, sel string) ([]hcSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return selected(f.snapshots, func(s *hcSnapshot) map[string]string { return s.Labels }, sel), nil
}

func (f *fakeHetzner) CreateSnapshot(_ context.Context, serverID int64, spec snapshotSpec) (hcSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.servers[serverID] == nil || f.servers[serverID].Status != "off" {
		return hcSnapshot{}, fmt.Errorf("server %d is not powered off", serverID)
	}
	snap := &hcSnapshot{ID: f.id(), Description: spec.Description, Status: "available", Labels: maps.Clone(spec.Labels)}
	f.snapshots[snap.ID] = snap
	return *snap, nil
}

func (f *fakeHetzner) DeleteSnapshot(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.snapshots, id)
	return nil
}

// byRole returns the servers carrying role r, as the subscription's selector finds them.
func (f *fakeHetzner) byRole(t *testing.T, r role) []hcServer {
	t.Helper()
	servers, err := f.ListServers(context.Background(), roleSelector(testSub, r))
	require.NoError(t, err)
	return servers
}

// powerOffBuilder stands in for the builder script's last step.
func (f *fakeHetzner) powerOffBuilder(t *testing.T) {
	t.Helper()
	builders := f.byRole(t, roleBuilder)
	require.Len(t, builders, 1, "want one builder to power off")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.servers[builders[0].ID].Status = "off"
}

func (f *fakeHetzner) specFor(t *testing.T, r role) serverSpec {
	t.Helper()
	for _, spec := range f.serverSpecs {
		if spec.Labels[keyRole] == string(r) {
			return spec
		}
	}
	require.Failf(t, "no server created", "no server with role %s was created", r)
	return serverSpec{}
}

func (f *fakeHetzner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.servers) + len(f.networks) + len(f.firewalls) + len(f.loadBalancers) + len(f.snapshots)
}

// fakeGitHub serves the repository, topics and contents calls the block makes, from memory.
type fakeGitHub struct {
	mu     sync.Mutex
	repos  map[string][]string
	files  map[string]string
	server *httptest.Server
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	gh := &fakeGitHub{repos: map[string][]string{}, files: map[string]string{}}
	gh.server = httptest.NewServer(http.HandlerFunc(gh.serve))
	t.Cleanup(gh.server.Close)
	return gh
}

func (gh *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/repos/")
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/orgs/"):
		var body struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		gh.repos[strings.Split(r.URL.Path, "/")[2]+"/"+body.Name] = []string{}
		w.WriteHeader(http.StatusCreated)
	case strings.HasSuffix(path, "/topics"):
		gh.serveTopics(w, r, strings.TrimSuffix(path, "/topics"))
	case strings.Contains(path, "/contents/"):
		gh.serveContents(w, r, path)
	case r.Method == http.MethodDelete:
		delete(gh.repos, path)
		for file := range gh.files {
			if strings.HasPrefix(file, path+"/") {
				delete(gh.files, file)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		if _, ok := gh.repos[path]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"full_name":"` + path + `"}`))
	}
}

func (gh *fakeGitHub) serveTopics(w http.ResponseWriter, r *http.Request, repo string) {
	if _, ok := gh.repos[repo]; !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method == http.MethodPut {
		var body struct{ Names []string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		gh.repos[repo] = body.Names
	}
	_ = json.NewEncoder(w).Encode(map[string][]string{"names": gh.repos[repo]})
}

func (gh *fakeGitHub) serveContents(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method == http.MethodPut {
		var body struct{ Content string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		decoded, _ := base64.StdEncoding.DecodeString(body.Content)
		gh.files[path] = string(decoded)
		w.WriteHeader(http.StatusCreated)
		return
	}
	content, ok := gh.files[path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(content))})
}

func (gh *fakeGitHub) topics(repo string) []string {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	return gh.repos[repo]
}

// remove deletes the repository with its files, as an owner deleting it on GitHub would.
func (gh *fakeGitHub) remove(repo string) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	delete(gh.repos, repo)
	for path := range gh.files {
		if strings.HasPrefix(path, repo+"/") {
			delete(gh.files, path)
		}
	}
}

func (gh *fakeGitHub) removeFile(path string) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	delete(gh.files, path)
}

func (gh *fakeGitHub) exists(repo string) bool {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	_, ok := gh.repos[repo]
	return ok
}

// fakeReports plays the platform's node-reports read.
type fakeReports struct {
	reports []observe.NodeReport
}

func (f *fakeReports) NodeReports(context.Context, contract.Desired, string) ([]observe.NodeReport, error) {
	return f.reports, nil
}

// report adds a node report received at at, naming each node with its readiness.
func (f *fakeReports) report(at time.Time, ready ...bool) {
	nodes := make([]map[string]any, len(ready))
	for i, r := range ready {
		nodes[i] = map[string]any{"name": fmt.Sprintf("node-%d", i), "ready": r}
	}
	body, _ := json.Marshal(map[string]any{"subscriptionId": testSub, "nodes": nodes})
	f.reports = append(f.reports, observe.NodeReport{ReceivedAt: at, Body: body})
}

// env is one block under test with every fake it calls, and the clock its verbs read.
type env struct {
	t       *testing.T
	block   *blockResource
	hetzner *fakeHetzner
	github  *fakeGitHub
	reports *fakeReports
	now     time.Time
	params  map[string]string
}

type envOption func(*env)

func withLocation(location string) envOption {
	return func(e *env) { e.params["location"] = location }
}

func withParam(key, value string) envOption {
	return func(e *env) { e.params[key] = value }
}

func newEnv(t *testing.T, opts ...envOption) *env {
	t.Helper()
	e := &env{t: t, now: testStart, reports: &fakeReports{}, params: map[string]string{"appName": testApp, "location": "nbg1"}}
	e.block = &blockResource{now: func() time.Time { return e.now }}
	e.hetzner = newFakeHetzner(func() time.Time { return e.now })
	e.github = newFakeGitHub(t)
	for _, opt := range opts {
		opt(e)
	}
	swap(t, &newHetznerAPI, func(string) hetznerAPI { return e.hetzner })
	swap(t, &newGitHubClient, func(_, token string) *ghapi.Client {
		return ghapi.NewClient(e.github.server.URL, token, e.github.server.Client())
	})
	swap(t, &toolsImage, func() string { return testutil.ToolsImage })
	var reports observe.NodeReportsAPI = e.reports
	swap(t, &nodeReports, reports)
	t.Setenv(envLogExportAddress, testutil.LogExport(t).Endpoint)
	t.Setenv(envLogExportCA, string(testutil.LogExport(t).CA))
	return e
}

func swap[T any](t *testing.T, target *T, value T) {
	t.Helper()
	old := *target
	*target = value
	t.Cleanup(func() { *target = old })
}

func (e *env) desired() contract.Desired {
	return contract.Desired{
		Identity: contract.Identity{SubscriptionID: testSub},
		Params:   maps.Clone(e.params),
		Secrets: map[string]map[string]string{
			"hetzner": {"api_token": testHetznerKey},
			"github":  {"token": testGitHubToken, "owner": testOwner},
		},
	}
}

func (e *env) ctx() context.Context {
	return events.WithEnvelope(context.Background(), events.Envelope{
		SubscriptionID: testSub,
		Callback:       &events.Callback{URL: testOrigin + operatorEventsPath, Token: "callback-token-for-tests"},
	})
}

func (e *env) ensure() error {
	return e.block.Ensure(e.ctx(), e.desired())
}

func (e *env) observe() contract.Observation {
	e.t.Helper()
	obs, err := e.block.Observe(e.ctx(), e.desired())
	require.NoError(e.t, err)
	return obs
}

// subscribe runs every step a subscribe and its redrives take, the builder's power-off and the node's first report included.
func (e *env) subscribe() contract.Observation {
	e.t.Helper()
	require.NoError(e.t, e.ensure(), "the first ensure")
	e.now = e.now.Add(10 * time.Minute)
	e.hetzner.powerOffBuilder(e.t)
	require.NoError(e.t, e.ensure(), "the ensure after the build")
	e.now = e.now.Add(5 * time.Minute)
	e.reports.report(e.now, true)
	return e.observe()
}

func (e *env) repo() string {
	return testOwner + "/" + testApp + "-cluster"
}
