// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package impl

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"syscall"

	"golang.org/x/exp/maps"
	"greatestworks/aop/files"
	"greatestworks/aop/metrics"
	imetrics "greatestworks/aop/metrics"
	"greatestworks/aop/perfetto"
	"greatestworks/aop/protos"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/sdk/trace"
	gproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"greatestworks/aop/logging"
	"greatestworks/aop/logtype"
	"greatestworks/aop/proto"
	"greatestworks/aop/protomsg"
	"greatestworks/aop/proxy"
	"greatestworks/aop/retry"
	"greatestworks/aop/status"
	"greatestworks/aop/traceio"
	"greatestworks/aop/versioned_map"
)

const (
	// URL suffixes for various SSH manager handlers.
	getComponentsToStartURL = "/manager/get_components_to_start"
	registerReplicaURL      = "/manager/register_replica"
	exportListenerURL       = "/manager/export_listener"
	startComponentURL       = "/manager/start_component"
	getRoutingInfoURL       = "/manager/get_routing_info"
	recvLogEntryURL         = "/manager/recv_log_entry"
	recvTraceSpansURL       = "/manager/recv_trace_spans"
	recvMetricsURL          = "/manager/recv_metrics"

	// babysitterInfoKey is the name of the env variable that contains deployment
	// information for a babysitter deployed using SSH.
	babysitterInfoKey = "SERVICEWEAVER_BABYSITTER_INFO"

	// routingInfoKey is the key where we track routing information for a given process.
	routingInfoKey = "routing_entries"

	// appVersionStateKey is the key where we track the state for a given application version.
	appVersionStateKey = "app_version_state"
)

// manager manages an application version deployment across a set of locations,
// where a location can be a physical or a virtual machine.
//
// TODO(rgrandl): Right now there is a lot of duplicate code between the
// internal/babysitter and the internal/tool/ssh/impl/manager. See if we can reduce the
// duplicated code.
type manager struct {
	ctx        context.Context
	dep        *protos.Deployment
	logger     logtype.Logger
	logDir     string
	locations  []string // addresses of the locations
	mgrAddress string   // manager address
	registry   *status.Registry

	// logSaver processes log entries generated by the weavelets and babysitters.
	// The entries either have the timestamp produced by the weavelet/babysitter,
	// or have a nil Time field. Defaults to a log saver that pretty prints log
	// entries to stderr.
	//
	// logSaver is called concurrently from multiple goroutines, so it should
	// be thread safe.
	logSaver func(*protos.LogEntry)

	// traceSaver processes trace spans generated by the weavelet. If nil,
	// weavelet traces are dropped.
	//
	// traceSaver is called concurrently from multiple goroutines, so it should
	// be thread safe.
	traceSaver func(spans *protos.Spans) error

	// statsProcessor tracks and computes stats to be rendered on the /statusz page.
	statsProcessor *imetrics.StatsProcessor

	mu           sync.Mutex
	started      map[string]bool //  colocation groups started, by group name
	appState     *versioned_map.Map[*AppVersionState]
	routingState *versioned_map.Map[*protos.RoutingInfo]
	proxies      map[string]*proxyInfo                         // proxies, by listener name
	metrics      map[groupReplicaInfo][]*protos.MetricSnapshot // latest metrics, by group name and replica id
}

type proxyInfo struct {
	proxy *proxy.Proxy
	addr  string // dialable address of the proxy
}

type groupReplicaInfo struct {
	name string
	id   int32
}

var _ status.Server = &manager{}

