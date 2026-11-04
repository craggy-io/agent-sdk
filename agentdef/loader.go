package agentdef

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"

	"go.starlark.net/starlark"

	"github.com/craggy-io/script-services/dialect"
	"github.com/craggy-io/script-services/modules"
	"github.com/craggy-io/script-services/script"
)

// Loader loads definitions, skills and workflow sources from ordered
// roots: shipped, then user, then project. A later root shadows an
// earlier one by name; shadowed names are recorded in Shadowed as
// "agent:<name>", "skill:<name>" or "workflow:<name>". A root without
// the expected subdirectories contributes nothing.
//
// Each root is an fs.FS with agents/*.star, skills/*/SKILL.md and
// workflows/*.star. Load builds fresh maps and swaps them in only on
// success, so a run in progress keeps the snapshot it resolved from and
// a bad edit cannot half-replace a working catalogue.
type Loader struct {
	roots []fs.FS

	mu          sync.RWMutex
	defs        map[string]*Definition
	skills      map[string]*Skill
	workflowSrc map[string]string // name -> source text
	workflowDir map[string]fs.FS  // name -> root serving its load()s
	digest      string            // content digest of catalogued files
	loaded      bool

	// Shadowed records names replaced by a later root at Load time.
	Shadowed []string
}

// Roots builds a Loader over filesystems, earliest first.
func Roots(fsys ...fs.FS) *Loader {
	return &Loader{roots: append([]fs.FS(nil), fsys...)}
}

// DirRoots builds a Loader over directories, earliest first. Missing
// directories are skipped. Definitions are trusted host configuration,
// like the workflows they sit beside.
func DirRoots(dirs ...string) *Loader {
	l := &Loader{}
	for _, d := range dirs {
		l.roots = append(l.roots, os.DirFS(d))
	}
	return l
}

