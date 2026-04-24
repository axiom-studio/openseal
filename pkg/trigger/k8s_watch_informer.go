package trigger

import (
	"context"
	"fmt"
	"sync"
	"time"

	k8sUtils "github.com/axiom-studio/openseal/pkg/k8s"
	"github.com/axiom-studio/openseal/pkg/repository"
	envRepository "github.com/axiom-studio/openseal/pkg/environment"
	clusterRead "github.com/axiom-studio/openseal/pkg/cluster"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

type K8sWatchTrigger struct {
	mu                 sync.RWMutex
	instances          map[int]*k8sWatchInstance
	informers          map[int]cache.SharedIndexInformer
	stopChans          map[int]chan struct{}
	handler            TriggerHandler
	started            bool
	clients            map[int]dynamic.Interface
	clusterReadService clusterRead.ClusterReadService
	k8sUtil            *k8sUtils.K8sServiceImpl
	instanceRepo       repository.AgentInstanceRepository
	environmentRepo    envRepository.EnvironmentRepository
	logger             *zap.SugaredLogger
}

type k8sWatchInstance struct {
	instanceId    int
	resource      string
	namespace     string
	labelSelector string
	fieldSelector string
	events        []string
	debounceMs    int
	config        map[string]interface{}
	lastFired     map[string]time.Time
	mu            sync.Mutex
}

func NewK8sWatchTrigger(
	clusterReadService clusterRead.ClusterReadService,
	k8sUtil *k8sUtils.K8sServiceImpl,
	instanceRepo repository.AgentInstanceRepository,
	environmentRepo envRepository.EnvironmentRepository,
	logger *zap.SugaredLogger,
) *K8sWatchTrigger {
	return &K8sWatchTrigger{
		instances:          make(map[int]*k8sWatchInstance),
		informers:          make(map[int]cache.SharedIndexInformer),
		stopChans:          make(map[int]chan struct{}),
		clients:            make(map[int]dynamic.Interface),
		clusterReadService: clusterReadService,
		k8sUtil:            k8sUtil,
		instanceRepo:       instanceRepo,
		environmentRepo:    environmentRepo,
		logger:             logger,
	}
}

func (t *K8sWatchTrigger) Type() string {
	return "k8s-watch"
}

func (t *K8sWatchTrigger) ValidateConfig(config map[string]interface{}) error {
	resource, ok := config["resource"].(string)
	if !ok || resource == "" {
		return fmt.Errorf("k8s-watch trigger requires 'resource' (e.g., 'pods', 'deployments')")
	}

	if events, ok := config["events"].([]interface{}); ok {
		for _, e := range events {
			eventStr, ok := e.(string)
			if !ok {
				return fmt.Errorf("events must be array of strings")
			}
			if eventStr != "ADDED" && eventStr != "MODIFIED" && eventStr != "DELETED" {
				return fmt.Errorf("invalid event type: %s (must be ADDED, MODIFIED, or DELETED)", eventStr)
			}
		}
	}

	return nil
}

func (t *K8sWatchTrigger) Setup(ctx context.Context, instanceId int, config map[string]interface{}) (*TriggerMetadata, error) {
	if err := t.ValidateConfig(config); err != nil {
		return nil, err
	}

	resource := config["resource"].(string)
	namespace := ""
	if ns, ok := config["namespace"].(string); ok {
		namespace = ns
	}

	labelSelector := ""
	if ls, ok := config["labelSelector"].(string); ok {
		labelSelector = ls
	}

	fieldSelector := ""
	if fs, ok := config["fieldSelector"].(string); ok {
		fieldSelector = fs
	}

	debounceMs := 1000
	if d, ok := config["debounceMs"].(float64); ok && d > 0 {
		debounceMs = int(d)
	}

	var events []string
	if e, ok := config["events"].([]interface{}); ok {
		for _, ev := range e {
			if evStr, ok := ev.(string); ok {
				events = append(events, evStr)
			}
		}
	} else {
		events = []string{"ADDED", "MODIFIED", "DELETED"}
	}

	instance := &k8sWatchInstance{
		instanceId:    instanceId,
		resource:      resource,
		namespace:     namespace,
		labelSelector: labelSelector,
		fieldSelector: fieldSelector,
		events:        events,
		debounceMs:    debounceMs,
		config:        config,
		lastFired:     make(map[string]time.Time),
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if existing := t.instances[instanceId]; existing != nil {
		t.stopInformer(instanceId)
	}

	t.instances[instanceId] = instance

	if t.started && t.handler != nil {
		if err := t.startInformer(ctx, instance); err != nil {
			return nil, fmt.Errorf("failed to start informer: %w", err)
		}
	}

	return &TriggerMetadata{}, nil
}

func (t *K8sWatchTrigger) Teardown(ctx context.Context, instanceId int) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.stopInformer(instanceId)
	delete(t.instances, instanceId)

	return nil
}

func (t *K8sWatchTrigger) Start(ctx context.Context, handler TriggerHandler) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.logger.Infow("🚀 Starting K8s Watch Trigger", "instanceCount", len(t.instances))

	t.handler = handler
	t.started = true

	if len(t.instances) == 0 {
		t.logger.Warn("⚠️  No K8s watch trigger instances configured")
		return nil
	}

	for _, instance := range t.instances {
		if err := t.ensureClientForInstance(ctx, instance.instanceId); err != nil {
			t.logger.Errorw("❌ Failed to create client for instance", "instanceId", instance.instanceId, "error", err)
			return fmt.Errorf("failed to create client for instance %d: %w", instance.instanceId, err)
		}

		t.logger.Infow("📡 Starting watch informer for instance",
			"instanceId", instance.instanceId,
			"resource", instance.resource,
			"namespace", instance.namespace,
			"events", instance.events,
			"labelSelector", instance.labelSelector,
		)
		if err := t.startInformer(ctx, instance); err != nil {
			t.logger.Errorw("❌ Failed to start watch informer", "instanceId", instance.instanceId, "error", err)
			return fmt.Errorf("failed to start informer for instance %d: %w", instance.instanceId, err)
		}
		t.logger.Infow("✅ Watch informer started successfully", "instanceId", instance.instanceId)
	}

	t.logger.Info("🎯 K8s Watch Trigger fully started and watching for resource changes")
	return nil
}