// RunManager creates and runs a new manager.
func RunManager(ctx context.Context, dep *protos.Deployment, locations []string,
	logDir string) (func() error, error) {
	fs, err := logging.NewFileStore(logDir)
	if err != nil {
		return nil, fmt.Errorf("cannot create log storage: %w", err)
	}
	logSaver := fs.Add

	logger := logging.FuncLogger{
		Opts: logging.Options{
			App:       dep.App.Name,
			Component: "manager",
			Weavelet:  uuid.NewString(),
			Attrs:     []string{"serviceweaver/system", ""},
		},
		Write: logSaver,
	}

	// Create the trace saver.
	traceDB, err := perfetto.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot open Perfetto database: %w", err)
	}
	traceSaver := func(spans *protos.Spans) error {
		var traces []trace.ReadOnlySpan
		for _, span := range spans.Span {
			traces = append(traces, &traceio.ReadSpan{Span: span})
		}
		return traceDB.Store(ctx, dep.App.Name, dep.Id, traces)
	}
	m := &manager{
		ctx:            ctx,
		dep:            dep,
		locations:      locations,
		logger:         logger,
		logDir:         logDir,
		logSaver:       logSaver,
		traceSaver:     traceSaver,
		statsProcessor: imetrics.NewStatsProcessor(),
		started:        map[string]bool{},
		appState:       versioned_map.NewMap[*AppVersionState](),
		routingState:   versioned_map.NewMap[*protos.RoutingInfo](),
		proxies:        map[string]*proxyInfo{},
		metrics:        map[groupReplicaInfo][]*protos.MetricSnapshot{},
	}

	go func() {
		if err := m.run(); err != nil {
			m.logger.Error("Unable to run the manager", err)
		}
	}()
	go m.statsProcessor.CollectMetrics(m.ctx, func() []*metrics.MetricSnapshot {
		m.mu.Lock()
		defer m.mu.Unlock()
		var result []*metrics.MetricSnapshot
		for _, ms := range m.metrics {
			for _, m := range ms {
				result = append(result, metrics.UnProto(m))
			}
		}
		return result
	})
	return func() error {
		return m.registry.Unregister(m.ctx, m.dep.Id)
	}, nil
}

func (m *manager) run() error {
	host, _ := os.Hostname()
	lis, err := net.Listen("tcp", fmt.Sprintf("%s:0", host))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	m.mgrAddress = fmt.Sprintf("http://%s", lis.Addr())

	m.logger.Info("Manager listening", "address", m.mgrAddress)

	mux := http.NewServeMux()
	m.addHTTPHandlers(mux)
	m.registerStatusPages(mux)

	go func() {
		if err := serveHTTP(m.ctx, lis, mux); err != nil {
			m.logger.Error("Unable to start HTTP server", err)
		}
	}()

	// Start the main process.
	group := &protos.ColocationGroup{Name: "main"}
	if err := m.startComponent(m.ctx, &protos.ComponentToStart{
		ColocationGroup: group.Name,
		Component:       "main",
	}); err != nil {
		return err
	}
	if err := m.startColocationGroup(m.ctx, group); err != nil {
		return err
	}

	// Wait for the status server to become active.
	client := status.NewClient(lis.Addr().String())
	for r := retry.Begin(); r.Continue(m.ctx); {
		_, err := client.Status(m.ctx)
		if err == nil {
			break
		}
		m.logger.Error("Error starting status server", err, "address", lis.Addr())
	}

	// AddHandler the deployment.
	registry, err := DefaultRegistry(m.ctx)
	if err != nil {
		return fmt.Errorf("create registry: %w", err)
	}
	m.registry = registry
	reg := status.Registration{
		DeploymentId: m.dep.Id,
		App:          m.dep.App.Name,
		Addr:         lis.Addr().String(),
	}
	fmt.Fprint(os.Stderr, reg.Rolodex())
	return registry.Register(m.ctx, reg)
}

// addHTTPHandlers adds handlers for the HTTP endpoints exposed by the SSH manager.
func (m *manager) addHTTPHandlers(mux *http.ServeMux) {
	mux.HandleFunc(getComponentsToStartURL, protomsg.HandlerFunc(m.logger, m.getComponentsToStart))
	mux.HandleFunc(registerReplicaURL, protomsg.HandlerDo(m.logger, m.registerReplica))
	mux.HandleFunc(exportListenerURL, protomsg.HandlerFunc(m.logger, m.exportListener))
	mux.HandleFunc(startComponentURL, protomsg.HandlerDo(m.logger, m.startComponent))
	mux.HandleFunc(getRoutingInfoURL, protomsg.HandlerFunc(m.logger, m.getRoutingInfo))
	mux.HandleFunc(recvLogEntryURL, protomsg.HandlerDo(m.logger, m.handleLogEntry))
	mux.HandleFunc(recvTraceSpansURL, protomsg.HandlerDo(m.logger, m.handleTraceSpans))
	mux.HandleFunc(recvMetricsURL, protomsg.HandlerDo(m.logger, m.handleRecvMetrics))
}

