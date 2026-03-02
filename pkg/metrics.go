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
	)
}
