package agentdef

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/craggy-io/foundation-kit/bundle"
	"github.com/craggy-io/foundation-kit/kind"
)

// SourcesIn discovers a tree of real bundles. A directory with bundle.yaml is
// one bundle; otherwise its non-hidden subdirectories are groups. Only bundle.Load
// reads manifests or authored content. Missing/empty roots are empty layers.
func SourcesIn(ctx context.Context, root string) ([]bundle.Source, error) {
	if root == "" {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name() == "bundle.yaml" {
			return []bundle.Source{bundle.Dir(root)}, nil
		}
	}
	var out []bundle.Source
	orphan := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !e.IsDir() {
			orphan = orphan || strings.HasSuffix(e.Name(), ".star")
			continue
		}
		found, err := SourcesIn(ctx, filepath.Join(root, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	if orphan {
		return nil, fmt.Errorf("agentdef: %s has definition files but has no bundle.yaml", root)
	}
	return out, nil
}

type definitionKey struct {
	kind kind.ID
	name string
}

func addRefs[T any](out map[definitionKey]bundle.Ref, cat *bundle.Catalog, k *kind.Kind[T], exclude map[string]bool) {
	for _, d := range bundle.All(cat, k) {
		if !exclude[d.Owner.ID] {
			out[definitionKey{k.ID(), d.Ref.ID}] = d.Ref
		}
	}
}
func layerRefs(cat *bundle.Catalog, exclude map[string]bool) map[definitionKey]bundle.Ref {
	out := map[definitionKey]bundle.Ref{}
	addRefs(out, cat, AgentKind, exclude)
	addRefs(out, cat, SkillKind, exclude)
	addRefs(out, cat, WorkflowKind, exclude)
	return out
}

// Each layer is validated by the standard loader before deriving explicit
// contribution overrides. The final Live load validates the combined catalog
// and publishes it atomically; no manifest parsing or content loading lives here.
func (l *Loader) layerOptions(ctx context.Context) (bundle.Options, error) {
	o := bundle.Options{Sources: append([]bundle.Source(nil), l.opts.Sources...), Compilers: l.compilers}
	external, err := bundle.Load(ctx, o)
	if err != nil {
		return o, err
	}
	excluded := map[string]bool{}
	for _, owner := range external.Owners() {
		excluded[owner.ID] = true
	}
	layers := [][]bundle.Source{l.opts.Shipped}
	for _, root := range []string{l.opts.UserDir, l.opts.Project} {
		sources, err := SourcesIn(ctx, root)
		if err != nil {
			return o, err
		}
		layers = append(layers, sources)
	}
	previous := map[definitionKey][]bundle.Ref{}
	owners := map[string]bool{}
	for _, sources := range layers {
		if len(sources) == 0 {
			continue
		}
		cat, err := bundle.Load(ctx, bundle.Options{Sources: append(append([]bundle.Source(nil), l.opts.Sources...), sources...), Compilers: l.compilers})
		if err != nil {
			return o, err
		}
		for _, owner := range cat.Owners() {
			if excluded[owner.ID] {
				continue
			}
			if owners[owner.ID] {
				return o, fmt.Errorf("agentdef: bundle %s belongs to multiple layers", owner.ID)
			}
			owners[owner.ID] = true
		}
		for key, ref := range layerRefs(cat, excluded) {
			previous[key] = append(previous[key], ref)
		}
		o.Sources = append(o.Sources, sources...)
	}
	for _, refs := range previous {
		for _, ref := range refs[:len(refs)-1] {
			o.Overrides = append(o.Overrides, bundle.Override{Target: ref, Replacement: refs[len(refs)-1]})
		}
	}
	return o, nil
}
