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
	// SaveSkillBinding writes one binding at expectedRevision. When no row
	// exists the write is a creation: expectedRevision must equal the ID's
	// revision floor (zero for an ID never deleted) and the binding's revision
	// must be the floor plus one.
	SaveSkillBinding(context.Context, *Binding, int64) error
	ListSkillBindings(context.Context, ScopeReference, string) ([]*Binding, error)
	// DeleteSkillBinding removes one exact binding row at its expected
	// revision and raises the ID's revision floor to expectedRevision+1, the
	// revision of the deletion itself. It returns ErrBindingNotFound when no
	// binding has that ID and ErrBindingRevisionConflict when the current
	// revision differs.
	DeleteSkillBinding(ctx context.Context, scope ScopeReference, deploymentID, bindingID string, expectedRevision int64) error
	// SkillBindingRevisionFloor returns the revision of the last deletion of
	// this binding ID, or zero. A binding created under the ID starts above
	// it, so a reference to a deleted binding never matches its successor.
	SkillBindingRevisionFloor(ctx context.Context, scope ScopeReference, deploymentID, bindingID string) (int64, error)
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

// BindingRevision is the payload-free identity of one stored binding row.
type BindingRevision struct {
	ID             string `json:"id"`
	Revision       int64  `json:"revision"`
	SkillID        string `json:"skillId"`
	SkillVersion   string `json:"skillVersion"`
	SourceIdentity string `json:"sourceIdentity,omitempty"`
}

// CatalogBindingRevisionStore optionally lists one deployment's binding
// revisions without hydrating binding payloads. Activation uses it to reuse a
// cached snapshot without decoding every binding on every turn. It must list
// exactly the rows ListSkillBindings would return.
type CatalogBindingRevisionStore interface {
	ListSkillBindingRevisions(context.Context, ScopeReference, string) ([]BindingRevision, error)
}
