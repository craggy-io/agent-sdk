// Command host is a runnable example of the whole stack: a
// capability-services registry (jobs + agent capabilities + optional
// Starlark capability scripts), an agent host whose tools are a
// registry view, durable trace and memory on disk, and either an HTTP
// API or MCP over stdio in front. It contains no secrets: the model
// key comes from OPENROUTER_API_KEY, and without one the server still
// runs (sampling then fails with a clear error).
//
//	go run . -addr :8080                 # HTTP + OpenAPI on /v1/
//	go run . -mcp                         # MCP tools on stdio
//	go run . -defs ./mydefs -scripts ./caps -trace trace.db -memory ./mem
//
// Views are the security story: agents and MCP clients get the agent
// view (no admin- or host-tagged capabilities, so no promote and no
// answering their own ask gates); HTTP gets the full view in this
// example only — a real deployment puts auth in front of it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/craggy-io/agent-sdk/adapters/configyaml"
	"github.com/craggy-io/agent-sdk/adapters/tracesqlite"
	"github.com/craggy-io/agent-sdk/agentcap"
	"github.com/craggy-io/agent-sdk/agentdef"
	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/agent-sdk/defs"
	"github.com/craggy-io/agent-sdk/host"
	"github.com/craggy-io/agent-sdk/memory"
	"github.com/craggy-io/agent-sdk/model"
	"github.com/craggy-io/agent-sdk/openrouter"
	"github.com/craggy-io/agent-sdk/trace"
	tracemem "github.com/craggy-io/agent-sdk/trace/memory"
	_ "github.com/craggy-io/agent-sdk/workflow"
	"github.com/craggy-io/capability-services/httpapi"
	"github.com/craggy-io/capability-services/jobs"
	regmcp "github.com/craggy-io/capability-services/mcp"
	"github.com/craggy-io/capability-services/registry"
	"github.com/craggy-io/capability-services/starlarkcap"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		log.Fatal(err)
	}
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// errProvider stands in when no API key is configured, so the server
// still starts and only sampling fails.
type errProvider struct{}

func (errProvider) Sample(context.Context, model.Request, func(model.Delta)) (model.Response, error) {
	return model.Response{}, fmt.Errorf("no model provider: set OPENROUTER_API_KEY")
}

