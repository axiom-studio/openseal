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

type K8sEventTrigger struct {
	mu                 sync.RWMutex
	instances          map[int]*k8sEventInstance
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

type k8sEventInstance struct {
	instanceId         int
	namespace          string
	reason             string
	eventType          string
	involvedObjectKind string
	labelSelector      string
	fieldSelector      string
	config             map[string]interface{}
}

func NewK8sEventTrigger(
	clusterReadService clusterRead.ClusterReadService,
	k8sUtil *k8sUtils.K8sServiceImpl,
	instanceRepo repository.AgentInstanceRepository,
	environmentRepo envRepository.EnvironmentRepository,
	logger *zap.SugaredLogger,
) *K8sEventTrigger {
	return &K8sEventTrigger{
		instances:          make(map[int]*k8sEventInstance),
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

func (t *K8sEventTrigger) Type() string {
	return "k8s-event"
}

func (t *K8sEventTrigger) ValidateConfig(config map[string]interface{}) error {
	return nil
}

func (t *K8sEventTrigger) Setup(ctx context.Context, instanceId int, config map[string]interface{}) (*TriggerMetadata, error) {
	namespace := ""
	if ns, ok := config["namespace"].(string); ok {
		namespace = ns
	}

	reason := ""
	if r, ok := config["reason"].(string); ok {
		reason = r
	}

	eventType := ""
	if et, ok := config["type"].(string); ok {
		eventType = et
	}

	involvedObjectKind := ""
	if kind, ok := config["involvedObjectKind"].(string); ok {
		involvedObjectKind = kind
	}

	labelSelector := ""
	if ls, ok := config["labelSelector"].(string); ok {
		labelSelector = ls
	}

	fieldSelector := ""
	if fs, ok := config["fieldSelector"].(string); ok {
		fieldSelector = fs
	}

	instance := &k8sEventInstance{
		instanceId:         instanceId,
		namespace:          namespace,
		reason:             reason,
		eventType:          eventType,
		involvedObjectKind: involvedObjectKind,
		labelSelector:      labelSelector,
		fieldSelector:      fieldSelector,
		config:             config,
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if existing := t.instances[instanceId]; existing != nil {
		t.stopInformer(instanceId)
	}

	t.instances[instanceId] = instance

	if t.started && t.handler != nil {
		if err := t.startInformer(ctx, instance); err != nil {
			return nil, fmt.Errorf("failed to start event informer: %w", err)
		}
	}

	return &TriggerMetadata{}, nil
}

func (t *K8sEventTrigger) Teardown(ctx context.Context, instanceId int) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.stopInformer(instanceId)
	delete(t.instances, instanceId)

	return nil
}

func (t *K8sEventTrigger) Start(ctx context.Context, handler TriggerHandler) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.logger.Infow("🚀 Starting K8s Event Trigger", "instanceCount", len(t.instances))

	t.handler = handler
	t.started = true

	if len(t.instances) == 0 {
		t.logger.Warn("⚠️  No K8s event trigger instances configured")
		return nil
	}

	for _, instance := range t.instances {
		if err := t.ensureClientForInstance(ctx, instance.instanceId); err != nil {
			t.logger.Errorw("❌ Failed to create client for instance", "instanceId", instance.instanceId, "error", err)
			return fmt.Errorf("failed to create client for instance %d: %w", instance.instanceId, err)
		}

		t.logger.Infow("📡 Starting event informer for instance",
			"instanceId", instance.instanceId,
			"namespace", instance.namespace,
			"reason", instance.reason,
			"eventType", instance.eventType,
			"involvedObjectKind", instance.involvedObjectKind,
		)
		if err := t.startInformer(ctx, instance); err != nil {
			t.logger.Errorw("❌ Failed to start event informer", "instanceId", instance.instanceId, "error", err)
			return fmt.Errorf("failed to start event informer for instance %d: %w", instance.instanceId, err)
		}
		t.logger.Infow("✅ Event informer started successfully", "instanceId", instance.instanceId)
	}

	t.logger.Info("🎯 K8s Event Trigger fully started and watching for events")
	return nil
}

func (t *K8sEventTrigger) ensureClientForInstance(ctx context.Context, instanceId int) error {
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

func (t *K8sEventTrigger) Stop(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	for instanceId := range t.instances {
		t.stopInformer(instanceId)
	}

	t.started = false
	t.handler = nil

	return nil
}

func (t *K8sEventTrigger) startInformer(ctx context.Context, instance *k8sEventInstance) error {
	gvr := schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "events",
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

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if ok {
				reason, _, _ := unstructured.NestedString(u.Object, "reason")
				eventType, _, _ := unstructured.NestedString(u.Object, "type")
				message, _, _ := unstructured.NestedString(u.Object, "message")
				involvedKind, _, _ := unstructured.NestedString(u.Object, "involvedObject", "kind")
				t.logger.Debugw("📨 K8s event detected (ADD)",
					"instanceId", instance.instanceId,
					"namespace", u.GetNamespace(),
					"eventName", u.GetName(),
					"reason", reason,
					"type", eventType,
					"involvedKind", involvedKind,
					"message", message,
				)
			}
			if t.matchesFilter(instance, obj) {
				t.logger.Infow("🔥 K8s event MATCHED filter - firing trigger!",
					"instanceId", instance.instanceId,
				)
				t.fireEvent(instance, obj)
			} else {
				t.logger.Debugw("⏭️  Event did not match filter", "instanceId", instance.instanceId)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			u, ok := newObj.(*unstructured.Unstructured)
			if ok {
				reason, _, _ := unstructured.NestedString(u.Object, "reason")
				eventType, _, _ := unstructured.NestedString(u.Object, "type")
				t.logger.Debugw("📨 K8s event detected (UPDATE)",
					"instanceId", instance.instanceId,
					"namespace", u.GetNamespace(),
					"reason", reason,
					"type", eventType,
				)
			}
			if t.matchesFilter(instance, newObj) {
				t.logger.Infow("🔥 K8s event MATCHED filter - firing trigger!",
					"instanceId", instance.instanceId,
				)
				t.fireEvent(instance, newObj)
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
		return fmt.Errorf("failed to sync event cache")
	}

	return nil
}

func (t *K8sEventTrigger) stopInformer(instanceId int) {
	if stopCh, ok := t.stopChans[instanceId]; ok {
		close(stopCh)
		delete(t.stopChans, instanceId)
	}
	delete(t.informers, instanceId)
}

func (t *K8sEventTrigger) matchesFilter(instance *k8sEventInstance, obj interface{}) bool {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		t.logger.Debugw("⚠️  Object is not unstructured", "instanceId", instance.instanceId)
		return false
	}

	reason, _, _ := unstructured.NestedString(u.Object, "reason")
	eventType, _, _ := unstructured.NestedString(u.Object, "type")
	involvedKind, _, _ := unstructured.NestedString(u.Object, "involvedObject", "kind")

	t.logger.Debugw("🔍 Matching event against filter",
		"instanceId", instance.instanceId,
		"event.reason", reason,
		"filter.reason", instance.reason,
		"event.type", eventType,
		"filter.type", instance.eventType,
		"event.involvedKind", involvedKind,
		"filter.involvedKind", instance.involvedObjectKind,
	)

	if instance.reason != "" {
		if reason != instance.reason {
			t.logger.Debugw("❌ Reason mismatch", "expected", instance.reason, "got", reason)
			return false
		}
	}

	if instance.eventType != "" {
		if eventType != instance.eventType {
			t.logger.Debugw("❌ Type mismatch", "expected", instance.eventType, "got", eventType)
			return false
		}
	}

	if instance.involvedObjectKind != "" {
		if involvedKind != instance.involvedObjectKind {
			t.logger.Debugw("❌ Kind mismatch", "expected", instance.involvedObjectKind, "got", involvedKind)
			return false
		}
	}

	t.logger.Infow("✅ Event matches all filters!", "instanceId", instance.instanceId)
	return true
}

func (t *K8sEventTrigger) fireEvent(instance *k8sEventInstance, obj interface{}) {
	if t.handler == nil {
		t.logger.Warn("⚠️  No handler registered, cannot fire event")
		return
	}

	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		t.logger.Warn("⚠️  Object is not unstructured, cannot fire event")
		return
	}

	reason, _, _ := unstructured.NestedString(u.Object, "reason")
	eventType, _, _ := unstructured.NestedString(u.Object, "type")
	message, _, _ := unstructured.NestedString(u.Object, "message")
	involvedObject, _, _ := unstructured.NestedMap(u.Object, "involvedObject")

	payload := map[string]interface{}{
		"event":          u.Object,
		"reason":         reason,
		"type":           eventType,
		"message":        message,
		"involvedObject": involvedObject,
		"namespace":      u.GetNamespace(),
		"name":           u.GetName(),
	}

	event := TriggerEvent{
		Type:      "k8s-event",
		Source:    fmt.Sprintf("%s/events", instance.namespace),
		Payload:   payload,
		Timestamp: time.Now().Unix(),
	}

	t.logger.Infow("🚀 FIRING K8s event trigger!",
		"instanceId", instance.instanceId,
		"eventType", "k8s-event",
		"reason", reason,
		"type", eventType,
		"message", message,
		"namespace", u.GetNamespace(),
		"involvedObject", involvedObject,
	)

	ctx := context.Background()
	if err := t.handler(ctx, instance.instanceId, event); err != nil {
		t.logger.Errorw("❌ K8s event trigger handler failed",
			"instanceId", instance.instanceId,
			"error", err,
		)
		fmt.Printf("k8s-event trigger failed for instance %d: %v\n", instance.instanceId, err)
	} else {
		t.logger.Infow("✅ K8s event trigger handler succeeded", "instanceId", instance.instanceId)
	}
}