// registerStatusPages registers the status pages with the provided mux.
func (m *manager) registerStatusPages(mux *http.ServeMux) {
	status.RegisterServer(mux, m, m.logger)
}

// Status implements the status.Server interface.
//
// TODO(rgrandl): the implementation is the same as the internal/babysitter.go.
// See if we can remove duplication.
func (m *manager) Status(ctx context.Context) (*status.Status, error) {
	state, _, err := m.loadAppState("" /*version*/)
	if err != nil {
		return nil, err
	}

	stats := m.statsProcessor.GetStatsStatusz()
	var components []*status.Component
	for _, g := range state.Groups {
		for component := range g.Components {
			c := &status.Component{
				Name:  component,
				Group: g.Name,
				Pids:  g.ReplicaPids,
			}
			components = append(components, c)

			s := stats[logging.ShortenComponent(component)]
			if s == nil {
				continue
			}
			for _, methodStats := range s {
				c.Methods = append(c.Methods, &status.Method{
					Name: methodStats.Name,
					Minute: &status.MethodStats{
						NumCalls:     methodStats.Minute.NumCalls,
						AvgLatencyMs: methodStats.Minute.AvgLatencyMs,
						RecvKbPerSec: methodStats.Minute.RecvKBPerSec,
						SentKbPerSec: methodStats.Minute.SentKBPerSec,
					},
					Hour: &status.MethodStats{
						NumCalls:     methodStats.Hour.NumCalls,
						AvgLatencyMs: methodStats.Hour.AvgLatencyMs,
						RecvKbPerSec: methodStats.Hour.RecvKBPerSec,
						SentKbPerSec: methodStats.Hour.SentKBPerSec,
					},
					Total: &status.MethodStats{
						NumCalls:     methodStats.Total.NumCalls,
						AvgLatencyMs: methodStats.Total.AvgLatencyMs,
						RecvKbPerSec: methodStats.Total.RecvKBPerSec,
						SentKbPerSec: methodStats.Total.SentKBPerSec,
					},
				})
			}
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	var listeners []*status.Listener
	for name, proxy := range m.proxies {
		listeners = append(listeners, &status.Listener{
			Name: name,
			Addr: proxy.addr,
		})
	}
	return &status.Status{
		App:            state.App,
		DeploymentId:   state.DeploymentId,
		SubmissionTime: state.SubmissionTime,
		Components:     components,
		Listeners:      listeners,
		Config:         m.dep.App,
	}, nil
}

// Metrics implements the status.Server interface.
func (m *manager) Metrics(context.Context) (*status.Metrics, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := &status.Metrics{}
	for _, snap := range m.metrics {
		ms.Metrics = append(ms.Metrics, snap...)
	}
	return ms, nil
}

// Profile implements the status.Server interface.
func (m *manager) Profile(context.Context, *protos.RunProfiling) (*protos.Profile, error) {
	return nil, nil
}

func (m *manager) getComponentsToStart(_ context.Context, req *protos.GetComponentsToStart) (
	*protos.ComponentsToStart, error) {
	// Load app state.
	state, newVersion, err := m.loadAppState(req.Version)
	if err != nil {
		return nil, err
	}
	g := m.findOrAddGroup(state, req.Group)

	// Return the components.
	var reply protos.ComponentsToStart
	reply.Version = newVersion
	reply.Components = maps.Keys(g.Components)
	return &reply, nil
}

func (m *manager) registerReplica(_ context.Context, req *protos.ReplicaToRegister) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Load app state.
	state, _, err := m.loadAppState("" /*version*/)
	if err != nil {
		return err
	}
	g := m.findOrAddGroup(state, req.Group)

	// Append the replica, if not already appended.
	var found bool
	for _, replica := range g.Replicas {
		if req.Address == replica {
			found = true
			break
		}
	}
	if !found {
		g.Replicas = append(g.Replicas, req.Address)
		g.ReplicaPids = append(g.ReplicaPids, req.Pid)
	}

	// Generate routing info, now that the replica set has changed.
	if err := m.mayGenerateNewRoutingInfo(g); err != nil {
		return err
	}

	// Store app state.
	m.appState.Update(appVersionStateKey, state)
	return nil
}

func (m *manager) exportListener(_ context.Context, req *protos.ExportListenerRequest) (
	*protos.ExportListenerReply, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Load app state.
	state, _, err := m.loadAppState("" /*version*/)
	if err != nil {
		return nil, err
	}

	// Update and store the state.
	state.Listeners = append(state.Listeners, req.Listener)
	m.appState.Update(appVersionStateKey, state)

	// Update the proxy.
	if p, ok := m.proxies[req.Listener.Name]; ok {
		p.proxy.AddBackend(req.Listener.Addr)
		return &protos.ExportListenerReply{ProxyAddress: p.addr}, nil
	}

	lis, err := net.Listen("tcp", req.LocalAddress)
	if errors.Is(err, syscall.EADDRINUSE) {
		// Don't retry if the address is already in use.
		return &protos.ExportListenerReply{Error: err.Error()}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("proxy listen: %w", err)
	}
	addr := lis.Addr().String()
	m.logger.Info("Proxy listening", "address", addr)
	proxy := proxy.NewProxy(m.logger)
	proxy.AddBackend(req.Listener.Addr)
	m.proxies[req.Listener.Name] = &proxyInfo{proxy: proxy, addr: addr}
	go func() {
		if err := serveHTTP(m.ctx, lis, proxy); err != nil {
			m.logger.Error("Proxy", err)
		}
	}()
	return &protos.ExportListenerReply{ProxyAddress: addr}, nil
}

func (m *manager) startComponent(ctx context.Context, req *protos.ComponentToStart) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Load app state.
	state, _, err := m.loadAppState("" /*version*/)
	if err != nil {
		return err
	}
	g := m.findOrAddGroup(state, req.ColocationGroup)

	// Update routing information.
	g.Components[req.Component] = req.IsRouted
	if req.IsRouted {
		if _, ok := g.Assignments[req.Component]; !ok {
			// Create an initial assignment for the component.
			g.Assignments[req.Component] = &protos.Assignment{
				App:          m.dep.App.Name,
				DeploymentId: m.dep.Id,
				Component:    req.Component,
			}
		}
	}
	if err := m.mayGenerateNewRoutingInfo(g); err != nil {
		return err
	}

	// Store app state
	m.appState.Update(appVersionStateKey, state)

	// Start the colocation group, if it hasn't started already.
	return m.startColocationGroup(ctx, &protos.ColocationGroup{Name: req.ColocationGroup})
}

