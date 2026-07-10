package skill

import (
	"context"
	"errors"
)

var (
	ErrDefinitionImmutable     = errors.New("skill definition versions are immutable")
	ErrBindingRevisionConflict = errors.New("skill binding revision conflict")
)

// CatalogStore persists the canonical skill control plane. Implementations
// store opaque credential references only; secret material never belongs here.
type CatalogStore interface {
	CreateSkillDefinition(context.Context, *Definition) error
	GetSkillDefinition(context.Context, string, string) (*Definition, error)
	SaveSkillBinding(context.Context, *Binding, int64) error
	ListSkillBindings(context.Context, ScopeReference, string) ([]*Binding, error)
}
