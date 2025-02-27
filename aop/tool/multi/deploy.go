package multi

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"greatestworks/aop"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/google/uuid"
	"greatestworks/aop/babysitter"
	"greatestworks/aop/codegen"
	"greatestworks/aop/colors"
	"greatestworks/aop/files"
	"greatestworks/aop/logging"
	"greatestworks/aop/protos"
	"greatestworks/aop/retry"
	"greatestworks/aop/status"
	"greatestworks/aop/tool"
)

var deployCmd = tool.Command{
	Name:        "deploy",
	Description: "Deploy a Service Weaver app",
	Help:        "Usage:\n  weaver multi deploy <configfile>",
	Flags:       flag.NewFlagSet("deploy", flag.ContinueOnError),
	Fn:          deploy,
}

// deploy deploys an application on the local machine using a multiprocess
// deployer. Note that each component is deployed as a separate OS process.
func deploy(ctx context.Context, args []string) error {
	// Validate command line arguments.
	if len(args) == 0 {
		return fmt.Errorf("no config file provided")
	}
	if len(args) > 1 {
		return fmt.Errorf("too many arguments")
	}

	// Load the config file.
	cfgFile := args[0]
	cfg, err := os.ReadFile(cfgFile)
	if err != nil {
		return fmt.Errorf("load config file %q: %w\n", cfgFile, err)
	}
	app, err := aop.ParseConfig(cfgFile, string(cfg), codegen.ComponentConfigValidator)
	if err != nil {
		return fmt.Errorf("load config file %q: %w\n", cfgFile, err)
	}

	// Sanity check the config.
	if _, err := os.Stat(app.Binary); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("binary %q doesn't exist", app.Binary)
	}

	// Create the log saver.
	fs, err := logging.NewFileStore(logdir)
	if err != nil {
		return fmt.Errorf("cannot create log storage: %w", err)
	}
	logSaver := fs.Add

	// Create the babysitter.
	dep := &protos.Deployment{
		Id:  uuid.New().String(),
		App: app,
	}
	b, err := babysitter.NewBabysitter(ctx, dep, logSaver)
	if err != nil {
		return fmt.Errorf("create babysitter: %w", err)
	}

	// Run a status server.
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	mux := http.NewServeMux()
	b.RegisterStatusPages(mux)
	go func() {
		if err := serveHTTP(ctx, lis, mux); err != nil {
			fmt.Fprintf(os.Stderr, "status server: %v\n", err)
		}
	}()

	// Deploy main.go.
	group := &protos.ColocationGroup{Name: "main.go"}
	if err := b.StartComponent(&protos.ComponentToStart{
		ColocationGroup: group.Name,
		Component:       "main.go",
	}); err != nil {
		return fmt.Errorf("start main.go process: %w", err)
	}

	// Wait for the status server to become active.
	client := status.NewClient(lis.Addr().String())
	for r := retry.Begin(); r.Continue(ctx); {
		_, err := client.Status(ctx)
		if err == nil {
			break
		}
		fmt.Fprintf(os.Stderr, "status server %q unavailable: %#v\n", lis.Addr(), err)
	}

	// AddHandler the deployment.
	registry, err := defaultRegistry(ctx)
	if err != nil {
		return fmt.Errorf("create registry: %w", err)
	}
	reg := status.Registration{
		DeploymentId: dep.Id,
		App:          app.Name,
		Addr:         lis.Addr().String(),
	}
	fmt.Fprint(os.Stderr, reg.Rolodex())
	if err := registry.Register(ctx, reg); err != nil {
		return fmt.Errorf("register deployment: %w", err)
	}

	// Wait for the user to kill the app.
	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-done // Will block here until user hits ctrl+c
		fmt.Fprintf(os.Stderr, "Application %s terminated\n", app.Name)
		if err := registry.Unregister(ctx, dep.Id); err != nil {
			fmt.Fprintf(os.Stderr, "unregister deployment: %v\n", err)
		}
		os.Exit(1)
	}()

	// Follow the logs.
	source := logging.FileSource(logdir)
	query := fmt.Sprintf(`full_version == %q && !("serviceweaver/system" in attrs)`, dep.Id)
	r, err := source.Query(ctx, query, true)
	if err != nil {
		return err
	}
	pp := logging.NewPrettyPrinter(colors.Enabled())
	for {
		entry, err := r.Read(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		fmt.Println(pp.Format(entry))
	}
}

// defaultRegistry returns the default registry in
// $XDG_DATA_HOME/serviceweaver/multi_registry, or
// ~/.local/share/serviceweaver/multi_registry if XDG_DATA_HOME is not set.
func defaultRegistry(ctx context.Context) (*status.Registry, error) {
	dir, err := files.DefaultDataDir()
	if err != nil {
		return nil, err
	}
	return status.NewRegistry(ctx, filepath.Join(dir, "multi_registry"))
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
