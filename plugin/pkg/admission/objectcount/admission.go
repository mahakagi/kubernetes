package objectcount

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apiserver/pkg/admission"
	admissioninitailizer "k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	objectcountapi "k8s.io/kubernetes/plugin/pkg/admission/objectcount/apis/objectcount"
)

const (
	// PluginName indicates name of admission plugin.
	PluginName = "ObjectCount"

	metricsPath  = "/metrics"
	targetMetric = "apiserver_storage_objects"

	defaultTimeout         = 60 * time.Second
	defaultRefreshInterval = 90
)

// Register registers a plugin
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName,
		func(config io.Reader) (admission.Interface, error) {
			// load the configuration provided (if any)
			configuration, err := LoadConfiguration(config)
			if err != nil {
				return nil, err
			}
			return NewObjectCount(configuration)
		})
}

// ObjectCount enforces usage limits on a per-resource basis in the namespace
type ObjectCount struct {
	*admission.Handler
	restClient rest.Interface

	refreshInterval time.Duration
	defaultLimit    *int64
	limits          map[string]int64

	// rejectResource stores the resources that should be rejected based on object counts.
	// If an resource is in this map, it will be rejected. The value is limit for the resource.
	// rejectResource map ifself should be considered as immutable and
	// to update the map you should update the reference to point a new map.
	rejectResource map[string]int64
	// lastUpdate is the most recent timestamp that the rejectResource was updated.
	lastUpdate time.Time
	mutex      sync.RWMutex
	done       chan bool
}

var _ admission.ValidationInterface = &ObjectCount{}

var _ admissioninitailizer.WantsExternalKubeClientSet = &ObjectCount{}

func NewObjectCount(config *objectcountapi.Configuration) (*ObjectCount, error) {
	// return a noop admission controller when there's no meaningful config.
	if config == nil || (len(config.Limits) == 0 && config.DefaultLimit == nil) {
		return &ObjectCount{
			Handler: admission.NewHandler(),
		}, nil
	}

	RegisterMetrics()
	oc := &ObjectCount{
		Handler:         admission.NewHandler(admission.Create),
		refreshInterval: time.Duration(*config.RefreshIntervalSeconds * int64(time.Second)),
		defaultLimit:    config.DefaultLimit,
		limits:          config.Limits,
		rejectResource:  map[string]int64{},
		mutex:           sync.RWMutex{},
		done:            make(chan bool),
	}

	go oc.updateObjectCount()
	return oc, nil
}

// SetExternalKubeClientSet registers the client into ObjectCount
func (oc *ObjectCount) SetExternalKubeClientSet(client kubernetes.Interface) {
	oc.restClient = client.CoreV1().RESTClient()
}

// TODO: ideally we should invoke this function when shutting down.
// However, there's no termination hook for admission plugins.
// It should not cause any problem even if the updater goroutine is not terminated gracefully.
func (oc *ObjectCount) Stop() {
	close(oc.done)
}

func (oc *ObjectCount) updateObjectCount() {
	ticker := time.NewTicker(oc.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-oc.done:
			return
		case <-ticker.C:
			oc.updateObjectCountOnce()
		}
	}
}

func (oc *ObjectCount) updateObjectCountOnce() {
	family, succeed := func() (*dto.MetricFamily, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
		defer cancel()

		rawResp, err := oc.restClient.Get().RequestURI(metricsPath).DoRaw(ctx)
		if err != nil {
			klog.Warningf("objectCount admission controller is unable to get apiserver metrics: %v", err)
			return nil, false
		}

		parser := &expfmt.TextParser{}
		families, err := parser.TextToMetricFamilies(bytes.NewReader(rawResp))
		if err != nil {
			klog.Warningf("objectCount admission controller is unable to parse apiserver metrics: %v", err)
			return nil, false
		}
		family, found := families[targetMetric]
		if !found {
			klog.Warningf("objectCount admission controller is unable to find the target metric: %v", targetMetric)
			return nil, false
		}
		return family, true
	}()

	if !succeed {
		cacheInvalidationStatus.Inc()
		oc.mutex.Lock()
		defer oc.mutex.Unlock()
		if oc.lastUpdate.Add(5 * oc.refreshInterval).Before(time.Now()) {
			oc.rejectResource = nil
		}
		return
	}

	newShouldReject := map[string]int64{}
	for _, metric := range family.Metric {
		var resourceName string
		if len(metric.Label) == 1 {
			resourceName = metric.Label[0].GetValue()
		} else {
			for _, label := range metric.Label {
				if label.GetName() == "resource" {
					resourceName = label.GetValue()
					break
				}
			}
			if resourceName == "" {
				klog.Warningf("objectCount admission controller is unable to find the expected resouce label: %v", targetMetric)
				continue
			}
		}
		if shouldReject, limit := oc.shouldReject(resourceName, int64(metric.Gauge.GetValue())); shouldReject {
			newShouldReject[resourceName] = limit
		}
	}

	oc.mutex.Lock()
	defer oc.mutex.Unlock()
	cacheInvalidationStatus.Set(0)
	oc.rejectResource = newShouldReject
	oc.lastUpdate = time.Now()
}

// ValidateInitialization verifies the ObjectCount object has been properly initialized
func (oc *ObjectCount) ValidateInitialization() error {
	if oc.restClient == nil {
		return fmt.Errorf("objectCount admission controller missing client")
	}
	return nil
}

// Validate admits resources into cluster that do not exceed the threshold
func (oc *ObjectCount) Validate(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) (err error) {
	// Skip request to subresources
	if a.GetSubresource() != "" {
		return nil
	}
	res := a.GetResource().GroupResource().String()

	// If threshold <= 0, we can return 429 early.
	if threshold, foundThreshold := oc.limits[res]; foundThreshold && threshold <= 0 {
		return apierrors.NewTooManyRequestsError(fmt.Sprintf("object count for %v exceeds the limit 0, creation will be rejected", res))
	}

	var rejectResource map[string]int64
	func() {
		oc.mutex.RLock()
		defer oc.mutex.RUnlock()
		// The rejectResource map is immutable, so we only need to get a reference of the map while holding the lock
		rejectResource = oc.rejectResource
	}()
	// If rejectResource map is nil, it means there's some problem to fetch the latest info to update the cache and the cache has invalidated.
	// All requests are allowed to admit.
	if rejectResource == nil {
		return nil
	}

	if limit, found := rejectResource[res]; found {
		return apierrors.NewTooManyRequestsError(fmt.Sprintf("object count for %v exceeds the limit %d, please clean up your resources and retry later", res, limit))
	}
	return nil
}

func (oc *ObjectCount) shouldReject(resource string, cnt int64) (bool, int64) {
	threshold, foundThreshold := oc.limits[resource]
	if !foundThreshold {
		if oc.defaultLimit == nil || *oc.defaultLimit < 0 {
			return false, threshold
		}
		threshold = *oc.defaultLimit
	}
	return cnt >= threshold, threshold
}