func (m *manager) startColocationGroup(_ context.Context, group *protos.ColocationGroup) error {
	// If the group is already started, ignore.
	if _, found := m.started[group.Name]; found {
		return nil
	}

	// Start the main colocation group. Right now, the number of replicas for
	// each colocation group is equal with the number of locations.
	//
	// TODO(rgrandl): Implement some smarter logic to determine the number of
	// replicas for each group.
	for replicaId, loc := range m.locations {
		if err := m.startBabysitter(loc, group, replicaId); err != nil {
			return fmt.Errorf("unable to start babysitter for group %s at location %s: %w\n", group.Name, loc, err)
		}
		m.logger.Info("Started babysitter", "location", loc, "colocation group", group.Name)
	}
	m.started[group.Name] = true
	return nil
}

func (m *manager) handleLogEntry(_ context.Context, entry *protos.LogEntry) error {
	m.logSaver(entry)
	return nil
}

func (m *manager) handleTraceSpans(_ context.Context, spans *protos.Spans) error {
	if m.traceSaver == nil {
		return nil
	}
	return m.traceSaver(spans)
}

func (m *manager) handleRecvMetrics(_ context.Context, metrics *BabysitterMetrics) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.metrics[groupReplicaInfo{name: metrics.GroupName, id: metrics.ReplicaId}] = metrics.Metrics
	return nil
}

