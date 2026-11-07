package agentdef

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/craggy-io/foundation-kit/bundle"
	"github.com/craggy-io/foundation-kit/kind"
)

// InstallAgent writes one user bundle under definitions/agentsdk.user.definitions.<name>.
// Live.Edit validates and atomically publishes the change, rolling it back on failure.
// Reinstalling replaces that bundle's agent, retaining its independent identity.
func (l *Loader) InstallAgent(ctx context.Context, name string, src []byte) (string, error) {
	return l.install(ctx, AgentKind.ID(), "definitions", "agent.star", name, src)
}

// InstallWorkflow is InstallAgent for a workflow, written to
// workflows/agentsdk.user.workflows.<name>/workflow.star.
func (l *Loader) InstallWorkflow(ctx context.Context, name string, src []byte) (string, error) {
	return l.install(ctx, WorkflowKind.ID(), "workflows", "workflow.star", name, src)
}

func (l *Loader) install(ctx context.Context, k kind.ID, group, path, name string, src []byte) (string, error) {
	if l.opts.UserDir == "" {
		return "", errors.New("agentdef: no user layer to install into (Options.UserDir is empty)")
	}
	id := "agentsdk.user." + group + "." + name
	root := filepath.Join(l.opts.UserDir, group)
	put := []bundle.Put{{ID: name, Kind: k, Path: path, Data: src}}
	if _, err := l.live.Edit(ctx, root, id, put, nil); err != nil {
		return "", err
	}
	return filepath.Join(root, id, path), nil
}
