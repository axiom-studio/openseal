package skill

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Activation cache bounds. Snapshots are cached per deployment and host
// state (including the run's host binding set); validated binding revisions
// are cached per exact binding revision and definition variant.
const (
	activationCacheDeployments    = 512
	activationCacheHostsPerTarget = 16
	validatedBindingCacheEntries  = 16384
)

// lruCache is a bounded least-recently-used map. It is safe for concurrent use.
type lruCache[V any] struct {
	mu       sync.Mutex
	capacity int
	order    *list.List
	entries  map[string]*list.Element
}

type lruEntry[V any] struct {
	key   string
	value V
}

func newLRUCache[V any](capacity int) *lruCache[V] {
	return &lruCache[V]{capacity: capacity, order: list.New(), entries: make(map[string]*list.Element)}
}

func (c *lruCache[V]) get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.order.MoveToFront(element)
	return element.Value.(*lruEntry[V]).value, true
}

func (c *lruCache[V]) put(key string, value V) {
	c.update(key, func(V, bool) V { return value })
}

// update atomically replaces the value stored under key with change(current).
func (c *lruCache[V]) update(key string, change func(V, bool) V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[key]; ok {
		entry := element.Value.(*lruEntry[V])
		entry.value = change(entry.value, true)
		c.order.MoveToFront(element)
		return
	}
	var zero V
	c.entries[key] = c.order.PushFront(&lruEntry[V]{key: key, value: change(zero, false)})
	for c.order.Len() > c.capacity {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*lruEntry[V]).key)
	}
}

func (c *lruCache[V]) remove(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[key]; ok {
		c.order.Remove(element)
		delete(c.entries, key)
	}
}

func (c *lruCache[V]) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// cachedActivation is one immutable snapshot and the exact inputs it was
// computed from.
type cachedActivation struct {
	fingerprint string
	snapshot    *ActivationSnapshot
}

func activationTargetKey(scope ScopeReference, deploymentID string) string {
	return scope.Kind + "\x00" + scope.ID + "\x00" + deploymentID
}

// hostActivationDigest identifies the declared, non-secret host state.
// Adapter presence is included because it changes availability even when no
// adapter is invoked.
func hostActivationDigest(host HostCapabilityState) string {
	encoded, _ := json.Marshal(struct {
		State    HostCapabilityState `json:"state"`
		Stager   bool                `json:"stager"`
		Preparer bool                `json:"preparer"`
	}{host, host.ResourceStager != nil, host.RuntimePreparer != nil})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// activationFingerprint identifies every input of an activation other than
// the host: the exact revision of every binding row, including disabled
// rows, and the definition variant each reference resolves to. Binding
// revisions are compare-and-swap counters, so an unchanged revision is an
// unchanged binding. definitions holds variants already resolved by the
// caller; other references are resolved here, and a reference that cannot be
// resolved contributes its failure.
func (c *Catalog) activationFingerprint(ctx context.Context, revisions []BindingRevision, definitions map[string]*Definition) string {
	sort.Slice(revisions, func(i, j int) bool { return revisions[i].ID < revisions[j].ID })
	resolved := make(map[string]string)
	hash := sha256.New()
	for _, revision := range revisions {
		key := definitionKey(revision.SkillID, revision.SkillVersion, revision.SourceIdentity)
		identity, ok := resolved[key]
		if !ok {
			definition, err := definitions[key], error(nil)
			if definition == nil {
				definition, err = c.definitionFor(ctx, revision.SkillID, revision.SkillVersion, revision.SourceIdentity)
			}
			switch {
			case err != nil:
				identity = "!unresolved"
			case definition == nil:
				identity = "!missing"
			default:
				identity = definitionKey(definition.ID, definition.Version, DefinitionSourceIdentity(definition)) + "\x00" + definitionSourceDigest(definition)
			}
			resolved[key] = identity
		}
		for _, part := range []string{revision.ID, strconv.FormatInt(revision.Revision, 10), key, identity} {
			hash.Write([]byte(part))
			hash.Write([]byte{0})
		}
		hash.Write([]byte{1})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func bindingRevisions(bindings []*Binding) []BindingRevision {
	revisions := make([]BindingRevision, 0, len(bindings))
	for _, binding := range bindings {
		revisions = append(revisions, BindingRevision{ID: binding.ID, Revision: binding.Revision, SkillID: binding.SkillID, SkillVersion: binding.SkillVersion, SourceIdentity: binding.SourceIdentity})
	}
	return revisions
}

// warmActivation returns a cached snapshot when the store can prove, from
// payload-free binding revisions, that its inputs are unchanged.
func (c *Catalog) warmActivation(ctx context.Context, scope ScopeReference, deploymentID, host string) (*ActivationSnapshot, error) {
	store, ok := c.store.(CatalogBindingRevisionStore)
	if !ok || c.activations == nil {
		return nil, nil
	}
	if _, cached := c.activations.get(activationTargetKey(scope, deploymentID)); !cached {
		return nil, nil
	}
	revisions, err := store.ListSkillBindingRevisions(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	return c.cachedActivation(scope, deploymentID, host, c.activationFingerprint(ctx, revisions, nil)), nil
}

func (c *Catalog) cachedActivation(scope ScopeReference, deploymentID, host, fingerprint string) *ActivationSnapshot {
	if c.activations == nil {
		return nil
	}
	entries, ok := c.activations.get(activationTargetKey(scope, deploymentID))
	if !ok {
		return nil
	}
	entry, ok := entries[host]
	if !ok || entry.fingerprint != fingerprint {
		return nil
	}
	return entry.snapshot
}

func (c *Catalog) storeActivation(scope ScopeReference, deploymentID, host, fingerprint string, snapshot *ActivationSnapshot) {
	if c.activations == nil {
		return
	}
	c.activations.update(activationTargetKey(scope, deploymentID), func(current map[string]cachedActivation, _ bool) map[string]cachedActivation {
		next := make(map[string]cachedActivation, len(current)+1)
		for key, value := range current {
			if len(next) < activationCacheHostsPerTarget-1 {
				next[key] = value
			}
		}
		next[host] = cachedActivation{fingerprint: fingerprint, snapshot: snapshot}
		return next
	})
}

// invalidateActivations drops every cached snapshot of one deployment. It is
// called after each binding write through this catalog; writes by other
// processes change a binding revision and so miss the cache by fingerprint.
func (c *Catalog) invalidateActivations(scope ScopeReference, deploymentID string) {
	if c.activations != nil {
		c.activations.remove(activationTargetKey(scope, deploymentID))
	}
}

// shareActivation returns a caller-owned top level of a cached snapshot.
// Nested schemas, configuration and actions are shared and read-only.
func shareActivation(snapshot *ActivationSnapshot, createdAt time.Time) *ActivationSnapshot {
	copy := *snapshot
	copy.Skills = append([]ActivatedSkill(nil), snapshot.Skills...)
	if snapshot.Unavailable != nil {
		copy.Unavailable = append([]UnavailableSkill(nil), snapshot.Unavailable...)
	}
	copy.CreatedAt = createdAt
	return &copy
}

// validatedBindingKey identifies one exact stored binding revision and the
// definition reference it was validated against. Definition variants are
// immutable, and an unqualified reference that later becomes ambiguous still
// fails definition resolution, which runs on every listing.
func validatedBindingKey(binding *Binding) string {
	return bindingKey(binding.Scope, binding.DeploymentID, binding.ID) + "@" + strconv.FormatInt(binding.Revision, 10) +
		"\x00" + definitionKey(binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
}
