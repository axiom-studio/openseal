// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/callback. Remove once in-flight branches have landed.
//
// Re-exports of the callback registry, ingress and event worker, kept so
// that importers of pkg/runtime continue to compile.

package runtime

import (
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/callback"
)

type CallbackEventWorkerConfig = callback.CallbackEventWorkerConfig

type CallbackEventWorker = callback.CallbackEventWorker

// NewCallbackEventWorker forwards to callback.NewCallbackEventWorker.
func NewCallbackEventWorker(store interface {
	models.CallbackRegistrationStore
	models.CallbackEventStore
}, service *callback.CallbackIngressService, config callback.CallbackEventWorkerConfig, opts ...models.ServiceOption) (*callback.CallbackEventWorker, error) {
	return callback.NewCallbackEventWorker(store, service, config, opts...)
}

const MaximumCallbackIngressBytes = callback.MaximumCallbackIngressBytes

type CallbackPublicRequest = callback.CallbackPublicRequest

type NormalizedCallbackEvent = callback.NormalizedCallbackEvent

type CallbackHostRequest = callback.CallbackHostRequest

type CallbackHostResult = callback.CallbackHostResult

type CallbackAdapterHost = callback.CallbackAdapterHost

type CallbackEventConsumer = callback.CallbackEventConsumer

type CallbackEventConsumerFunc = callback.CallbackEventConsumerFunc

type CallbackIngressResult = callback.CallbackIngressResult

type CallbackIngressService = callback.CallbackIngressService

// NewCallbackIngressService forwards to callback.NewCallbackIngressService.
func NewCallbackIngressService(store interface {
	models.CallbackRegistrationStore
	models.CallbackEventStore
}, resolver callback.CallbackAdapterResolver, consumers map[string]callback.CallbackEventConsumer, opts ...models.ServiceOption) *callback.CallbackIngressService {
	return callback.NewCallbackIngressService(store, resolver, consumers, opts...)
}

type CreateCallbackRegistrationRequest = callback.CreateCallbackRegistrationRequest

type UpdateCallbackRegistrationRequest = callback.UpdateCallbackRegistrationRequest

type CallbackAdapterResolver = callback.CallbackAdapterResolver

type CallbackRegistry = callback.CallbackRegistry

// NewCallbackRegistry forwards to callback.NewCallbackRegistry.
func NewCallbackRegistry(store models.CallbackRegistrationStore, resolver callback.CallbackAdapterResolver, opts ...models.ServiceOption) *callback.CallbackRegistry {
	return callback.NewCallbackRegistry(store, resolver, opts...)
}
