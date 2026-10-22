package main

// The shared fakes: Hetzner in memory behind hetznerAPI, GitHub over HTTP, the platform's node reports, and the
// platform's own settle check sending signed requests to the block's handler.

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

	"github.com/mitstack-eu/operator-kit/v3/contract"
	"github.com/mitstack-eu/operator-kit/v3/events"
	"github.com/mitstack-eu/operator-kit/v3/ghapi"
	"github.com/mitstack-eu/operator-kit/v3/handler"
	"github.com/mitstack-eu/operator-kit/v3/kittest"
	"github.com/mitstack-eu/operator-kit/v3/observe"
	"github.com/mitstack-eu/operator-kit/v3/webhook"
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
	servers       map[int64]*hcServer
	userData      map[int64]string
	networks      map[int64]*hcNetwork
	firewalls     map[int64]*hcFirewall
	loadBalancers map[int64]*hcLoadBalancer
	snapshots     map[int64]*hcSnapshot
	serverSpecs   []serverSpec
	lbSpecs       []loadBalancerSpec
	networkSpecs  []networkSpec
	// nodesPerCreate holds how many nodes each server create made.
	nodesPerCreate []int
	now            func() time.Time
	// started and imaging hold when each server was powered on and each snapshot was started.
	started, imaging map[int64]time.Time
	// waits counts the calls the adapter waits on a Hetzner action for, and writes every call that changes something.
	waits, writes int
	// instantBuild powers each builder off as it is powered on, and images each snapshot as it starts.
	instantBuild bool
	// failNext, when set, fails the next server list with it.
	failNext error
	// failDelete, when set, fails the next server delete with it.
	failDelete error
	// failCreate, when set, fails the next server create of that role with it.
	failCreate map[role]error
	// failLBStep, when set, fails the next load balancer call of that name with it.
	failLBStep map[string]error
	// failAttach, when set, fails the next attach of a server of that role with it once the attach has landed.
	failAttach map[role]error
	// soldOut holds the server types no location can order.
	soldOut []string
	// metrics answers each server's metrics read; failMetrics, when set, fails every read, and metricsReads records each one.
	metrics      map[int64]hcMetrics
	failMetrics  error
	metricsReads []metricsRead
}

type metricsRead struct {
	id         int64
	start, end time.Time
}

func newFakeHetzner(now func() time.Time) *fakeHetzner {
	return &fakeHetzner{
		nextID: 1000, now: now,
		servers: map[int64]*hcServer{}, userData: map[int64]string{}, networks: map[int64]*hcNetwork{},
		firewalls: map[int64]*hcFirewall{}, loadBalancers: map[int64]*hcLoadBalancer{}, snapshots: map[int64]*hcSnapshot{},
		started: map[int64]time.Time{}, imaging: map[int64]time.Time{}, failCreate: map[role]error{}, failLBStep: map[string]error{},
		failAttach: map[role]error{}, metrics: map[int64]hcMetrics{},
	}
}

func (f *fakeHetzner) id() int64 {
	f.nextID++
	return f.nextID
}

// write counts a call that changes the project, and one the adapter waits on.
func (f *fakeHetzner) write(waited bool) {
	f.writes++
	if waited {
		f.waits++
	}
}

// held reports whether a server or load balancer holds ip on the network.
func (f *fakeHetzner) held(networkID int64, ip string) bool {
	for _, s := range f.servers {
		if s.PrivateIPs[networkID] == ip {
			return true
		}
	}
	for _, lb := range f.loadBalancers {
		if lb.PrivateIPs[networkID] == ip {
			return true
		}
	}
	return false
}

