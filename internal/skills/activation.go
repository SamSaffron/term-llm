package skills

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ActivationErrorKind classifies direct activation failures for UI and tool
// adapters without forcing them to parse error strings.
type ActivationErrorKind int

const (
	ActivationNotFound ActivationErrorKind = iota
	ActivationInvalidMetadata
	ActivationDisabledForOrigin
	ActivationInvalidArguments
)

// ActivationError is returned by Activator when a skill cannot be activated.
type ActivationError struct {
	Kind   ActivationErrorKind
	Name   string
	Origin SkillActivationOrigin
	Err    error
}

func (e *ActivationError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return fmt.Sprintf("activate skill %q", e.Name)
	}
	return fmt.Sprintf("activate skill %q: %v", e.Name, e.Err)
}

func (e *ActivationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ActivationRequest describes a user- or model-origin skill activation.
type ActivationRequest struct {
	Name    string
	RawArgs string
	Origin  SkillActivationOrigin
}

// Activation is an immutable description of the resolved activation. Applying
// tools and permission filters is deliberately left to the engine adapter.
type Activation struct {
	Skill               *Skill
	Metadata            InvocationMetadata
	Prompt              string
	BaseDir             string
	Resources           []string
	AllowedTools        []string
	AllowedToolsPresent bool
	ToolDefs            []SkillToolDef
	RawArgs             string
	Origin              SkillActivationOrigin
}

// Activator resolves skills and constructs activations without mutating an
// engine. It is shared by model tools, slash UIs, and isolated child runners.
type Activator struct {
	registry *Registry
}

func NewActivator(registry *Registry) *Activator {
	return &Activator{registry: registry}
}

// Activate resolves, validates, authorizes, and expands a skill activation.
func (a *Activator) Activate(request ActivationRequest) (*Activation, error) {
	name := strings.TrimSpace(request.Name)
	if a == nil || a.registry == nil || name == "" {
		return nil, &ActivationError{
			Kind:   ActivationNotFound,
			Name:   name,
			Origin: request.Origin,
			Err:    fmt.Errorf("skill not found"),
		}
	}

	skill, err := a.registry.Get(name)
	if err != nil {
		return nil, &ActivationError{
			Kind:   ActivationNotFound,
			Name:   name,
			Origin: request.Origin,
			Err:    err,
		}
	}

	metadata, err := InvocationFor(skill)
	if err != nil {
		return nil, &ActivationError{
			Kind:   ActivationInvalidMetadata,
			Name:   name,
			Origin: request.Origin,
			Err:    err,
		}
	}

	switch request.Origin {
	case SkillActivationModel:
		if !a.registry.ModelInvocationEnabled() {
			return nil, originDisabledError(name, request.Origin, "model-driven skill activation is disabled")
		}
		if metadata.DisableModelInvocation {
			return nil, originDisabledError(name, request.Origin, "disable-model-invocation is true")
		}
	case SkillActivationUser:
		if !metadata.UserInvocable {
			return nil, originDisabledError(name, request.Origin, "user-invocable is false")
		}
	default:
		return nil, originDisabledError(name, request.Origin, "unknown activation origin")
	}

	prompt, err := ExpandInvocationArguments(skill.Body, request.RawArgs)
	if err != nil {
		return nil, &ActivationError{
			Kind:   ActivationInvalidArguments,
			Name:   name,
			Origin: request.Origin,
			Err:    err,
		}
	}

	toolDefs := append([]SkillToolDef(nil), skill.Tools...)
	for i := range toolDefs {
		toolDefs[i].UserInstalled = skill.UserInstalled
	}

	return &Activation{
		Skill:               skill,
		Metadata:            metadata,
		Prompt:              prompt,
		BaseDir:             skill.SourcePath,
		Resources:           activationResources(skill),
		AllowedTools:        append([]string(nil), skill.AllowedTools...),
		AllowedToolsPresent: skill.AllowedToolsPresent,
		ToolDefs:            toolDefs,
		RawArgs:             request.RawArgs,
		Origin:              request.Origin,
	}, nil
}

func originDisabledError(name string, origin SkillActivationOrigin, reason string) *ActivationError {
	return &ActivationError{
		Kind:   ActivationDisabledForOrigin,
		Name:   name,
		Origin: origin,
		Err:    fmt.Errorf("%s activation is not permitted: %s", origin, reason),
	}
}

// RenderActivationInstructions formats the expanded body with the same source,
// description, and resource context used by model-driven activation.
func RenderActivationInstructions(activation *Activation) string {
	if activation == nil || activation.Skill == nil {
		return ""
	}
	skill := *activation.Skill
	skill.Body = activation.Prompt
	return GenerateActivationResponse(&skill, "")
}

func activationResources(skill *Skill) []string {
	if skill == nil {
		return nil
	}
	resources := make([]string, 0, len(skill.References)+len(skill.Scripts)+len(skill.Assets))
	for _, path := range skill.References {
		resources = append(resources, filepath.ToSlash(filepath.Join("references", path)))
	}
	for _, path := range skill.Scripts {
		resources = append(resources, filepath.ToSlash(filepath.Join("scripts", path)))
	}
	for _, path := range skill.Assets {
		resources = append(resources, filepath.ToSlash(filepath.Join("assets", path)))
	}
	return resources
}