func (t *K8sWatchTrigger) ensureClientForInstance(ctx context.Context, instanceId int) error {
	if _, exists := t.clients[instanceId]; exists {
		return nil
	}

	agentInstance, err := t.instanceRepo.FindById(instanceId)
	if err != nil {
		return fmt.Errorf("failed to get agent instance: %w", err)
	}

	environment, err := t.environmentRepo.FindById(agentInstance.EnvironmentId)
	if err != nil {
		return fmt.Errorf("failed to get environment %d: %w", agentInstance.EnvironmentId, err)
	}

	cluster, err := t.clusterReadService.FindById(environment.ClusterId)
	if err != nil {
		return fmt.Errorf("failed to get cluster %d: %w", environment.ClusterId, err)
	}

	clusterConfig := cluster.GetClusterConfig()
	restConfig, err := t.k8sUtil.GetRestConfigByCluster(clusterConfig)
	if err != nil {
		return fmt.Errorf("failed to get rest config for cluster %d: %w", cluster.Id, err)
	}

	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create dynamic client: %w", err)
	}

	t.clients[instanceId] = dynClient
	t.logger.Infow("✅ Created K8s client for instance", "instanceId", instanceId, "clusterId", cluster.Id)
	return nil
}

func (t *K8sWatchTrigger) Stop(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	for instanceId := range t.instances {
		t.stopInformer(instanceId)
	}

	t.started = false
	t.handler = nil

	return nil
}