// firstFree is the lowest address no one holds above the router's, which Hetzner gives a server joining with no address.
func (f *fakeHetzner) firstFree(networkID int64) string {
	for host := 2; ; host++ {
		if ip := fmt.Sprintf("10.0.0.%d", host); !f.held(networkID, ip) {
			return ip
		}
	}
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

// hetznerUserDataLimit is the most user data Hetzner's create-server reference accepts: "limited to 32KiB".
const hetznerUserDataLimit = 32 * 1024

func (f *fakeHetzner) CreateServers(_ context.Context, specs ...serverSpec) ([]hcServer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.write(true)
	nodes := 0
	for _, spec := range specs {
		if spec.Labels[keyRole] == string(roleNode) {
			nodes++
		}
	}
	f.nodesPerCreate = append(f.nodesPerCreate, nodes)
	var created []hcServer
	for _, spec := range specs {
		if err := f.failCreate[role(spec.Labels[keyRole])]; err != nil {
			delete(f.failCreate, role(spec.Labels[keyRole]))
			return created, err
		}
		if len(spec.UserData) > hetznerUserDataLimit {
			return created, fmt.Errorf("creating server %s: invalid input in field 'user_data' (invalid_input)", spec.Name)
		}
		s := &hcServer{
			ID: f.id(), Name: spec.Name, Location: spec.Location, ServerType: spec.ServerType, Status: serverRunning,
			Labels: maps.Clone(spec.Labels), PrivateIPs: map[int64]string{}, FirewallIDs: slices.Clone(spec.FirewallIDs),
			ImageID: spec.ImageID, Created: f.now(),
		}
		if spec.Stopped {
			s.Status = serverOff
		} else {
			f.started[s.ID] = f.now()
		}
		if spec.PublicIPv4 {
			s.PublicIPv4 = fmt.Sprintf("203.0.113.%d", s.ID%250)
		}
		for _, n := range spec.NetworkIDs {
			s.PrivateIPs[n] = f.firstFree(n)
		}
		f.servers[s.ID], f.userData[s.ID] = s, spec.UserData
		f.serverSpecs = append(f.serverSpecs, spec)
		created = append(created, *s)
	}
	return created, nil
}

func (f *fakeHetzner) AvailableServerTypes(_ context.Context, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.DeleteFunc([]string{"cx23", "cx33", "cx43", "cx53", "cpx22", "cpx52", "cpx62"},
		func(name string) bool { return slices.Contains(f.soldOut, name) }), nil
}

func (f *fakeHetzner) ServerStarted(_ context.Context, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.started[id]
	return ok, nil
}

// AttachServerToNetwork answers as Hetzner did live, refusing a server already on the network and an address held.
func (f *fakeHetzner) AttachServerToNetwork(_ context.Context, serverID, networkID int64, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.servers[serverID]
	switch {
	case s.PrivateIPs[networkID] != "":
		return fmt.Errorf("attaching server %d to network %d: server already attached (server_already_attached)", serverID, networkID)
	case ip == "":
		ip = f.firstFree(networkID)
	case f.held(networkID, ip):
		return fmt.Errorf("attaching server %d to network %d at %s: IP not available (ip_not_available)", serverID, networkID, ip)
	}
	f.write(true)
	s.PrivateIPs[networkID] = ip
	if err := f.failAttach[role(s.Labels[keyRole])]; err != nil {
		delete(f.failAttach, role(s.Labels[keyRole]))
		return err
	}
	return nil
}

func (f *fakeHetzner) PowerOnServer(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.write(true)
	s := f.servers[id]
	s.Status, f.started[id] = serverRunning, f.now()
	if f.instantBuild && s.Labels[keyRole] == string(roleBuilder) {
		s.Status = serverOff
	}
	return nil
}

func (f *fakeHetzner) ServerMetrics(_ context.Context, id int64, start, end time.Time) (hcMetrics, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metricsReads = append(f.metricsReads, metricsRead{id: id, start: start, end: end})
	if f.failMetrics != nil {
		return hcMetrics{}, f.failMetrics
	}
	return f.metrics[id], nil
}

// builderMetrics gives the builder the series Hetzner would answer, a point a minute from a minute after its creation.
func (f *fakeHetzner) builderMetrics(t *testing.T, series map[string][]float64) hcServer {
	t.Helper()
	builders := f.byRole(t, roleBuilder)
	require.Len(t, builders, 1, "want one builder to give metrics")
	m := hcMetrics{Step: time.Minute, Series: map[string][]metricPoint{}}
	for name, values := range series {
		for i, v := range values {
			m.Series[name] = append(m.Series[name], metricPoint{At: builders[0].Created.Add(time.Duration(i+1) * time.Minute), Value: v})
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metrics[builders[0].ID] = m
	return builders[0]
}

func (f *fakeHetzner) DeleteServer(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failDelete; err != nil {
		f.failDelete = nil
		return err
	}
	f.write(true)
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
	f.write(false)
	n := &hcNetwork{ID: f.id(), Name: spec.Name, IPRange: spec.IPRange, Labels: maps.Clone(spec.Labels), Routes: spec.Routes}
	f.networks[n.ID] = n
	f.networkSpecs = append(f.networkSpecs, spec)
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
	f.write(true)
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
	f.write(true)
	lb := &hcLoadBalancer{
		ID: f.id(), Name: spec.Name, Location: spec.Location, Labels: maps.Clone(spec.Labels), PublicInterface: spec.PublicInterface,
		PrivateIPs: map[int64]string{}, Services: slices.Clone(spec.Services),
	}
	if spec.NetworkID != 0 {
		lb.PrivateIPs[spec.NetworkID] = f.firstFree(spec.NetworkID)
	}
	if spec.TargetSelector != "" {
		lb.TargetSelectors = []string{spec.TargetSelector}
	}
	if spec.PublicInterface {
		lb.PublicIPv4 = fmt.Sprintf("198.51.100.%d", lb.ID%250)
	}
	f.loadBalancers[lb.ID] = lb
	f.lbSpecs = append(f.lbSpecs, spec)
	return *lb, nil
}

// lbStep fails a load balancer call failLBStep names, once, and otherwise returns the load balancer it acts on.
func (f *fakeHetzner) lbStep(step string, id int64) (*hcLoadBalancer, error) {
	if err := f.failLBStep[step]; err != nil {
		delete(f.failLBStep, step)
		return nil, err
	}
	lb, ok := f.loadBalancers[id]
	if !ok {
		return nil, fmt.Errorf("not_found: load balancer %d", id)
	}
	f.write(true)
	return lb, nil
}

// AttachLoadBalancerToNetwork assigns the requested address once the attach action finishes, and refuses a second attach as Hetzner does.
func (f *fakeHetzner) AttachLoadBalancerToNetwork(_ context.Context, id, networkID int64, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	lb, err := f.lbStep("attach", id)
	if err != nil {
		return err
	}
	if len(lb.PrivateIPs) > 0 {
		return fmt.Errorf("load_balancer_already_attached: load balancer %d", id)
	}
	if f.held(networkID, ip) {
		return fmt.Errorf("attaching load balancer %d to network %d at %s: IP not available (ip_not_available)", id, networkID, ip)
	}
	lb.PrivateIPs[networkID] = ip
	return nil
}

func (f *fakeHetzner) DisableLoadBalancerPublicInterface(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	lb, err := f.lbStep("disable-public", id)
	if err != nil {
		return err
	}
	if len(lb.PrivateIPs) == 0 {
		return fmt.Errorf("load_balancer_not_attached_to_network: load balancer %d", id)
	}
	lb.PublicInterface = false
	return nil
}

func (f *fakeHetzner) AddLoadBalancerService(_ context.Context, id int64, service lbService) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	lb, err := f.lbStep("add-service", id)
	if err != nil {
		return err
	}
	for _, s := range lb.Services {
		if s.ListenPort == service.ListenPort {
			return fmt.Errorf("source_port_already_used: load balancer %d listens on %d", id, s.ListenPort)
		}
	}
	lb.Services = append(lb.Services, service)
	return nil
}

func (f *fakeHetzner) AddLoadBalancerTarget(_ context.Context, id int64, labelSelector string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	lb, err := f.lbStep("add-target", id)
	if err != nil {
		return err
	}
	if len(lb.PrivateIPs) == 0 {
		return fmt.Errorf("load_balancer_not_attached_to_network: load balancer %d", id)
	}
	if slices.Contains(lb.TargetSelectors, labelSelector) {
		return fmt.Errorf("target_already_defined: load balancer %d", id)
	}
	lb.TargetSelectors = append(lb.TargetSelectors, labelSelector)
	return nil
}

// clusterLocationNames are Hetzner Cloud's three European locations, each of which holds one node of every cluster.
var clusterLocationNames = []string{"fsn1", "nbg1", "hel1"}

// lbsByLocation returns the load balancers carrying role r by location, failing unless each location holds exactly one.
func (f *fakeHetzner) lbsByLocation(t *testing.T, r role) map[string]hcLoadBalancer {
	t.Helper()
	lbs, err := f.ListLoadBalancers(context.Background(), roleSelector(testSub, r))
	require.NoError(t, err)
	byLocation := map[string]hcLoadBalancer{}
	for _, lb := range lbs {
		byLocation[lb.Location] = lb
	}
	require.Len(t, lbs, len(clusterLocationNames), "%s load balancers", r)
	require.ElementsMatch(t, clusterLocationNames, slices.Collect(maps.Keys(byLocation)), "the %s load balancers' locations", r)
	return byLocation
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

func (f *fakeHetzner) StartSnapshot(_ context.Context, serverID int64, spec snapshotSpec) (hcSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.servers[serverID] == nil || f.servers[serverID].Status != serverOff {
		return hcSnapshot{}, fmt.Errorf("server %d is not powered off", serverID)
	}
	f.write(false)
	snap := &hcSnapshot{ID: f.id(), Description: spec.Description, Status: "creating", Labels: maps.Clone(spec.Labels)}
	if f.instantBuild {
		snap.Status = snapshotAvailable
	}
	f.snapshots[snap.ID], f.imaging[snap.ID] = snap, f.now()
	return *snap, nil
}

func (f *fakeHetzner) DeleteSnapshot(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.write(false)
	delete(f.snapshots, id)
	return nil
}

// advance plays what Hetzner and the builder finish on their own: a builder powers itself off build after its
// power-on, unless build is zero, and a snapshot is available image after it started.
func (f *fakeHetzner) advance(build, image time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, s := range f.servers {
		if s.Labels[keyRole] == string(roleBuilder) && s.Status == serverRunning && build > 0 && !f.now().Before(f.started[id].Add(build)) {
			s.Status = serverOff
		}
	}
	for id, snap := range f.snapshots {
		if !f.now().Before(f.imaging[id].Add(image)) {
			snap.Status = snapshotAvailable
		}
	}
}

// resources is every resource but the builder, as text, so a later read shows any change to one made before.
func (f *fakeHetzner) resources() map[int64]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int64]string{}
	for id, s := range f.servers {
		if s.Labels[keyRole] != string(roleBuilder) {
			out[id] = fmt.Sprintf("%+v", *s)
		}
	}
	for id, n := range f.networks {
		out[id] = fmt.Sprintf("%+v", *n)
	}
	for id, fw := range f.firewalls {
		out[id] = fmt.Sprintf("%+v", *fw)
	}
	for id, lb := range f.loadBalancers {
		out[id] = fmt.Sprintf("%+v", *lb)
	}
	return out
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
	f.servers[builders[0].ID].Status = serverOff
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
	writes int
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
	if r.Method != http.MethodGet {
		gh.writes++
	}
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
	// buildTakes and imageTakes are how long the builder and the snapshot run once started; a zero build never ends.
	buildTakes, imageTakes time.Duration
	// quietNode holds back the nodes' report, so the cluster waits on it.
	quietNode bool
	eng       *engine
}

type envOption func(*env)

func withLocation(location string) envOption {
	return withParam("location", location)
}

func withParam(key, value string) envOption {
	return func(e *env) { e.params[key] = value }
}

func newEnv(t *testing.T, opts ...envOption) *env {
	t.Helper()
	e := &env{
		t: t, now: testStart, reports: &fakeReports{}, params: map[string]string{"appName": testApp},
		block: &blockResource{}, buildTakes: 10 * time.Minute, imageTakes: 3 * time.Minute,
	}
	e.hetzner = newFakeHetzner(func() time.Time { return e.now })
	e.github = newFakeGitHub(t)
	for _, opt := range opts {
		opt(e)
	}
	swap(t, &clock, func() time.Time { return e.now })
	swap(t, &newHetznerAPI, func(string) hetznerAPI { return e.hetzner })
	swap(t, &newGitHubClient, func(_, token string) *ghapi.Client {
		return ghapi.NewClient(e.github.server.URL, token, e.github.server.Client())
	})
	var reports observe.NodeReportsAPI = e.reports
	swap(t, &nodeReports, reports)
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

// changes counts every call that changed Hetzner or GitHub.
func (e *env) changes() int {
	e.hetzner.mu.Lock()
	defer e.hetzner.mu.Unlock()
	e.github.mu.Lock()
	defer e.github.mu.Unlock()
	return e.hetzner.writes + e.github.writes
}

// converge sends Ensure until one changes nothing, as the engine's re-drives would, and returns the first error.
func (e *env) converge() error {
	e.t.Helper()
	for range 50 {
		before := e.changes()
		if err := e.ensure(); err != nil || e.changes() == before {
			return err
		}
	}
	e.t.Fatal("Ensure still changed the cluster after 50 calls")
	return nil
}

func (e *env) observe() contract.Observation {
	e.t.Helper()
	obs, err := e.block.Observe(e.ctx(), e.desired())
	require.NoError(e.t, err)
	return obs
}

// progress plays what Hetzner, the builder and the nodes do on their own while the platform waits: the cluster
// reports every node Ready once all three exist.
func (e *env) progress() {
	e.hetzner.advance(e.buildTakes, e.imageTakes)
	if !e.quietNode && len(e.hetzner.byRole(e.t, roleNode)) == len(clusterLocationNames) && len(e.reports.reports) == 0 {
		e.reports.report(e.now, true, true, true)
	}
}

// building reports whether the builder runs its build, the long step the engine's re-drive resumes after.
func (e *env) building() bool {
	builders := e.hetzner.byRole(e.t, roleBuilder)
	return len(builders) == 1 && builders[0].Status == serverRunning
}

// buildingBuilder settles a subscription until its builder runs a build that never ends.
func (e *env) buildingBuilder() *engine {
	e.t.Helper()
	e.buildTakes = 0
	p := e.engine()
	p.settle(func() bool { return e.building() })
	require.True(e.t, e.building(), "the builder never started its build")
	return p
}

const mib = 1 << 20

// subscribe settles the subscription as the platform does and returns what Observe then reads.
func (e *env) subscribe() contract.Observation {
	e.t.Helper()
	final := e.engine().settle(nil)
	require.Equal(e.t, http.StatusOK, final.status, "the settle's last answer: %s", final.detail)
	return e.observe()
}

func (e *env) repo() string {
	return testOwner + "/" + testApp + "-cluster"
}

// settleBudget is the manifest's spec.lifecycle.settleBudget.
const settleBudget = 60 * time.Minute

// engine plays the platform's lifecycle engine against the block's own handler, as signed requests.
type engine struct {
	e      *env
	h      http.Handler
	signer *kittest.Signer
	seq    int
	sleeps int
	posted []answer
}

// answer is what one request to the handler answered, with how many Hetzner actions it waited on.
type answer struct {
	op               string
	status           int
	pending, redrive bool
	detail           string
	waits            int
	outputs          map[string]string
	resources        map[string]contract.ResourceOutcome
}

// observed is what the answer's observe of the named resource returned, or nil when the call never reached it.
func (a answer) observed(name string) *contract.CallOutcome {
	return a.resources[name].Observe
}

func (a answer) err() error {
	if a.status >= http.StatusBadRequest {
		return fmt.Errorf("%s answered %d: %s", a.op, a.status, a.detail)
	}
	return nil
}

func (e *env) engine() *engine {
	if e.eng != nil {
		return e.eng
	}
	p := &engine{e: e, signer: kittest.NewSigner(e.t, "kid-hcloud-talos-cluster", 41)}
	cfg := handlerConfig(p.signer.JWKSServer(e.t), nil)
	cfg.Resource = e.block
	cfg.Sleep = func(context.Context, time.Duration) { p.sleeps++ }
	h, err := handler.New(cfg)
	require.NoError(e.t, err)
	p.h, e.eng = h, p
	return p
}

func (p *engine) post(operation string) answer {
	p.e.t.Helper()
	p.seq++
	body := hookBody(operation, p.e)
	obo, err := webhook.MintOBO(p.signer.Private, p.signer.Kid,
		webhook.OBOClaims{OrgID: body["orgId"].(string), SubscriptionID: testSub}, time.Hour, time.Now())
	require.NoError(p.e.t, err)
	body["obo"], body["idempotencyKey"] = obo, fmt.Sprintf("%s-%d", operation, p.seq)
	waits := p.e.hetzner.waits
	w := httptest.NewRecorder()
	p.h.ServeHTTP(w, p.signer.HookRequest(p.e.t, "/hook", kittest.Envelope(fmt.Sprintf("nonce-%d", p.seq), time.Now(), body)))
	var doc struct {
		Pending, Redrive    bool
		StatusDetail, Error string
		Outputs             map[string]string
		Resources           map[string]contract.ResourceOutcome
	}
	require.NoError(p.e.t, json.Unmarshal(w.Body.Bytes(), &doc), "the %s answer", operation)
	a := answer{op: operation, status: w.Code, pending: doc.Pending, redrive: doc.Redrive, detail: doc.StatusDetail + doc.Error, waits: p.e.hetzner.waits - waits,
		outputs: doc.Outputs, resources: doc.Resources}
	p.posted = append(p.posted, a)
	return a
}

// settle sends one subscribe, then the settle check's validates, parked 15 seconds doubling to two minutes, and a
// re-driven subscribe whenever validate answers redrive. It returns the answer that settled the subscription, or the
// last subscribe's once stop reads true.
func (p *engine) settle(stop func() bool) answer {
	last := p.post("subscribe")
	for park, began := 15*time.Second, p.e.now; last.pending && last.err() == nil && p.e.now.Sub(began) < settleBudget; {
		if stop != nil && stop() {
			return last
		}
		p.e.now = p.e.now.Add(park)
		park = min(2*park, 2*time.Minute)
		p.e.progress()
		v := p.post("validate")
		if !v.pending || v.err() != nil {
			return v
		}
		if v.redrive {
			last, park = p.post("subscribe"), 15*time.Second
		}
	}
	return last
}
