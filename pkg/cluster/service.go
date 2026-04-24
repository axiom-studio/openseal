package cluster

import "time"

type ClusterConfig struct {
	ID        int
	Name      string
	Server    string
	Token     string
	CaData    string
}

type Cluster struct {
	Id           int
	Name         string
	ClusterConfig *ClusterConfig
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (c *Cluster) GetClusterConfig() *ClusterConfig {
	return c.ClusterConfig
}

type ClusterReadService interface {
	FindById(id int) (*Cluster, error)
	ListClusters() ([]*Cluster, error)
}
