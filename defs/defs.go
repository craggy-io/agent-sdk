// Package defs supplies the SDK's default logical definition bundles.
// Hosts pass Sources to agentdef.Options.Shipped; user and project bundles
// replace definitions by kind and name.
package defs

import (
	"embed"
	"github.com/craggy-io/foundation-kit/bundle"
	"io/fs"
	"path"
)

//go:embed definitions skills workflows shared
var embedded embed.FS

// Sources returns one source per authored bundle, including shared helpers.
func Sources() []bundle.Source {
	manifests, err := fs.Glob(embedded, "*/*/bundle.yaml")
	if err != nil {
		panic(err)
	}
	sources := make([]bundle.Source, 0, len(manifests))
	for _, manifest := range manifests {
		dir := path.Dir(manifest)
		files, err := fs.Sub(embedded, dir)
		if err != nil {
			panic(err)
		}
		sources = append(sources, bundle.Embedded("agent-sdk/defs/"+dir, files))
	}
	return sources
}