func (errProvider) Model(_ context.Context, id string) (model.ModelInfo, error) {
	return model.ModelInfo{}, fmt.Errorf("no model provider: set OPENROUTER_API_KEY")
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("host", flag.ContinueOnError)
	flags.SetOutput(stderr)
	addr := flags.String("addr", ":8080", "HTTP listen address")
	useMCP := flags.Bool("mcp", false, "serve MCP over stdio instead of HTTP")
	defsDir := flags.String("defs", "", "extra definition root (agents/, skills/, workflows/) shadowing the built-ins; also the promote target")
	scripts := flags.String("scripts", "", "directory of capability *.star files to load and hot-reload")
	tracePath := flags.String("trace", "", "SQLite trace file (default: in-memory, no resume across restarts)")
	memDir := flags.String("memory", "", "durable memory directory (default: in-memory)")
	cfgPath := flags.String("config", "", "session config YAML (default: built-in defaults)")
	approve := flags.Bool("approve-promote", false, "approve agents.promote/workflows.promote writes under -defs (payloads are logged)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	lg := log.New(stderr, "", log.LstdFlags)
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	cfg := config.Defaults()
	if *cfgPath != "" {
		loaded, err := configyaml.Load(*cfgPath)
		if err != nil {
			return err
		}
		cfg = *loaded
	}

	var sink trace.Sink = tracemem.New()
	if *tracePath != "" {
		store, err := tracesqlite.Open(*tracePath)
		if err != nil {
			return err
		}
		defer store.Close()
		sink = store
	}
	var mem memory.Store = memory.NewMemStore()
	if *memDir != "" {
		fm, err := memory.OpenFileStore(*memDir)
		if err != nil {
			return err
		}
		mem = fm
	}

	audit := func(next registry.Handler, c registry.Capability) registry.Handler {
		return func(ctx context.Context, in json.RawMessage) (any, error) {
			caller, _ := registry.CallerFrom(ctx)
			start := time.Now()
			out, err := next(ctx, in)
			lg.Printf("audit %s caller=%q err=%v in %s", c.Ref(), caller.ID, err, time.Since(start).Round(time.Millisecond))
			return out, err
		}
	}
	r := registry.New(registry.Use(audit))
	defer r.Close()
	if err := r.Add(jobs.Capabilities()...); err != nil {
		return err
	}
	if err := r.AddGroup(jobs.Group()); err != nil {
		return err
	}

	// The agent view: everything except admin (promote) and host
	// (ask-gate answering) capabilities.
	agentView := r.View(registry.And(
		registry.Not(registry.Tagged("admin")),
		registry.Not(registry.Tagged("host")),
	))

	var prov model.Provider = errProvider{}
	if key := os.Getenv("OPENROUTER_API_KEY"); key != "" {
		prov = openrouter.New(key)
	}
	roots := []fs.FS{defs.FS()}
	if *defsDir != "" {
		roots = append(roots, os.DirFS(*defsDir))
	}
	h, err := host.Open(ctx, host.Options{
		Config: cfg, View: host.NewRegistryView(agentView), Provider: prov,
		Defs: agentdef.Roots(roots...), Trace: sink,
		Caller: model.CallerInfo{ID: "user:local", Kind: "user"},
	})
	if err != nil {
		return err
	}
	defer h.Close()

	var approveFn func(context.Context, string, string, []byte) bool
	if *approve && *defsDir != "" {
		approveFn = func(_ context.Context, kind, name string, payload []byte) bool {
			lg.Printf("promote approved: %s %q (%d bytes)", kind, name, len(payload))
			return true
		}
	}
	if err := agentcap.Register(r, h, agentcap.Options{
		Root: *defsDir, Approve: approveFn, Memory: mem,
	}); err != nil {
		return err
	}
	if *scripts != "" {
		w, err := starlarkcap.Watch(ctx, r, starlarkcap.WatchOptions{
			Dir: *scripts,
			OnResult: func(prefix string, l starlarkcap.Loaded, err error) {
				switch {
				case err != nil:
					lg.Printf("scripts: %s: %v", prefix, err)
				case l.Digest == "":
					lg.Printf("scripts: %s: unloaded", prefix)
				default:
					lg.Printf("scripts: %s: loaded %s", prefix, strings.Join(l.Capabilities, ", "))
				}
			},
		})
		if err != nil {
			return err
		}
		defer w.Close()
	}

	listing := stdout
	if *useMCP {
		listing = stderr
	}
	fmt.Fprintln(listing, "registered:")
	for _, e := range r.List() {
		fmt.Fprintf(listing, "  %-28s %s\n", e.Capability.Ref(), e.Title)
	}

	if *useMCP {
		server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "agent-sdk-host", Version: "0.1.0"}, nil)
		stop := regmcp.Serve(server, agentView, regmcp.OnClose(func(c registry.Caller) {
			lg.Printf("mcp session closed: %s (%s)", c.ID, c.Session)
		}))
		defer stop()
		lg.Printf("serving MCP on stdio (agent view: %d tools)", len(agentView.List()))
		transport := &mcpsdk.IOTransport{Reader: io.NopCloser(stdin), Writer: nopCloser{stdout}}
		if err := server.Run(ctx, transport); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	}

	handler := httpapi.Handler(r.View(registry.All), httpapi.Caller(func(req *http.Request) registry.Caller {
		if u := req.Header.Get("X-User"); u != "" {
			return registry.Caller{ID: "user:" + u, Kind: "user"}
		}
		return registry.Caller{ID: "user:anonymous", Kind: "user"}
	}))
	srv := &http.Server{Addr: *addr, Handler: handler}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	lg.Printf("serving HTTP on %s — open http://localhost%s/v1/ and /v1/openapi.json", *addr, *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
