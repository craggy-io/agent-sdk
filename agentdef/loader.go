package agentdef

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"sync"

	"github.com/craggy-io/foundation-kit/bundle"
	"github.com/craggy-io/foundation-kit/kind"
)

// GoID owns definitions supplied by Go rather than bundles.
const GoID = "agentsdk.go"

// Options selects definition layers. Each layer may contain many bundles;
// higher layers replace lower definitions of the same kind and name.
// Duplicate names within a layer or in Sources remain errors.
type Options struct {
	// Shipped is the bottom layer, for example defs.Sources().
	Shipped []bundle.Source
	// UserDir is a tree of user bundles, rediscovered on reload. Runtime
	// installs create one bundle per definition beneath definitions/ or workflows/.
	UserDir string
	// Project is a tree of project bundles, the highest layer.
	Project string
	// Sources are shared libraries and extra definitions outside the override
	// hierarchy. Each layer may declare dependencies on these bundles.
	Sources     []bundle.Source
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
}

// Open loads the layers. The first load must succeed: a layer folder
// without a bundle.yaml, a file that fails its check or top level, or a
// name two non-layer bundles share is a *bundle.LoadError.
func Open(ctx context.Context, opts Options) (*Loader, error) {
	l := &Loader{opts: opts, compilers: Compilers()}
	l.opts.Sources = slices.Clone(opts.Sources)
	l.opts.Shipped = slices.Clone(opts.Shipped)
	for _, d := range opts.Definitions {
		if err := validGoDefinition(d); err != nil {
			return nil, err
		}
		l.defs = append(l.defs, d)
	}
	live, err := bundle.NewLive(ctx, l.options)
	if err != nil {
		return nil, err
	}
	l.live = live
	return l, nil
}

// options is the bundle.Live options function: it looks for the layer
// folders again on every load.
func (l *Loader) options(ctx context.Context) (bundle.Options, error) {
	o, err := l.layerOptions(ctx)
	if err != nil {
		return bundle.Options{}, err
	}
	l.mu.Lock()
	defs := slices.Clone(l.defs)
	l.mu.Unlock()
	if len(defs) > 0 {
		o.Providers = []kind.Provider{goProvider(defs)}
	}
	return o, nil
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

// Catalog returns the current catalog. It never blocks; read everything
// one task needs from a single catalog.
func (l *Loader) Catalog() *bundle.Catalog { return l.live.Catalog() }

// Reload loads the layers again (bundle.Live.Reload) and reports whether
// the catalog changed. On failure the previous catalog stays current and
// the error is returned, usually a *bundle.LoadError.
func (l *Loader) Reload(ctx context.Context) (bool, error) { return l.live.Reload(ctx) }

// UserDir returns Options.UserDir, where InstallAgent and InstallWorkflow
// write; "" when there is no user layer.
func (l *Loader) UserDir() string { return l.opts.UserDir }

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
		return err
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
