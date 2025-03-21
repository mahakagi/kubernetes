package objectcount

import (
	"sync"

	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

const objectCountAdmissionControllerSubsystem = "object_count_admission_controller"

var (
	cacheInvalidationStatus = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      objectCountAdmissionControllerSubsystem,
			Name:           "cache_invalidation_status",
			Help:           "Gauge of if the reporting system has invalidated its object count cache due to staleness, 0 indicates no issue, X (greater than 0) indicates it has been failing to fetch metrics for X times and cache has been invalidated.",
			StabilityLevel: metrics.ALPHA},
	)
)

var registerMetrics sync.Once

// RegisterMetrics registers metrics for node package.
func RegisterMetrics() {
	registerMetrics.Do(func() {
		legacyregistry.MustRegister(cacheInvalidationStatus)
	})
}
