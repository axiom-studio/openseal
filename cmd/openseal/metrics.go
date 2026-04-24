package main

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

type MetricsRegistry struct {
	runtimeInfo          *prometheus.GaugeVec
	executionTotal       *prometheus.CounterVec
	executionDuration    *prometheus.HistogramVec
	executionInProgress  prometheus.Gauge
	executionQueueLength prometheus.Gauge
	stepExecutionTotal   *prometheus.CounterVec
	stepExecutionDuration *prometheus.HistogramVec
	lastHeartbeatTimestamp prometheus.Gauge
	natsConnectionStatus   prometheus.Gauge
	workerCount            prometheus.Gauge
	errorTotal             *prometheus.CounterVec
}

func NewMetricsRegistry(clusterId int, namespace, version string) *MetricsRegistry {
	m := &MetricsRegistry{
		runtimeInfo: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "openseal_runtime_info",
				Help: "Information about the openseal runtime",
			},
			[]string{"cluster_id", "namespace", "version"},
		),
		executionTotal: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "openseal_execution_total",
				Help: "Total number of agent executions",
			},
			[]string{"status"},
		),
		executionDuration: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "openseal_execution_duration_seconds",
				Help:    "Duration of agent executions in seconds",
				Buckets: prometheus.ExponentialBuckets(0.1, 2, 10),
			},
			[]string{"status"},
		),
		executionInProgress: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "openseal_execution_in_progress",
				Help: "Number of agent executions currently in progress",
			},
		),
		executionQueueLength: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "openseal_execution_queue_length",
				Help: "Number of agent executions waiting in queue",
			},
		),
		stepExecutionTotal: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "openseal_step_execution_total",
				Help: "Total number of step executions",
			},
			[]string{"step_type", "status"},
		),
		stepExecutionDuration: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "openseal_step_execution_duration_seconds",
				Help:    "Duration of step executions in seconds",
				Buckets: prometheus.ExponentialBuckets(0.01, 2, 12),
			},
			[]string{"step_type"},
		),
		lastHeartbeatTimestamp: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "openseal_runtime_last_heartbeat_timestamp",
				Help: "Unix timestamp of the last successful heartbeat",
			},
		),
		natsConnectionStatus: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "openseal_runtime_nats_connection_status",
				Help: "NATS connection status (1=connected, 0=disconnected)",
			},
		),
		workerCount: promauto.NewGauge(
			prometheus.GaugeOpts{
				Name: "openseal_runtime_worker_count",
				Help: "Number of active workers in this runtime",
			},
		),
		errorTotal: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "openseal_runtime_errors_total",
				Help: "Total number of errors by type",
			},
			[]string{"error_type"},
		),
	}

	m.runtimeInfo.WithLabelValues(
		strconv.Itoa(clusterId),
		namespace,
		version,
	).Set(1)

	return m
}

func (m *MetricsRegistry) RecordExecutionStart() {
	m.executionInProgress.Inc()
}

func (m *MetricsRegistry) RecordExecutionComplete(duration time.Duration, success bool) {
	m.executionInProgress.Dec()
	status := "success"
	if !success {
		status = "failed"
	}
	m.executionTotal.WithLabelValues(status).Inc()
	m.executionDuration.WithLabelValues(status).Observe(duration.Seconds())
}

func (m *MetricsRegistry) RecordStepExecution(stepType string, duration time.Duration, success bool) {
	status := "success"
	if !success {
		status = "failed"
	}
	m.stepExecutionTotal.WithLabelValues(stepType, status).Inc()
	m.stepExecutionDuration.WithLabelValues(stepType).Observe(duration.Seconds())
}

func (m *MetricsRegistry) RecordHeartbeat() {
	m.lastHeartbeatTimestamp.Set(float64(time.Now().Unix()))
}

func (m *MetricsRegistry) SetNATSConnectionStatus(connected bool) {
	if connected {
		m.natsConnectionStatus.Set(1)
	} else {
		m.natsConnectionStatus.Set(0)
	}
}

func (m *MetricsRegistry) SetWorkerCount(count int) {
	m.workerCount.Set(float64(count))
}

func (m *MetricsRegistry) RecordError(errorType string) {
	m.errorTotal.WithLabelValues(errorType).Inc()
}

func (m *MetricsRegistry) SetQueueLength(length int) {
	m.executionQueueLength.Set(float64(length))
}

func StartMetricsServer(port int, logger *zap.SugaredLogger) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	go func() {
		logger.Infow("starting metrics server", "port", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Errorw("metrics server error", "error", err)
		}
	}()

	return server
}
