package agentdef

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing/fstest"

	"github.com/craggy-io/foundation-kit/bundle"
	"github.com/craggy-io/foundation-kit/kind"
)

// InstallAgent adds or replaces agent name in the user layer: src is
// written to the file the user bundle already declares for name, or to
// agents/<name>.star with a new contribution entry in its bundle.yaml, and
// the rewritten bundle is installed with bundle.Live.Install at
// <UserDir>/agentsdk.user. The install reloads every layer and is rolled
// back if that load fails, for example because src does not declare
// agent name. It returns the path of the written file.
func (l *Loader) InstallAgent(ctx context.Context, name string, src []byte) (string, error) {
	return l.installUser(ctx, AgentKind.ID(), name, "agents/"+name+".star", src, func(cat *bundle.Catalog) (bundle.Identity, string, bool) {
		def, ok := bundle.Lookup(cat, AgentKind, name)
		if !ok {
			return bundle.Identity{}, "", false
		}
		return def.Owner, def.Value.Source, true
	})
}

// InstallWorkflow is InstallAgent for a workflow, written to
// workflows/<name>.star when the user bundle does not declare it yet.
func (l *Loader) InstallWorkflow(ctx context.Context, name string, src []byte) (string, error) {
	return l.installUser(ctx, WorkflowKind.ID(), name, "workflows/"+name+".star", src, func(cat *bundle.Catalog) (bundle.Identity, string, bool) {
		def, ok := bundle.Lookup(cat, WorkflowKind, name)
		if !ok {
			return bundle.Identity{}, "", false
		}
		return def.Owner, def.Value.Unit.Path(), true
	})
}

// installUser rewrites the user bundle with name's file and installs it.
// current reports the effective definition named name: its owner and its
// path in that owner's bundle.
func (l *Loader) installUser(ctx context.Context, k kind.ID, name, defaultPath string, src []byte,
	current func(*bundle.Catalog) (bundle.Identity, string, bool)) (string, error) {
	if l.opts.UserDir == "" {
		return "", errors.New("agentdef: no user layer to install into (Options.UserDir is empty)")
	}
	l.installMu.Lock()
	defer l.installMu.Unlock()
	cat := l.Catalog()
	files, err := readUserBundle(l.userBundle())
	if err != nil {
		return "", err
	}
	p := defaultPath
	declared := false
	if owner, at, ok := current(cat); ok {
		switch {
		case owner.ID == UserID:
			p, declared = at, true
		case shadows(cat, bundle.Ref{Owner: UserID, ID: name}):
			return "", fmt.Errorf("agentdef: %s is defined by layer %s, which replaces the user layer's; installing it there would have no effect", name, owner.ID)
		}
	}
	manifest := newUserManifest()
	if f, ok := files["bundle.yaml"]; ok {
		manifest = f.Data
	}
	if !declared {
		if _, taken := files[p]; taken {
			return "", fmt.Errorf("agentdef: user bundle already has %s, which bundle.yaml does not declare as %s; declare it there or move it", p, name)
		}
		if manifest, err = addContribution(manifest, name, k, p); err != nil {
			return "", err
		}
	}
	files["bundle.yaml"] = &fstest.MapFile{Data: manifest, Mode: 0o644}
	files[p] = &fstest.MapFile{Data: append([]byte(nil), src...), Mode: 0o644}
	if _, err := l.live.Install(ctx, l.opts.UserDir, files); err != nil {
		return "", loadError(err)
	}
	return filepath.Join(l.userBundle(), filepath.FromSlash(p)), nil
}

// shadows reports whether target is replaced in cat.
func shadows(cat *bundle.Catalog, target bundle.Ref) bool {
	for _, o := range cat.Overrides() {
		if o.Target == target {
			return true
		}
	}
	return false
}

// readUserBundle copies every file of the user bundle; none when it does
// not exist yet. A symbolic link is an error, as it is to bundle.Load.
func readUserBundle(dir string) (fstest.MapFS, error) {
	files := fstest.MapFS{}
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return files, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agentdef: user layer: %w", err)
	}
	defer root.Close()
	fsys := root.FS()
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s is a symbolic link", p)
		case d.IsDir():
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		files[p] = &fstest.MapFile{Data: data, Mode: 0o644}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("agentdef: user layer %s: %w", dir, err)
	}
	return files, nil
}

// newUserManifest is the bundle.yaml of a user layer that does not exist
// yet; addContribution adds its first entry.
func newUserManifest() []byte {
	return []byte(`# The user definition layer (agentdef.UserID). It replaces shipped
# definitions with the same name. agents.promote and workflows.promote add
# entries here; you can edit it by hand too.
apiVersion: craggy.io/bundle/v1

metadata:
  id: ` + UserID + `
  version: 1.0.0

contributions:
`)
}

var (
	contributionsKey   = regexp.MustCompile(`^contributions:\s*(#.*)?$`)
	contributionsEmpty = regexp.MustCompile(`^contributions:\s*\[\s*\]\s*(#.*)?$`)
	blockItem          = regexp.MustCompile(`^(\s*)- `)
)

// addContribution adds one entry to a bundle.yaml's contributions, as
// the first item of its block list, keeping the rest of the file (comments
// included) as written. The install that follows validates the result.
func addContribution(manifest []byte, id string, k kind.ID, path string) ([]byte, error) {
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(manifest))
	sc.Buffer(make([]byte, 0, 64*1024), len(manifest)+1)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	entry := func(indent string) string {
		return fmt.Sprintf("%s- {id: %s, kind: %s, path: %s}", indent, strconv.Quote(id), k, strconv.Quote(path))
	}
	for i, line := range lines {
		switch {
		case contributionsEmpty.MatchString(line):
			lines[i] = "contributions:"
			lines = slices.Insert(lines, i+1, entry("  "))
			return joinLines(lines), nil
		case contributionsKey.MatchString(line):
			indent := "  "
			for _, next := range lines[i+1:] {
				t := strings.TrimSpace(next)
				if t == "" || strings.HasPrefix(t, "#") {
					continue
				}
				if m := blockItem.FindStringSubmatch(next); m != nil {
					indent = m[1]
				}
				break
			}
			lines = slices.Insert(lines, i+1, entry(indent))
			return joinLines(lines), nil
		case strings.HasPrefix(line, "contributions:"):
			return nil, errors.New("agentdef: the user bundle.yaml writes contributions as a flow list; write it as a block list (one \"- \" item per line) so entries can be added")
		}
	}
	lines = append(lines, "contributions:", entry("  "))
	return joinLines(lines), nil
}

func joinLines(lines []string) []byte { return []byte(strings.Join(lines, "\n") + "\n") }
