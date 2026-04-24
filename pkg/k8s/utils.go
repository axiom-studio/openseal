package k8s

import "k8s.io/client-go/rest"

type K8sServiceImpl struct {
	baseConfig *rest.Config
}

func NewK8sServiceImpl() *K8sServiceImpl {
	return &K8sServiceImpl{}
}

func (s *K8sServiceImpl) GetRestConfigByCluster(config interface{}) (*rest.Config, error) {
	if rc, ok := config.(*rest.Config); ok {
		return rc, nil
	}
	if s.baseConfig != nil {
		return s.baseConfig, nil
	}
	return rest.InClusterConfig()
}
