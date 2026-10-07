package skill

import (
	"context"
	"errors"

	"github.com/axiom-studio/openseal/pkg/capability"
)

var (
	ErrDefinitionImmutable     = errors.New("skill definition versions are immutable")
	ErrDefinitionAmbiguous     = errors.New("skill definition source is ambiguous")
	ErrBindingAmbiguous        = errors.New("skill binding selection is ambiguous")
	ErrBindingInvalid          = errors.New("skill binding is invalid")
	ErrBindingRevisionConflict = errors.New("skill binding revision conflict")
	ErrBindingNotFound         = errors.New("skill binding not found")
	ErrBindingUnavailable      = errors.New("selected skill binding is unavailable or stale")
	ErrBindingAlreadyDisabled  = errors.New("skill binding is already disabled")
)

// CatalogStore persists the canonical skill control plane. Implementations
// store opaque credential references only; secret material never belongs here.
type CatalogStore interface {
	CreateSkillDefinition(context.Context, *Definition) error
	ListSkillDefinitionVariants(context.Context, string, string) ([]*Definition, error)
	SaveSkillBinding(context.Context, *Binding, int64) error
	ListSkillBindings(context.Context, ScopeReference, string) ([]*Binding, error)
	// DeleteSkillBinding removes one exact binding row at its expected
	// revision. It returns ErrBindingNotFound when no binding has that ID and
	// ErrBindingRevisionConflict when the current revision differs.
	DeleteSkillBinding(ctx context.Context, scope ScopeReference, deploymentID, bindingID string, expectedRevision int64) error
}

// CatalogDefinitionIdentityStore optionally projects the current immutable
// definition keys without hydrating their payloads. It must return every exact
// ID/version/source variant for the requested ID and version on each call;
// source-less builtins have an empty SourceIdentity. Definitions remain global
// capabilities: this projection does not replace scoped binding authorization.
// Stores that omit this interface retain full definition loading and validation.
type CatalogDefinitionIdentityStore interface {
	ListSkillDefinitionIdentities(context.Context, string, string) ([]capability.SkillIdentity, error)
}
