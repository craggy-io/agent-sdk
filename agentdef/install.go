package agentdef

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/craggy-io/foundation-kit/bundle"
	"github.com/craggy-io/foundation-kit/kind"
)

// InstallAgent adds or replaces agent name in the user layer, as
// agents/<name>.star in the bundle at <UserDir>/agentsdk.user, with
// bundle.Live.Edit: the bundle.yaml entry is written for it, every layer
// is reloaded, and the install is rolled back if that load fails, for
// example because src does not declare agent name. It returns the path of
// the written file.
func (l *Loader) InstallAgent(ctx context.Context, name string, src []byte) (string, error) {
	return l.install(ctx, AgentKind.ID(), "agents/"+name+".star", name, src)
}

// InstallWorkflow is InstallAgent for a workflow, written to
// workflows/<name>.star.
func (l *Loader) InstallWorkflow(ctx context.Context, name string, src []byte) (string, error) {
	return l.install(ctx, WorkflowKind.ID(), "workflows/"+name+".star", name, src)
}

func (l *Loader) install(ctx context.Context, k kind.ID, path, name string, src []byte) (string, error) {
	if l.opts.UserDir == "" {
		return "", errors.New("agentdef: no user layer to install into (Options.UserDir is empty)")
	}
	put := []bundle.Put{{ID: name, Kind: k, Path: path, Data: src}}
	if _, err := l.live.Edit(ctx, l.opts.UserDir, UserID, put, nil); err != nil {
		return "", err
	}
	return filepath.Join(l.userBundle(), filepath.FromSlash(path)), nil
}
