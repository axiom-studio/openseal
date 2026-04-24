
package environment

import (
	"time"
)

type Environment struct {
	ID        int
	Name      string
	ClusterId int
	CreatedAt time.Time
	UpdatedAt time.Time
}

type EnvironmentRepository interface {
	FindById(id int) (*Environment, error)
	ListEnvironments() ([]*Environment, error)
	Save(env *Environment) (*Environment, error)
}
