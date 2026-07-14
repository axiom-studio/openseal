package skill

import (
	"context"
	"errors"
)

var (
	ErrDefinitionImmutable     = errors.New("skill definition versions are immutable")
	ErrDefinitionAmbiguous     = errors.New("skill definition source is ambiguous")
	ErrBindingAmbiguous        = errors.New("skill binding selection is ambiguous")
	ErrBindingRevisionConflict = errors.New("skill binding revision conflict")
)

// CatalogStore persists the canonical skill control plane. Implementations
// store opaque credential references only; secret material never belongs here.
type CatalogStore interface {
	CreateSkillDefinition(context.Context, *Definition) error
	ListSkillDefinitionVariants(context.Context, string, string) ([]*Definition, error)
	SaveSkillBinding(context.Context, *Binding, int64) error
	ListSkillBindings(context.Context, ScopeReference, string) ([]*Binding, error)
}
