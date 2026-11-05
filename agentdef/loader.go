package agentdef

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/craggy-io/foundation-kit/bundle"
	"github.com/craggy-io/foundation-kit/kind"
)

// The owner IDs of the definition layers and of Go-built definitions. A
// layer's bundle.yaml must use its layer's metadata.id.
const (
	ShippedID = "agentsdk.shipped" // Options.Shipped, such as defs.Source()
	UserID    = "agentsdk.user"    // the bundle at <Options.UserDir>/agentsdk.user
	ProjectID = "agentsdk.project" // the bundle at Options.Project
	GoID      = "agentsdk.go"      // Options.Definitions and AddDefinition
)

// Options choose the bundles a Loader catalogues. Every layer is one
// bundle, a folder with a bundle.yaml whose metadata.id is the layer's ID.
// Layers replace each other by contribution id, explicitly, through a
// bundle override chain: user replaces shipped, project replaces user (and
// shipped, when there is no user layer). Catalog.Overrides reports what
// was replaced.
type Options struct {
	// Shipped is the bottom layer, usually defs.Source(). Its
	// metadata.id must be ShippedID. nil: none.
	Shipped bundle.Source
	// UserDir holds the user layer: the bundle at
	// <UserDir>/agentsdk.user (metadata.id UserID). It is where
	// InstallAgent and InstallWorkflow write (agents.promote and
	// workflows.promote), through bundle.Live.Install, which places a
	// bundle at <dir>/<metadata.id>; other folders in UserDir are not
	// read. A missing agentsdk.user folder is no user layer yet. "": none.
	UserDir string
	// Project is the top layer: a bundle directory whose metadata.id is
	// ProjectID. A missing directory is skipped. "": none.
	Project string
	// Sources are other bundles: libraries that layers declare as
	// dependencies and load() as @<id>/path, or extra definitions. They
	// replace nothing, so a name one of them shares with a layer is a
	// bundle.id_conflict error.
	Sources []bundle.Source
	// Definitions are built in Go, owned by GoID. They replace nothing
	// either. See AddDefinition.
	Definitions []*Definition
}

// Loader is the live catalogue of agent definitions, skills and
// workflows: a bundle.Live over the layers in Options. Every read takes the
// current catalog, which a reload replaces whole or not at all, so a run
// in progress keeps what it resolved and a bad edit never half-replaces a
// working catalogue.
type Loader struct {
	opts      Options
	compilers []bundle.AnyCompiler
	live      *bundle.Live

	mu   sync.Mutex // guards defs
	defs []*Definition

	installMu sync.Mutex // serializes InstallAgent/InstallWorkflow
}

// Open loads the layers. The first load must succeed: a layer folder
// without a bundle.yaml, a file that fails its check or top level, or a
// name two non-layer bundles share is an error listing every problem.
func Open(ctx context.Context, opts Options) (*Loader, error) {
	l := &Loader{opts: opts, compilers: Compilers()}
	l.opts.Sources = slices.Clone(opts.Sources)
	for _, d := range opts.Definitions {
		if err := validGoDefinition(d); err != nil {
			return nil, err
		}
		l.defs = append(l.defs, d)
	}
	live, err := bundle.NewLive(ctx, l.options)
	if err != nil {
		return nil, loadError(err)
	}
	l.live = live
	return l, nil
}

// options is the bundle.Live options function: it looks for the layer
// folders again on every load.
func (l *Loader) options(context.Context) (bundle.Options, error) {
	o := bundle.Options{Compilers: l.compilers, Sources: slices.Clone(l.opts.Sources)}
	var layers []string
	if l.opts.Shipped != nil {
		o.Sources = append(o.Sources, l.opts.Shipped)
		layers = append(layers, ShippedID)
	}
	for _, layer := range []struct{ id, dir string }{{UserID, l.userBundle()}, {ProjectID, l.opts.Project}} {
		ok, err := layerPresent(layer.id, layer.dir)
		if err != nil {
			return bundle.Options{}, err
		}
		if ok {
			o.Sources = append(o.Sources, bundle.Dir(layer.dir))
			layers = append(layers, layer.id)
		}
	}
	for i := 1; i < len(layers); i++ {
		o.BundleOverrides = append(o.BundleOverrides, bundle.BundleOverride{Target: layers[i-1], Replacement: layers[i]})
	}
	l.mu.Lock()
	defs := slices.Clone(l.defs)
	l.mu.Unlock()
	if len(defs) > 0 {
		o.Providers = []kind.Provider{goProvider(defs)}
	}
	return o, nil
}

// layerPresent reports whether a layer folder exists. One without a
// bundle.yaml is an error: the folder is meant to be a layer.
func layerPresent(id, dir string) (bool, error) {
	if dir == "" {
		return false, nil
	}
	st, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("agentdef: layer %s: %w", id, err)
	case !st.IsDir():
		return false, fmt.Errorf("agentdef: layer %s: %s is not a folder", id, dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "bundle.yaml")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("agentdef: layer %s: %s has no bundle.yaml; a definition folder needs one, with metadata.id %s, listing its agents, skills and workflows", id, dir, id)
		}
		return false, fmt.Errorf("agentdef: layer %s: %w", id, err)
	}
	return true, nil
}

