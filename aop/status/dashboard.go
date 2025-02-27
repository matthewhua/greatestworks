package status

import (
	"bytes"
	"context"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/pkg/browser"
	"google.golang.org/protobuf/types/known/timestamppb"
	"greatestworks/aop/codegen"
	"greatestworks/aop/logging"
	"greatestworks/aop/metrics"
	imetrics "greatestworks/aop/metrics"
	"greatestworks/aop/perfetto"
	"greatestworks/aop/protos"
	dtool "greatestworks/aop/tool"
)

var (
	dashboardFlags = flag.NewFlagSet("dashboard", flag.ContinueOnError)
	dashboardHost  = dashboardFlags.String("host", "localhost", "Dashboard host")
	dashboardPort  = dashboardFlags.Int("port", 0, "Dashboart port")

	//go:embed templates/index.html
	indexHTML     string
	indexTemplate = template.Must(template.New("index").Parse(indexHTML))

	//go:embed templates/deployment.html
	deploymentHTML     string
	deploymentTemplate = template.Must(template.New("deployment").Funcs(template.FuncMap{
		"shorten": logging.ShortenComponent,
		"pidjoin": func(pids []int64) string {
			s := make([]string, len(pids))
			for i, x := range pids {
				s[i] = fmt.Sprint(x)
			}
			return strings.Join(s, ", ")
		},
		"age": func(t *timestamppb.Timestamp) string {
			return time.Since(t.AsTime()).Truncate(time.Second).String()
		},
		"dec": func(x int) int {
			return x - 1
		},
		"traceurl": func(app, version string) string {
			v := url.Values{}
			v.Set("app", app)
			v.Set("version", version)
			tracerURL := url.QueryEscape("http://127.0.0.1:9001?" + v.Encode())
			return "https://ui.perfetto.dev/#!/?url=" + tracerURL
		},
	}).Parse(deploymentHTML))

	//go:embed assets/*
	assets embed.FS
)

// A Command is a labeled terminal command that a user can run. We show these
// commands on the dashboard so that users can copy and run them.
type Command struct {
	Label   string // e.g., cat logs
	Command string // e.g., weaver single logs '--version=="12345678"'
}

// DashboardSpec configures the command returned by DashboardCommand.
type DashboardSpec struct {
	Tool     string                                   // tool name (e.g., "weaver single")
	Registry func(context.Context) (*Registry, error) // registry of deployments
	Commands func(deploymentId string) []Command      // commands for a deployment
}

// DashboardCommand returns a "dashboard" subcommand that serves a dashboard
// with information about the active applications.
func DashboardCommand(spec *DashboardSpec) *dtool.Command {
	const help = `Usage:
  {{.Tool}} dashboard [--host=<host>] [--port=<port>]

Flags:
  -h, --help	Print this help message.
{{.Flags}}`
	var b strings.Builder
	t := template.Must(template.New("dashboard-help").Parse(help))
	content := struct{ Tool, Flags string }{spec.Tool, dtool.FlagsHelp(dashboardFlags)}
	if err := t.Execute(&b, content); err != nil {
		panic(err)
	}

	return &dtool.Command{
		Name:        "dashboard",
		Description: "Inspect Service Weaver applications",
		Help:        b.String(),
		Flags:       dashboardFlags,
		Fn: func(ctx context.Context, _ []string) error {
			r, err := spec.Registry(ctx)
			if err != nil {
				return err
			}
			dashboard := &dashboard{spec, r}
			http.HandleFunc("/", dashboard.handleIndex)
			http.HandleFunc("/favicon.ico", http.NotFound)
			http.HandleFunc("/deployment", dashboard.handleDeployment)
			http.HandleFunc("/metrics", dashboard.handleMetrics)
			http.Handle("/assets/", http.FileServer(http.FS(assets)))

			lis, err := net.Listen("tcp", fmt.Sprintf("%s:%d", *dashboardHost, *dashboardPort))
			if err != nil {
				return err
			}
			url := "http://" + lis.Addr().String()

			traceDB, err := perfetto.Open(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "cannot open Perfetto database: %v\n", err)
			}
			go traceDB.Serve(ctx)

			fmt.Fprintln(os.Stderr, "Dashboard available at:", url)
			go browser.OpenURL(url) //nolint:errcheck // browser open is optional
			return http.Serve(lis, nil)
		},
	}
}