// startBabysitter starts a new babysitter that manages a colocation group using SSH.
func (m *manager) startBabysitter(loc string, group *protos.ColocationGroup, replicaId int) error {
	input, err := proto.ToEnv(&BabysitterInfo{
		ManagerAddr: m.mgrAddress,
		Deployment:  m.dep,
		Group:       group,
		ReplicaId:   int32(replicaId),
		LogDir:      m.logDir,
	})
	if err != nil {
		return err
	}

	env := fmt.Sprintf("%s=%s", babysitterInfoKey, input)
	binaryPath := filepath.Join(os.TempDir(), m.dep.Id, "weaver")
	cmd := exec.Command("ssh", loc, env, binaryPath, "ssh", "babysitter")
	return cmd.Start()
}

func (m *manager) getRoutingInfo(_ context.Context, req *protos.GetRoutingInfo) (
	*protos.RoutingInfo, error) {
	existing, newVersion, err := m.loadRoutingState(req.Group, req.Version)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		existing = &protos.RoutingInfo{}
	}
	existing.Version = newVersion
	return existing, nil
}

// mayGenerateNewRoutingInfo may generate new routing information for a given
// colocation group.
//
// This method is called whenever (1) the colocation group starts managing
// new routed components, or (2) a new replica of the colocation group gets
// started.
//
// REQUIRES: m.mu is held.
func (m *manager) mayGenerateNewRoutingInfo(g *ColocationGroupState) error {
	for component, currAssignment := range g.Assignments {
		newAssignment, err := routingAlgo(currAssignment, g.Replicas)
		if err != nil || newAssignment == nil {
			continue // don't update assignments
		}
		g.Assignments[component] = newAssignment
	}

	// Update the routing information.
	sort.Strings(g.Replicas)
	routingInfo := protos.RoutingInfo{
		Replicas: g.Replicas,
	}
	for _, assignment := range g.Assignments {
		routingInfo.Assignments = append(routingInfo.Assignments, assignment)
	}
	return m.updateRoutingInfo(g, &routingInfo)
}

// updateRoutingInfo update the state with the latest routing info for a
// colocation group.
// REQUIRES: m.mu is held.
func (m *manager) updateRoutingInfo(g *ColocationGroupState, info *protos.RoutingInfo) error {
	state, _, err := m.loadRoutingState(g.Name, "" /*version*/)
	if err != nil {
		return err
	}
	if gproto.Equal(state, info) { // Nothing to update
		return nil
	}
	m.routingState.Update(routingKey(g.Name), info)
	return nil
}

func (m *manager) loadRoutingState(group, version string) (*protos.RoutingInfo, string, error) {
	state, newVersion, err := m.routingState.Read(m.ctx, routingKey(group), version)
	if err != nil {
		return nil, "", err
	}
	if state == nil {
		state = &protos.RoutingInfo{}
	}
	return state, newVersion, nil
}

