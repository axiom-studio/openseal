package skill

import "strings"

// UmbrellaKind names what holds a run's connections.
type UmbrellaKind string

const (
	UmbrellaAgent UmbrellaKind = "agent"
	UmbrellaTeam  UmbrellaKind = "team"
)

// Umbrella is the Agent or Team deployment whose connections a run uses: the
// Team in a Team chat, task or hand-off, and the Agent itself in a 1:1 chat.
// It mirrors runtime.ObjectiveOwner, which this package cannot import.
type Umbrella struct {
	Kind UmbrellaKind `json:"kind"`
	ID   string       `json:"id"`
}

// Valid reports whether u names an Agent or Team deployment.
func (u Umbrella) Valid() bool {
	return (u.Kind == UmbrellaAgent || u.Kind == UmbrellaTeam) && u.ID != "" && strings.TrimSpace(u.ID) == u.ID
}