// Load (re)reads every root.
func (l *Loader) Load() error {
	defs := map[string]*Definition{}
	skills := map[string]*Skill{}
	wfs := map[string]string{}
	wfDirs := map[string]fs.FS{}
	var shadowed []string
	h := sha256.New()

	for _, root := range l.roots {
		// agents/*.star
		agentFiles, _ := fs.Glob(root, "agents/*.star")
		sort.Strings(agentFiles)
		for _, file := range agentFiles {
			data, err := fs.ReadFile(root, file)
			if err != nil {
				return fmt.Errorf("agentdef: read %s: %w", file, err)
			}
			fmt.Fprintf(h, "%s:%x\n", file, sha256.Sum256(data))
			col := &collector{
				file: file, dir: path.Dir(file),
				read: func(name string) ([]byte, error) { return fs.ReadFile(root, name) },
			}
			if err := execDefFile(root, file, data, col); err != nil {
				return err
			}
			for _, d := range col.defs {
				d.SourceText = append([]byte(nil), data...)
				if _, exists := defs[d.Name]; exists {
					shadowed = append(shadowed, "agent:"+d.Name)
				}
				defs[d.Name] = d
			}
			for _, sk := range col.skill {
				if _, exists := skills[sk.Name]; exists {
					shadowed = append(shadowed, "skill:"+sk.Name)
				}
				skills[sk.Name] = sk
			}
		}
		// skills/*/SKILL.md
		skillFiles, _ := fs.Glob(root, "skills/*/SKILL.md")
		sort.Strings(skillFiles)
		for _, file := range skillFiles {
			data, err := fs.ReadFile(root, file)
			if err != nil {
				continue
			}
			fmt.Fprintf(h, "%s:%x\n", file, sha256.Sum256(data))
			sk, err := parseSkill(data, path.Base(path.Dir(file)), file)
			if err != nil {
				return err
			}
			if _, exists := skills[sk.Name]; exists {
				shadowed = append(shadowed, "skill:"+sk.Name)
			}
			skills[sk.Name] = sk
		}
		// workflows/*.star (catalogued; executed by package workflow)
		wfFiles, _ := fs.Glob(root, "workflows/*.star")
		sort.Strings(wfFiles)
		for _, file := range wfFiles {
			data, err := fs.ReadFile(root, file)
			if err != nil {
				return fmt.Errorf("agentdef: read workflow %s: %w", file, err)
			}
			fmt.Fprintf(h, "%s:%x\n", file, sha256.Sum256(data))
			name := strings.TrimSuffix(path.Base(file), ".star")
			if _, exists := wfs[name]; exists {
				shadowed = append(shadowed, "workflow:"+name)
			}
			wfs[name] = string(data)
			wfDirs[name] = root
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.defs, l.skills, l.workflowSrc, l.workflowDir = defs, skills, wfs, wfDirs
	l.digest = hex.EncodeToString(h.Sum(nil))
	l.Shadowed = shadowed
	l.loaded = true
	return nil
}

// execDefFile runs one definition file's top level under the shared
// Starlark dialect, collecting agent()/skill() declarations. The root
// serves load() so files can share fragments.
func execDefFile(root fs.FS, file string, data []byte, col *collector) error {
	ctx := context.Background()
	loader := modules.NewLoader(modules.Dir(root), modules.Options{
		Predeclared: col.predeclared(), MaxSteps: script.DefaultLimits.MaxSteps,
	})
	thread := script.NewThread(file, script.DefaultLimits, nil)
	loader.Bind(ctx, thread, file)
	_, err := script.Run(ctx, script.DefaultLimits, thread, func() (starlark.Value, error) {
		_, err := starlark.ExecFileOptions(dialect.FileOptions, thread, file, data, col.predeclared())
		return starlark.None, err
	})
	if err != nil {
		return fmt.Errorf("agentdef: %s: %w", file, err)
	}
	return nil
}

// ParseSource executes one in-memory definition source and returns its
// declarations without cataloguing them. Relative file reads resolve
// against root (may be nil when the source reads no files). It backs
// runtime registration: define validates here before anything is stored.
func ParseSource(root fs.FS, filename string, src []byte) (defs []*Definition, skills []*Skill, err error) {
	col := &collector{file: filename, dir: path.Dir(filename)}
	if root != nil {
		col.read = func(name string) ([]byte, error) { return fs.ReadFile(root, name) }
	} else {
		col.read = func(name string) ([]byte, error) { return nil, fs.ErrNotExist }
	}
	ef := root
	if ef == nil {
		ef = emptyFS{}
	}
	if err := execDefFile(ef, filename, src, col); err != nil {
		return nil, nil, err
	}
	for _, d := range col.defs {
		d.SourceText = append([]byte(nil), src...)
	}
	return col.defs, col.skill, nil
}

type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) { return nil, fs.ErrNotExist }

// AddDefinition catalogues one in-memory definition (runtime define).
// It shadows any same-named catalogued definition until the next Load.
func (l *Loader) AddDefinition(d *Definition) {
	l.ensureLoaded()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.defs == nil {
		l.defs = map[string]*Definition{}
	}
	l.defs[d.Name] = d
}

// AddWorkflowSource catalogues one in-memory workflow source.
func (l *Loader) AddWorkflowSource(name, src string, root fs.FS) {
	l.ensureLoaded()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.workflowSrc == nil {
		l.workflowSrc = map[string]string{}
		l.workflowDir = map[string]fs.FS{}
	}
	l.workflowSrc[name] = src
	l.workflowDir[name] = root
}

// ensureLoaded loads on first use so Roots(...).Resolve works without an
// explicit Load in small programs; Load errors surface at Resolve.
func (l *Loader) ensureLoaded() {
	l.mu.RLock()
	ok := l.loaded
	l.mu.RUnlock()
	if !ok {
		_ = l.Load()
	}
}

// ReloadIfChanged re-reads the roots when any catalogued file's content
// changed (or files appeared/vanished). It reports whether a reload
// happened. A failed reload keeps the previous catalogue.
func (l *Loader) ReloadIfChanged() bool {
	l.ensureLoaded()
	l.mu.RLock()
	prev := l.digest
	l.mu.RUnlock()
	// Cheap full re-hash: catalogues are small text files.
	fresh := &Loader{roots: l.roots}
	if err := fresh.Load(); err != nil {
		return false
	}
	if fresh.digest == prev {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.defs, l.skills, l.workflowSrc, l.workflowDir = fresh.defs, fresh.skills, fresh.workflowSrc, fresh.workflowDir
	l.digest = fresh.digest
	l.Shadowed = fresh.Shadowed
	return true
}

// Get returns the raw (unresolved) definition for name.
func (l *Loader) Get(name string) (*Definition, bool) {
	l.ensureLoaded()
	l.mu.RLock()
	defer l.mu.RUnlock()
	d, ok := l.defs[name]
	return d, ok
}

// Names returns definition names, sorted.
func (l *Loader) Names() []string {
	l.ensureLoaded()
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.defs))
	for n := range l.defs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Skill returns a loaded skill by name.
func (l *Loader) Skill(name string) (*Skill, bool) {
	l.ensureLoaded()
	l.mu.RLock()
	defer l.mu.RUnlock()
	s, ok := l.skills[name]
	return s, ok
}

// SkillBody returns a skill's body for the loop's skill built-in.
func (l *Loader) SkillBody(name string) (string, bool) {
	s, ok := l.Skill(name)
	if !ok {
		return "", false
	}
	return s.Body, true
}

// SkillNames returns skill names, sorted.
func (l *Loader) SkillNames() []string {
	l.ensureLoaded()
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.skills))
	for n := range l.skills {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Workflows returns catalogued workflow names, sorted.
func (l *Loader) Workflows() []string {
	l.ensureLoaded()
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.workflowSrc))
	for n := range l.workflowSrc {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// WorkflowSource returns a catalogued workflow's source.
func (l *Loader) WorkflowSource(name string) (string, bool) {
	l.ensureLoaded()
	l.mu.RLock()
	defer l.mu.RUnlock()
	s, ok := l.workflowSrc[name]
	return s, ok
}

// WorkflowFS returns the root filesystem a workflow's load()s resolve
// against (nil when the workflow was registered without one).
func (l *Loader) WorkflowFS(name string) fs.FS {
	l.ensureLoaded()
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.workflowDir[name]
}