// routingAlgo is an implementation of a routing algorithm that distributes the
// entire key space approximately equally across all healthy resources.
//
// The algorithm is as follows:
// - split the entire key space in a number of slices that is more likely to
// spread uniformly the key space among all healthy resources
//
// - distribute the slices round robin across all healthy resources
func routingAlgo(currAssignment *protos.Assignment, candidates []string) (*protos.Assignment, error) {
	newAssignment := protomsg.Clone(currAssignment)
	newAssignment.Version++

	// Note that the healthy resources should be sorted. This is required because
	// we want to do a deterministic assignment of slices to resources among
	// different invocations, to avoid unnecessary churn while generating
	// new assignments.
	sort.Strings(candidates)

	if len(candidates) == 0 {
		newAssignment.Slices = nil
		return newAssignment, nil
	}

	const minSliceKey = 0
	const maxSliceKey = math.MaxUint64

	// If there is only one healthy resource, assign the entire key space to it.
	if len(candidates) == 1 {
		newAssignment.Slices = []*protos.Assignment_Slice{
			{Start: minSliceKey, Replicas: candidates},
		}
		return newAssignment, nil
	}

	// Compute the total number of slices in the assignment.
	numSlices := nextPowerOfTwo(len(candidates))

	// Split slices in equal subslices in order to generate numSlices.
	splits := [][]uint64{{minSliceKey, maxSliceKey}}
	var curr []uint64
	for ok := true; ok; ok = len(splits) != numSlices {
		curr, splits = splits[0], splits[1:]
		midPoint := curr[0] + uint64(math.Floor(0.5*float64(curr[1]-curr[0])))
		splitl := []uint64{curr[0], midPoint}
		splitr := []uint64{midPoint, curr[1]}
		splits = append(splits, splitl, splitr)
	}

	// Sort the computed slices in increasing order based on the start key, in
	// order to provide a deterministic assignment across multiple runs, hence to
	// minimize churn.
	sort.Slice(splits, func(i, j int) bool {
		return splits[i][0] <= splits[j][0]
	})

	// Assign the computed slices to resources in a round robin fashion.
	slices := make([]*protos.Assignment_Slice, len(splits))
	rId := 0
	for i, s := range splits {
		slices[i] = &protos.Assignment_Slice{
			Start:    s[0],
			Replicas: []string{candidates[rId]},
		}
		rId = (rId + 1) % len(candidates)
	}
	newAssignment.Slices = slices
	return newAssignment, nil
}

func (m *manager) loadAppState(version string) (*AppVersionState, string, error) {
	state, newVersion, err := m.appState.Read(m.ctx, appVersionStateKey, version)
	if err != nil {
		return nil, "", err
	}
	if state == nil {
		state = &AppVersionState{
			App:            m.dep.App.Name,
			DeploymentId:   m.dep.Id,
			SubmissionTime: timestamppb.Now(),
			Groups:         map[string]*ColocationGroupState{},
		}
	}
	// TODO(spetrovic): Versioned map stores empty maps as nil maps.
	// This means that it's not enough to initialize empty maps when
	// creating the new AppVersionState above.
	if state.Groups == nil {
		state.Groups = map[string]*ColocationGroupState{}
	}
	return state, newVersion, nil
}

func (m *manager) findOrAddGroup(state *AppVersionState, group string) *ColocationGroupState {
	g := state.Groups[group]
	if g == nil {
		g = &ColocationGroupState{
			Name: group,
		}
		state.Groups[group] = g
	}
	// TODO(spetrovic): Versioned map stores empty maps as nil maps.
	// This means that it's not enough to initialize empty maps when
	// creating the new ColocationGroupState above.
	if g.Components == nil {
		g.Components = map[string]bool{}
	}
	if g.Assignments == nil {
		g.Assignments = map[string]*protos.Assignment{}
	}
	return g
}

// serveHTTP serves HTTP traffic on the provided listener using the provided
// handler. The server is shut down when then provided context is cancelled.
func serveHTTP(ctx context.Context, lis net.Listener, handler http.Handler) error {
	server := http.Server{Handler: handler}
	errs := make(chan error, 1)
	go func() { errs <- server.Serve(lis) }()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		return server.Shutdown(ctx)
	}
}

// DefaultRegistry returns the default registry in
// $XDG_DATA_HOME/serviceweaver/ssh_registry, or
// ~/.local/share/serviceweaver/ssh_registry if XDG_DATA_HOME is not set.
func DefaultRegistry(ctx context.Context) (*status.Registry, error) {
	dir, err := files.DefaultDataDir()
	if err != nil {
		return nil, err
	}
	return status.NewRegistry(ctx, filepath.Join(dir, "ssh_registry"))
}

// nextPowerOfTwo returns the next power of 2 that is greater or equal to x.
func nextPowerOfTwo(x int) int {
	// If x is already power of 2, return x.
	if x&(x-1) == 0 {
		return x
	}
	return int(math.Pow(2, math.Ceil(math.Log2(float64(x)))))
}

func routingKey(group string) string {
	return path.Join(routingInfoKey, group)
}
