package pkg

import (
	"github.com/prometheus/client_golang/prometheus"
)

type KairosMetrics struct {
	RestartTotal         *prometheus.CounterVec
	TrackedResources     *prometheus.GaugeVec
	RestartErrorsTotal   *prometheus.CounterVec
	RestartDuration      *prometheus.HistogramVec
	ScheduledJobs        *prometheus.GaugeVec
	QueueDepth           *prometheus.GaugeVec
	SyncErrorsTotal      *prometheus.CounterVec
	ChainExecutionTotal  *prometheus.CounterVec
	ChainStepDuration    *prometheus.HistogramVec
	ChainSkippedTotal    *prometheus.CounterVec
}

func NewKairosMetrics() *KairosMetrics {
	return &KairosMetrics{
		RestartTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairos_restart_total",
			Help: "Total number of successful workload restarts",
		}, []string{"kind", "namespace", "name"}),
		TrackedResources: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairos_tracked_resources",
			Help: "Number of resources currently being tracked",
		}, []string{"kind"}),
		RestartErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairos_restart_errors_total",
			Help: "Total number of errors during workload restarts",
		}, []string{"kind", "namespace", "name", "error_phase"}),
		RestartDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kairos_restart_duration_seconds",
			Help:    "Duration of restart operations (Get + Update) in seconds",
			Buckets: prometheus.DefBuckets,
		}, []string{"kind", "namespace", "name"}),
		ScheduledJobs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairos_scheduled_jobs",
			Help: "Number of currently scheduled cron jobs",
		}, []string{"kind"}),
		QueueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairos_queue_depth",
			Help: "Current depth of the controller work queue",
		}, []string{"kind"}),
		SyncErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairos_sync_errors_total",
			Help: "Total number of sync errors after exhausting retries",
		}, []string{"kind"}),
		ChainExecutionTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairos_chain_execution_total",
			Help: "Total number of chain executions by outcome",
		}, []string{"chain", "result"}),
		ChainStepDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kairos_chain_step_duration_seconds",
			Help:    "Duration of individual chain steps in seconds",
			Buckets: prometheus.ExponentialBuckets(1, 2, 12),
		}, []string{"chain", "kind", "namespace", "name"}),
		ChainSkippedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairos_chain_skipped_total",
			Help: "Total number of chain triggers skipped because the chain was already running",
		}, []string{"chain"}),
	}
}

func (m *KairosMetrics) Register(reg prometheus.Registerer) {
	reg.MustRegister(
		m.RestartTotal,
		m.TrackedResources,
		m.RestartErrorsTotal,
		m.RestartDuration,
		m.ScheduledJobs,
		m.QueueDepth,
		m.SyncErrorsTotal,
		m.ChainExecutionTotal,
		m.ChainStepDuration,
		m.ChainSkippedTotal,
	)
}