// goProvider holds the Go-built definitions. A built-in has no bytes to
// hash, so the version is a digest of the definitions: changing one
// changes the catalog digest, which is what makes a reload swap it in.
func goProvider(defs []*Definition) kind.Provider {
	b, _ := json.Marshal(defs)
	sum := sha256.Sum256(b)
	p := kind.Provider{ID: GoID, Version: "sha256-" + hex.EncodeToString(sum[:8])}
	for _, d := range defs {
		p.Builtins = append(p.Builtins, kind.Builtin(AgentKind, d.Name, d))
	}
	return p
}

func validGoDefinition(d *Definition) error {
	if d == nil || d.Name == "" {
		return errors.New("agentdef: a Go definition needs a name")
	}
	return ValidateMaxEffect(d.Name, d.Tools.MaxEffect)
}

// loadFailure lists every diagnostic of a failed load.
type loadFailure struct{ err *bundle.LoadError }

func (e loadFailure) Error() string {
	parts := make([]string, 0, len(e.err.Diagnostics))
	for _, d := range e.err.Diagnostics {
		if d.Severity == bundle.SeverityError {
			parts = append(parts, formatDiagnostic(d))
		}
	}
	return "agentdef: definitions failed to load: " + strings.Join(parts, "; ")
}

func (e loadFailure) Unwrap() error { return e.err }

// loadError wraps a bundle load error so its message lists every problem;
// errors.As still finds the *bundle.LoadError.
func loadError(err error) error {
	var le *bundle.LoadError
	if errors.As(err, &le) {
		return loadFailure{le}
	}
	return err
}

// Catalog returns the current catalog. It never blocks; read everything
// one task needs from a single catalog.
func (l *Loader) Catalog() *bundle.Catalog { return l.live.Catalog() }

// Reload loads the layers again (bundle.Live.Reload) and reports whether
// the catalog changed. On failure the previous catalog stays current and
// the error lists every problem.
func (l *Loader) Reload(ctx context.Context) (bool, error) {
	changed, err := l.live.Reload(ctx)
	return changed, loadError(err)
}

// Shadowed returns every definition a higher layer replaced, from the
// current catalog (bundle.Catalog.Overrides).
func (l *Loader) Shadowed() []bundle.Override { return l.Catalog().Overrides() }

// UserDir returns Options.UserDir, where InstallAgent and InstallWorkflow
// write; "" when there is no user layer.
func (l *Loader) UserDir() string { return l.opts.UserDir }

func (l *Loader) userBundle() string {
	if l.opts.UserDir == "" {
		return ""
	}
	return filepath.Join(l.opts.UserDir, UserID)
}

// AddDefinition adds or replaces a Go-built definition (owned by GoID)
// and reloads. It replaces nothing from a bundle: a name a bundle also
// defines is a bundle.id_conflict error, and the definition is not kept.
func (l *Loader) AddDefinition(ctx context.Context, d *Definition) error {
	if err := validGoDefinition(d); err != nil {
		return err
	}
	l.mu.Lock()
	prev := slices.Clone(l.defs)
	l.defs = slices.DeleteFunc(slices.Clone(l.defs), func(x *Definition) bool { return x.Name == d.Name })
	l.defs = append(l.defs, d)
	l.mu.Unlock()
	if _, err := l.live.Reload(ctx); err != nil {
		l.mu.Lock()
		l.defs = prev
		l.mu.Unlock()
		return loadError(err)
	}
	return nil
}

func lookup[T any](cat *bundle.Catalog, k *kind.Kind[T], name string) (T, bool) {
	def, ok := bundle.Lookup(cat, k, name)
	return def.Value, ok
}

func names[T any](cat *bundle.Catalog, k *kind.Kind[T]) []string {
	defs := bundle.All(cat, k)
	out := make([]string, 0, len(defs))
	for _, def := range defs {
		out = append(out, def.Ref.ID)
	}
	sort.Strings(out)
	return out
}

// Get returns the raw (unresolved) definition for name.
func (l *Loader) Get(name string) (*Definition, bool) { return lookup(l.Catalog(), AgentKind, name) }

// Names returns definition names, sorted.
func (l *Loader) Names() []string { return names(l.Catalog(), AgentKind) }

// Skill returns a skill by name.
func (l *Loader) Skill(name string) (*Skill, bool) { return lookup(l.Catalog(), SkillKind, name) }

// SkillBody returns a skill's body for the loop's skill built-in.
func (l *Loader) SkillBody(name string) (string, bool) {
	s, ok := l.Skill(name)
	if !ok {
		return "", false
	}
	return s.Body, true
}

// SkillNames returns skill names, sorted.
func (l *Loader) SkillNames() []string { return names(l.Catalog(), SkillKind) }

// Workflows returns catalogued workflow names, sorted.
func (l *Loader) Workflows() []string { return names(l.Catalog(), WorkflowKind) }

// Workflow returns a catalogued workflow.
func (l *Loader) Workflow(name string) (*Workflow, bool) {
	return lookup(l.Catalog(), WorkflowKind, name)
}