func (t *K8sWatchTrigger) startInformer(ctx context.Context, instance *k8sWatchInstance) error {
	gvr, err := resourceToGVR(instance.resource)
	if err != nil {
		return err
	}

	namespace := instance.namespace
	if namespace == "" {
		namespace = metav1.NamespaceAll
	}

	client, ok := t.clients[instance.instanceId]
	if !ok {
		return fmt.Errorf("no k8s client found for instance %d", instance.instanceId)
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		client,
		time.Minute*10,
		namespace,
		func(options *metav1.ListOptions) {
			if instance.labelSelector != "" {
				options.LabelSelector = instance.labelSelector
			}
			if instance.fieldSelector != "" {
				options.FieldSelector = instance.fieldSelector
			}
		},
	)

	informer := factory.ForResource(gvr).Informer()

	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u, _ := obj.(*unstructured.Unstructured)
			t.logger.Debugw("📨 Resource ADDED detected",
				"instanceId", instance.instanceId,
				"resource", instance.resource,
				"name", u.GetName(),
				"namespace", u.GetNamespace(),
			)
			if t.shouldFireEvent(instance, "ADDED") {
				t.logger.Infow("🔥 ADDED event matches filter - firing trigger!",
					"instanceId", instance.instanceId,
					"resource", instance.resource,
					"name", u.GetName(),
				)
				t.fireEvent(instance, "ADDED", obj, nil)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			u, _ := newObj.(*unstructured.Unstructured)
			t.logger.Debugw("📨 Resource MODIFIED detected",
				"instanceId", instance.instanceId,
				"resource", instance.resource,
				"name", u.GetName(),
				"namespace", u.GetNamespace(),
			)
			if t.shouldFireEvent(instance, "MODIFIED") {
				t.logger.Infow("🔥 MODIFIED event matches filter - firing trigger!",
					"instanceId", instance.instanceId,
					"resource", instance.resource,
					"name", u.GetName(),
				)
				t.fireEvent(instance, "MODIFIED", newObj, oldObj)
			}
		},
		DeleteFunc: func(obj interface{}) {
			u, _ := obj.(*unstructured.Unstructured)
			t.logger.Debugw("📨 Resource DELETED detected",
				"instanceId", instance.instanceId,
				"resource", instance.resource,
				"name", u.GetName(),
				"namespace", u.GetNamespace(),
			)
			if t.shouldFireEvent(instance, "DELETED") {
				t.logger.Infow("🔥 DELETED event matches filter - firing trigger!",
					"instanceId", instance.instanceId,
					"resource", instance.resource,
					"name", u.GetName(),
				)
				t.fireEvent(instance, "DELETED", obj, nil)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	stopCh := make(chan struct{})
	t.informers[instance.instanceId] = informer
	t.stopChans[instance.instanceId] = stopCh

	go informer.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, informer.HasSynced) {
		close(stopCh)
		return fmt.Errorf("failed to sync cache")
	}

	return nil
}

func (t *K8sWatchTrigger) stopInformer(instanceId int) {
	if stopCh, ok := t.stopChans[instanceId]; ok {
		close(stopCh)
		delete(t.stopChans, instanceId)
	}
	delete(t.informers, instanceId)
}

func (t *K8sWatchTrigger) shouldFireEvent(instance *k8sWatchInstance, eventType string) bool {
	for _, e := range instance.events {
		if e == eventType {
			return true
		}
	}
	return false
}

func (t *K8sWatchTrigger) fireEvent(instance *k8sWatchInstance, eventType string, obj, oldObj interface{}) {
	objMeta := extractMetadata(obj)

	if instance.debounceMs > 0 {
		key := fmt.Sprintf("%s/%s/%s", eventType, objMeta["namespace"], objMeta["name"])

		instance.mu.Lock()
		lastFired, exists := instance.lastFired[key]
		now := time.Now()
		if exists && now.Sub(lastFired) < time.Duration(instance.debounceMs)*time.Millisecond {
			t.logger.Debugw("⏭️  Event debounced (too soon after last fire)",
				"instanceId", instance.instanceId,
				"key", key,
				"timeSinceLastFire", now.Sub(lastFired),
			)
			instance.mu.Unlock()
			return
		}
		instance.lastFired[key] = now
		instance.mu.Unlock()
	}

	if t.handler == nil {
		t.logger.Warn("⚠️  No handler registered, cannot fire event")
		return
	}

	payload := map[string]interface{}{
		"eventType": eventType,
		"object":    obj,
	}

	if oldObj != nil {
		payload["oldObject"] = oldObj
	}

	payload["namespace"] = objMeta["namespace"]
	payload["name"] = objMeta["name"]

	event := TriggerEvent{
		Type:      "k8s-watch",
		Source:    fmt.Sprintf("%s/%s", instance.namespace, instance.resource),
		Payload:   payload,
		Timestamp: time.Now().Unix(),
	}

	t.logger.Infow("🚀 FIRING K8s watch trigger!",
		"instanceId", instance.instanceId,
		"eventType", eventType,
		"resource", instance.resource,
		"namespace", objMeta["namespace"],
		"name", objMeta["name"],
	)

	ctx := context.Background()
	if err := t.handler(ctx, instance.instanceId, event); err != nil {
		t.logger.Errorw("❌ K8s watch trigger handler failed",
			"instanceId", instance.instanceId,
			"error", err,
		)
		fmt.Printf("k8s-watch trigger failed for instance %d: %v\n", instance.instanceId, err)
	} else {
		t.logger.Infow("✅ K8s watch trigger handler succeeded", "instanceId", instance.instanceId)
	}
}

func resourceToGVR(resource string) (schema.GroupVersionResource, error) {
	gvrMap := map[string]schema.GroupVersionResource{
		"pods":                     {Group: "", Version: "v1", Resource: "pods"},
		"services":                 {Group: "", Version: "v1", Resource: "services"},
		"configmaps":               {Group: "", Version: "v1", Resource: "configmaps"},
		"secrets":                  {Group: "", Version: "v1", Resource: "secrets"},
		"nodes":                    {Group: "", Version: "v1", Resource: "nodes"},
		"persistentvolumeclaims":   {Group: "", Version: "v1", Resource: "persistentvolumeclaims"},
		"persistentvolumes":        {Group: "", Version: "v1", Resource: "persistentvolumes"},
		"deployments":              {Group: "apps", Version: "v1", Resource: "deployments"},
		"statefulsets":             {Group: "apps", Version: "v1", Resource: "statefulsets"},
		"daemonsets":               {Group: "apps", Version: "v1", Resource: "daemonsets"},
		"replicasets":              {Group: "apps", Version: "v1", Resource: "replicasets"},
		"jobs":                     {Group: "batch", Version: "v1", Resource: "jobs"},
		"cronjobs":                 {Group: "batch", Version: "v1", Resource: "cronjobs"},
		"ingresses":                {Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
		"networkpolicies":          {Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"},
		"horizontalpodautoscalers": {Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"},
	}

	gvr, ok := gvrMap[resource]
	if !ok {
		return schema.GroupVersionResource{}, fmt.Errorf("unsupported resource type: %s", resource)
	}
	return gvr, nil
}

func extractMetadata(obj interface{}) map[string]interface{} {
	result := map[string]interface{}{
		"namespace": "",
		"name":      "",
	}

	if obj == nil {
		return result
	}

	if u, ok := obj.(*unstructured.Unstructured); ok {
		result["namespace"] = u.GetNamespace()
		result["name"] = u.GetName()
		return result
	}

	if mapObj, ok := obj.(map[string]interface{}); ok {
		if metadata, ok := mapObj["metadata"].(map[string]interface{}); ok {
			if ns, ok := metadata["namespace"].(string); ok {
				result["namespace"] = ns
			}
			if name, ok := metadata["name"].(string); ok {
				result["name"] = name
			}
		}
	}

	return result
}