// dashboard implements the "weaver dashboard" HTTP server.
type dashboard struct {
	spec     *DashboardSpec // e.g., "weaver multi" or "weaver single"
	registry *Registry      // registry of deployments
}

// handleIndex handles requests to /
func (d *dashboard) handleIndex(w http.ResponseWriter, r *http.Request) {
	regs, err := d.registry.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var statuses []*Status
	for _, reg := range regs {
		status, err := NewClient(reg.Addr).Status(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		statuses = append(statuses, status)
	}
	content := struct {
		Tool     string
		Statuses []*Status
	}{
		Tool:     d.spec.Tool,
		Statuses: statuses,
	}
	if err := indexTemplate.Execute(w, content); err != nil {
		panic(err)
	}
}

// handleDeployment handles requests to /deployment?id=<deployment id>
func (d *dashboard) handleDeployment(w http.ResponseWriter, r *http.Request) {
	// TODO(mwhittaker): Change to /<deployment id>?
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "no deployment id provided", http.StatusBadRequest)
		return
	}

	reg, err := d.registry.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	client := NewClient(reg.Addr)
	status, err := client.Status(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Sort components and methods so that they appear in a deterministic order
	// on the deployment page.
	sort.Slice(status.Components, func(i, j int) bool {
		ci, cj := status.Components[i], status.Components[j]
		if ci.Group != cj.Group {
			return ci.Group < cj.Group
		}
		return ci.Name < cj.Name
	})
	for _, component := range status.Components {
		sort.Slice(component.Methods, func(i, j int) bool {
			return component.Methods[i].Name < component.Methods[j].Name
		})
	}

	// Fetch metrics.
	metrics, err := client.Metrics(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Display content.
	content := struct {
		*Status
		Tool     string
		Traffic  []edge
		Commands []Command
	}{
		Status:   status,
		Tool:     d.spec.Tool,
		Traffic:  computeTraffic(status, metrics.Metrics),
		Commands: d.spec.Commands(id),
	}
	if err := deploymentTemplate.Execute(w, content); err != nil {
		fmt.Println(err)
	}
}

// An edge represents an edge in a traffic graph. If a component s calls n
// methods on component t, then an edge is formed from s to t with weight v.
type edge struct {
	Source string // calling component
	Target string // callee component
	Value  int    // number of method calls
}

// computeTraffic calculates cross-component traffic.
func computeTraffic(status *Status, metrics []*protos.MetricSnapshot) []edge {
	// Aggregate traffic by component.
	type pair struct {
		caller    string
		component string
	}
	byPair := map[pair]int{}
	for _, metric := range metrics {
		if metric.Name != codegen.MethodCounts.Name() {
			continue
		}
		call := pair{
			caller:    metric.Labels["caller"],
			component: metric.Labels["component"],
		}
		byPair[call] += int(metric.Value)
	}

	// Massage data into graph format.
	var edges []edge
	for call, value := range byPair {
		edges = append(edges, edge{
			Source: call.caller,
			Target: call.component,
			Value:  value,
		})
	}
	return edges
}

// handleMetrics handles requests to /metrics?id=<deployment id>
func (d *dashboard) handleMetrics(w http.ResponseWriter, r *http.Request) {
	// TODO(mwhittaker): Change to /<deployment id>/metrics?
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "no deployment id provided", http.StatusBadRequest)
		return
	}

	reg, err := d.registry.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ms, err := NewClient(reg.Addr).Metrics(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	snapshots := make([]*metrics.MetricSnapshot, len(ms.Metrics))
	for i, m := range ms.Metrics {
		snapshots[i] = metrics.UnProto(m)
	}

	var b bytes.Buffer
	imetrics.TranslateMetricsToPrometheusTextFormat(&b, snapshots, reg.Addr, prometheusEndpoint)
	w.Write(b.Bytes()) //nolint:errcheck // response write error
}
