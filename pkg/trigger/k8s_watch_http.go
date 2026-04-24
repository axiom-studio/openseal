package trigger

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/repository"
	envRepository "github.com/axiom-studio/openseal/pkg/environment"
	"go.uber.org/zap"
)

type K8sWatchTriggerHTTP struct {
	mu              sync.RWMutex
	instances       map[int]*k8sWatchInstanceHTTP
	pollers         map[int]context.CancelFunc
	handler         TriggerHandler
	started         bool
	k8sClient       *K8sEventClient
	instanceRepo    repository.AgentInstanceRepository
	environmentRepo envRepository.EnvironmentRepository
	logger          *zap.SugaredLogger
	resourceCache   map[int]map[string]string // instanceId -> resourceUID -> resourceVersion
}

type k8sWatchInstanceHTTP struct {
	instanceId    int
	clusterId     int
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

func NewK8sWatchTriggerHTTP(
	k8sClient *K8sEventClient,
	instanceRepo repository.AgentInstanceRepository,
	environmentRepo envRepository.EnvironmentRepository,
	logger *zap.SugaredLogger,
) *K8sWatchTriggerHTTP {
	return &K8sWatchTriggerHTTP{
		instances:       make(map[int]*k8sWatchInstanceHTTP),
		pollers:         make(map[int]context.CancelFunc),
		k8sClient:       k8sClient,
		instanceRepo:    instanceRepo,
		environmentRepo: environmentRepo,
		logger:          logger,
		resourceCache:   make(map[int]map[string]string),
	}
}

func (t *K8sWatchTriggerHTTP) Type() string {
	return "k8s-watch"
}

func (t *K8sWatchTriggerHTTP) ValidateConfig(config map[string]interface{}) error {
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

func (t *K8sWatchTriggerHTTP) Setup(ctx context.Context, instanceId int, config map[string]interface{}) (*TriggerMetadata, error) {
	if err := t.ValidateConfig(config); err != nil {
		return nil, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	agentInstance, err := t.instanceRepo.FindById(instanceId)
	if err != nil {
		return nil, fmt.Errorf("failed to get agent instance: %w", err)
	}

	environment, err := t.environmentRepo.FindById(agentInstance.EnvironmentId)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment: %w", err)
	}

	resource := config["resource"].(string)
	namespace, _ := config["namespace"].(string)
	labelSelector, _ := config["labelSelector"].(string)
	fieldSelector, _ := config["fieldSelector"].(string)

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

	instance := &k8sWatchInstanceHTTP{
		instanceId:    instanceId,
		clusterId:     environment.ClusterId,
		resource:      resource,
		namespace:     namespace,
		labelSelector: labelSelector,
		fieldSelector: fieldSelector,
		events:        events,
		debounceMs:    debounceMs,
		config:        config,
		lastFired:     make(map[string]time.Time),
	}

	t.instances[instanceId] = instance
	t.resourceCache[instanceId] = make(map[string]string)

	if t.started {
		t.startPoller(ctx, instance)
	}

	t.logger.Infow("K8s watch trigger configured (HTTP)",
		"instanceId", instanceId,
		"clusterId", environment.ClusterId,
		"resource", resource,
		"namespace", namespace,
		"events", events,
	)

	return &TriggerMetadata{}, nil
}

func (t *K8sWatchTriggerHTTP) Teardown(ctx context.Context, instanceId int) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if cancel, exists := t.pollers[instanceId]; exists {
		cancel()
		delete(t.pollers, instanceId)
	}

	delete(t.instances, instanceId)
	delete(t.resourceCache, instanceId)

	t.logger.Infow("K8s watch trigger torn down (HTTP)", "instanceId", instanceId)
	return nil
}

func (t *K8sWatchTriggerHTTP) Start(ctx context.Context, handler TriggerHandler) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.logger.Infow("Starting K8s Watch Trigger (HTTP polling)", "instanceCount", len(t.instances))

	t.handler = handler
	t.started = true

	if len(t.instances) == 0 {
		t.logger.Warn("No K8s watch trigger instances configured")
		return nil
	}

	for _, instance := range t.instances {
		t.startPoller(ctx, instance)
	}

	t.logger.Info("K8s Watch Trigger fully started and polling for resource changes")
	return nil
}

func (t *K8sWatchTriggerHTTP) startPoller(ctx context.Context, instance *k8sWatchInstanceHTTP) {
	pollCtx, cancel := context.WithCancel(ctx)
	t.pollers[instance.instanceId] = cancel

	go t.pollResources(pollCtx, instance)
}

