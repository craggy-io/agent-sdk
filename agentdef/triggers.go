package agentdef

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/craggy-io/foundation-kit/bundle"
	"github.com/craggy-io/foundation-kit/bundle/compile"
	"github.com/craggy-io/foundation-kit/kind"
	"github.com/craggy-io/foundation-kit/trigger"
	"github.com/craggy-io/script-services/schema"
	"github.com/craggy-io/script-services/starcompile"
	"github.com/craggy-io/script-services/triggercompile"
	"go.starlark.net/starlark"
)

// WorkflowBinding attaches an independently installed trigger to a workflow.
// The host supplies identity and scope; neither comes from authored content.
type WorkflowBinding struct {
	Workflow string       `json:"workflow" yaml:"workflow"`
	Trigger  trigger.Spec `json:"trigger" yaml:"trigger"`
}

// WorkflowBindingKind is an independent workflow event binding.
var WorkflowBindingKind = kind.New[WorkflowBinding]("agentsdk.workflow-binding", 1, kind.Global())

func bindingCompiler() bundle.AnyCompiler {
	return bundle.Compile(WorkflowBindingKind, func(ctx context.Context, c bundle.Contribution) (bundle.Compiled[WorkflowBinding], []bundle.Diagnostic) {
		out, diags := compile.JSON[WorkflowBinding]()(ctx, c)
		if len(diags) > 0 {
			return out, diags
		}
		if err := out.Value.Trigger.Validate(); err != nil {
			return out, []bundle.Diagnostic{{Code: "agentsdk.workflow-binding", Severity: bundle.SeverityError, Bundle: c.Bundle.ID, Contribution: c.Ref.ID, Message: err.Error()}}
		}
		return out, nil
	})
}

// ValidateBindings checks independent binding targets and mapped input names
// against the fully linked catalog before it is activated.
func ValidateBindings(cat *bundle.Catalog) []bundle.Diagnostic {
	var diagnostics []bundle.Diagnostic
	for _, def := range bundle.All(cat, WorkflowBindingKind) {
		workflow, ok := bundle.Lookup(cat, WorkflowKind, def.Value.Workflow)
		var err error
		if !ok {
			err = fmt.Errorf("workflow %q is not installed", def.Value.Workflow)
		} else {
			err = ValidateMapping(def.Value.Trigger, workflow.Value.InputSchema)
		}
		if err != nil {
			diagnostics = append(diagnostics, bundle.Diagnostic{Code: "agentsdk.workflow-binding", Severity: bundle.SeverityError, Bundle: def.Owner.ID, Contribution: def.Ref.ID, Message: err.Error()})
		}
	}
	return diagnostics
}

// workflowDeclaration evaluates only declarations. Calling runtime operations
// at file scope is rejected; run(input) is never called by the compiler.
func workflowDeclaration(ctx context.Context, u *starcompile.Unit) (*Workflow, error) {
	pre := triggercompile.Builtins()
	for _, name := range workflowBuiltins {
		if _, ok := pre[name]; ok {
			continue
		}
		pre[name] = starlark.NewBuiltin(name, func(_ *starlark.Thread, b *starlark.Builtin, _ starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
			return nil, fmt.Errorf("%s is only available inside run(input)", b.Name())
		})
	}
	pre["schema"] = schema.Module()
	pre["role"] = RoleBuiltin()
	w := &Workflow{Name: u.Contribution().Ref.ID, Description: u.Contribution().Metadata.Description, Unit: u}
	pre["workflow"] = starlark.NewBuiltin("workflow", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var desc string
		var input, agents, profiles, limits, triggers, roles starlark.Value
		if err := starlark.UnpackArgs("workflow", args, kwargs, "description?", &desc, "input?", &input, "agents?", &agents, "profiles?", &profiles, "limits?", &limits, "triggers?", &triggers, "chat?", &w.Chat, "roles?", &roles); err != nil {
			return nil, err
		}
		var err error
		if w.Roles, err = ParseRoles(roles, w.Chat); err != nil {
			return nil, err
		}
		w.Triggers, err = triggercompile.List(triggers)
		if err != nil {
			return nil, err
		}
		if input != nil {
			s, err := schema.Of(input)
			if err != nil {
				return nil, err
			}
			w.InputSchema, err = json.Marshal(s)
			if err != nil {
				return nil, err
			}
		}
		return starlark.None, nil
	})
	_, err := u.Exec(ctx, starcompile.ExecOptions{Predeclared: pre, ModulePredeclared: pre})
	return w, err
}

// ValidateMapping checks required and unknown workflow input names before
// activation. Runtime schema validation still checks actual mapped values.
func ValidateMapping(spec trigger.Spec, raw json.RawMessage) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
		Additional *bool                      `json:"additionalProperties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return err
	}
	for _, name := range schema.Required {
		if _, ok := spec.Input[name]; !ok {
			return fmt.Errorf("trigger omits required workflow input %q", name)
		}
	}
	if schema.Additional != nil && !*schema.Additional {
		for name := range spec.Input {
			if _, ok := schema.Properties[name]; !ok {
				return fmt.Errorf("trigger maps unknown workflow input %q", name)
			}
		}
	}
	return nil
}
