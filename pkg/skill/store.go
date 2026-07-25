package skill

import (
	"context"
	"errors"
)

var (
	ErrDefinitionImmutable     = errors.New("skill definition versions are immutable")
	ErrDefinitionAmbiguous     = errors.New("skill definition source is ambiguous")
	ErrBindingAmbiguous        = errors.New("skill binding selection is ambiguous")
	ErrBindingInvalid          = errors.New("skill binding is invalid")
	ErrBindingRevisionConflict = errors.New("skill binding revision conflict")
	ErrBindingNotFound         = errors.New("skill binding not found")
	ErrBindingAlreadyDisabled  = errors.New("skill binding is already disabled")
)

// CatalogStore persists the canonical skill control plane. Implementations
// store opaque credential references only; secret material never belongs here.
type CatalogStore interface {
	CreateSkillDefinition(context.Context, *Definition) error
	ListSkillDefinitionVariants(context.Context, string, string) ([]*Definition, error)
	SaveSkillBinding(context.Context, *Binding, int64) error
	ListSkillBindings(context.Context, ScopeReference, string) ([]*Binding, error)
}