func (t *K8sWatchTriggerHTTP) pollResources(ctx context.Context, instance *k8sWatchInstanceHTTP) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	t.logger.Infow("Starting resource poller",
		"instanceId", instance.instanceId,
		"clusterId", instance.clusterId,
		"resource", instance.resource,
		"namespace", instance.namespace,
	)

	for {
		select {
		case <-ctx.Done():
			t.logger.Infow("Stopping resource poller", "instanceId", instance.instanceId)
			return
		case <-ticker.C:
			t.checkResources(ctx, instance)
		}
	}
}

func (t *K8sWatchTriggerHTTP) checkResources(ctx context.Context, instance *k8sWatchInstanceHTTP) {
	resources, err := t.k8sClient.ListResources(ctx, instance.clusterId, instance.namespace, instance.resource, instance.labelSelector, instance.fieldSelector)
	if err != nil {
		t.logger.Errorw("failed to list resources", "instanceId", instance.instanceId, "clusterId", instance.clusterId, "namespace", instance.namespace, "resource", instance.resource, "error", err)
		return
	}

	t.mu.Lock()
	cache := t.resourceCache[instance.instanceId]
	currentUIDs := make(map[string]bool)
	t.mu.Unlock()

	for _, resource := range resources {
		metadata, ok := resource["metadata"].(map[string]interface{})
		if !ok {
			continue
		}

		uid, _ := metadata["uid"].(string)
		resourceVersion, _ := metadata["resourceVersion"].(string)
		if uid == "" {
			continue
		}

		currentUIDs[uid] = true

		t.mu.Lock()
		oldVersion, existed := cache[uid]
		t.mu.Unlock()

		if !existed {
			// New resource - ADDED event
			if t.shouldFireEvent(instance, "ADDED") {
				t.fireEvent(ctx, instance, "ADDED", resource, nil)
			}
			t.mu.Lock()
			cache[uid] = resourceVersion
			t.mu.Unlock()
		} else if oldVersion != resourceVersion {
			// Resource changed - MODIFIED event
			if t.shouldFireEvent(instance, "MODIFIED") {
				t.fireEvent(ctx, instance, "MODIFIED", resource, nil)
			}
			t.mu.Lock()
			cache[uid] = resourceVersion
			t.mu.Unlock()
		}
	}

	// Check for deleted resources
	t.mu.Lock()
	for uid := range cache {
		if !currentUIDs[uid] {
			// Resource no longer exists - DELETED event
			if t.shouldFireEvent(instance, "DELETED") {
				deletedResource := map[string]interface{}{
					"metadata": map[string]interface{}{
						"uid": uid,
					},
				}
				t.fireEvent(ctx, instance, "DELETED", deletedResource, nil)
			}
			delete(cache, uid)
		}
	}
	t.mu.Unlock()
}

func (t *K8sWatchTriggerHTTP) shouldFireEvent(instance *k8sWatchInstanceHTTP, eventType string) bool {
	for _, e := range instance.events {
		if e == eventType {
			return true
		}
	}
	return false
}

func (t *K8sWatchTriggerHTTP) fireEvent(ctx context.Context, instance *k8sWatchInstanceHTTP, eventType string, obj, oldObj interface{}) {
	objMeta := extractMetadata(obj)

	if instance.debounceMs > 0 {
		key := fmt.Sprintf("%s/%s/%s", eventType, objMeta["namespace"], objMeta["name"])

		instance.mu.Lock()
		lastFired, exists := instance.lastFired[key]
		now := time.Now()
		if exists && now.Sub(lastFired) < time.Duration(instance.debounceMs)*time.Millisecond {
			t.logger.Debugw("Event debounced (too soon after last fire)",
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

	payload := map[string]interface{}{
		"eventType": eventType,
		"object":    obj,
		"namespace": objMeta["namespace"],
		"name":      objMeta["name"],
	}

	if oldObj != nil {
		payload["oldObject"] = oldObj
	}

	event := TriggerEvent{
		Type:      "k8s-watch",
		Source:    fmt.Sprintf("cluster-%d/%s/%s", instance.clusterId, instance.namespace, instance.resource),
		Payload:   payload,
		Timestamp: time.Now().Unix(),
	}

	t.logger.Infow("K8s watch event MATCHED - firing trigger",
		"instanceId", instance.instanceId,
		"eventType", eventType,
		"resource", instance.resource,
		"namespace", objMeta["namespace"],
		"name", objMeta["name"],
	)

	if err := t.handler(ctx, instance.instanceId, event); err != nil {
		t.logger.Errorw("K8s watch trigger handler failed",
			"instanceId", instance.instanceId,
			"error", err,
		)
	} else {
		t.logger.Infow("K8s watch trigger handler succeeded", "instanceId", instance.instanceId)
	}
}

func (t *K8sWatchTriggerHTTP) Stop(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	for instanceId, cancel := range t.pollers {
		cancel()
		t.logger.Infow("Stopped resource poller", "instanceId", instanceId)
	}

	t.pollers = make(map[int]context.CancelFunc)
	t.started = false
	t.handler = nil

	return nil
}
