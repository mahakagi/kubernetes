package objectcount

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/handlers"
	fakerest "k8s.io/client-go/rest/fake"
	objectcountapi "k8s.io/kubernetes/plugin/pkg/admission/objectcount/apis/objectcount"
)

const (
	metricText = `# HELP apiserver_storage_objects [STABLE] Number of stored objects at the time of last check split by kind. In case of a fetching error, the value will be -1.
# TYPE apiserver_storage_objects gauge
apiserver_storage_objects{resource="configmaps"} 60
apiserver_storage_objects{resource="pods"} 101
apiserver_storage_objects{resource="deployments.apps"} 10
`
	metricText2 = `# HELP apiserver_storage_objects [STABLE] Number of stored objects at the time of last check split by kind. In case of a fetching error, the value will be -1.
# TYPE apiserver_storage_objects gauge
apiserver_storage_objects{resource="configmaps"} 20
apiserver_storage_objects{resource="pods"} 101
apiserver_storage_objects{resource="deployments.apps"} 100
`
)

func TestUpdateObjectCountOnce(t *testing.T) {
	defaultLimit := int64(100)
	hr := &httpResponder{
		responses: []*response{
			{resp: metricText},
			{resp: metricText2},
		},
	}
	oc := &ObjectCount{
		Handler: admission.NewHandler(admission.Create),
		restClient: &fakerest.RESTClient{
			Client: fakerest.CreateHTTPClient(hr.RoundTrip),
		},
		refreshInterval: time.Second,
		defaultLimit:    &defaultLimit,
		limits:          map[string]int64{"configmaps": 50},
		rejectResource:  map[string]int64{},
		mutex:           sync.RWMutex{},
	}
	oc.updateObjectCountOnce()
	expected := map[string]int64{
		"configmaps": 50,
		"pods":       100,
	}
	if !reflect.DeepEqual(oc.rejectResource, expected) {
		t.Fatalf("expect %v to equal %v", oc.rejectResource, expected)
	}

	oc.updateObjectCountOnce()
	expected = map[string]int64{
		"pods":             100,
		"deployments.apps": 100,
	}
	if !reflect.DeepEqual(oc.rejectResource, expected) {
		t.Fatalf("expect %v to equal %v", oc.rejectResource, expected)
	}
}

func TestAdmission(t *testing.T) {
	defaultLimit := int64(100)
	hr := &httpResponder{
		responses: []*response{
			{resp: metricText},
			{resp: metricText2},
		},
	}
	oc := &ObjectCount{
		Handler: admission.NewHandler(admission.Create),
		restClient: &fakerest.RESTClient{
			Client: fakerest.CreateHTTPClient(hr.RoundTrip),
		},
		refreshInterval: time.Second,
		defaultLimit:    &defaultLimit,
		limits:          map[string]int64{"configmaps": 50},
		mutex:           sync.RWMutex{},
	}

	// Allow all admission when cache is not populated e.g. fail to fetch metrics and cache has been invalidated.
	err := oc.Validate(context.Background(), newAttributes(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}), &handlers.RequestScope{})
	if err != nil {
		t.Errorf("expect no error, but got: %v", err)
	}
	err = oc.Validate(context.Background(), newAttributes(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}), &handlers.RequestScope{})
	if err != nil {
		t.Errorf("expect no error, but got: %v", err)
	}
	err = oc.Validate(context.Background(), newAttributes(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}), &handlers.RequestScope{})
	if err != nil {
		t.Errorf("expect no error, but got: %v", err)
	}

	// populate cache
	oc.updateObjectCountOnce()
	err = oc.Validate(context.Background(), newAttributes(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}), &handlers.RequestScope{})
	if !apierrors.IsTooManyRequests(err) {
		t.Errorf("expect a TooManyRequests error, but got: %v", err)
	}
	err = oc.Validate(context.Background(), newAttributes(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}), &handlers.RequestScope{})
	if !apierrors.IsTooManyRequests(err) {
		t.Errorf("expect a TooManyRequests error, but got: %v", err)
	}
	err = oc.Validate(context.Background(), newAttributes(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}), &handlers.RequestScope{})
	if err != nil {
		t.Errorf("expect no error, but got: %v", err)
	}

	// update cache with different values
	oc.updateObjectCountOnce()
	err = oc.Validate(context.Background(), newAttributes(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}), &handlers.RequestScope{})
	if err != nil {
		t.Errorf("expect no error, but got: %v", err)
	}
	err = oc.Validate(context.Background(), newAttributes(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}), &handlers.RequestScope{})
	if !apierrors.IsTooManyRequests(err) {
		t.Errorf("expect a TooManyRequests error, but got: %v", err)
	}
	err = oc.Validate(context.Background(), newAttributes(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}), &handlers.RequestScope{})
	if !apierrors.IsTooManyRequests(err) {
		t.Errorf("expect a TooManyRequests error, but got: %v", err)
	}
}

func TestCacheInvalidationAndRepopulation(t *testing.T) {
	hr := &httpResponder{
		responses: []*response{
			{err: errors.New("error foo")},
			{err: errors.New("error foo")},
			{resp: metricText},
		},
	}
	oc := &ObjectCount{
		Handler: admission.NewHandler(admission.Create),
		restClient: &fakerest.RESTClient{
			Client: fakerest.CreateHTTPClient(hr.RoundTrip),
		},
		refreshInterval: 10 * time.Second,
		rejectResource: map[string]int64{
			"configmaps": 50,
			"pods":       100,
		},
		lastUpdate: time.Now(),
		mutex:      sync.RWMutex{},
	}
	// cache stays even it gets an error when fetching metrics
	oc.updateObjectCountOnce()
	if oc.rejectResource == nil {
		t.Fatalf("unexpected cache invalidation, oc.rejectResource should not be nil")
	}
	// Mock some time passage.
	oc.lastUpdate = time.Now().Add(-10 * oc.refreshInterval)
	// error when fetching metrics from APIserver and cache is invalidated due to staleness
	oc.updateObjectCountOnce()
	if oc.rejectResource != nil {
		t.Fatalf("cache should be invalidated and oc.rejectResource should be nil")
	}
	// cache is re-populated after a successful metrics fetch
	oc.updateObjectCountOnce()
	if oc.rejectResource == nil {
		t.Fatalf("expect cache to be re-populated and oc.rejectResource should not be nil")
	}
}

type httpResponder struct {
	curIdx    int
	responses []*response
}

type response struct {
	resp string
	err  error
}

func (r *httpResponder) RoundTrip(req *http.Request) (*http.Response, error) {
	if r.curIdx >= len(r.responses) {
		return nil, fmt.Errorf("httpResponder goes out of bound: %v", r.curIdx)
	}
	response := r.responses[r.curIdx]
	r.curIdx++
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(response.resp)),
	}, response.err
}

func newAttributes(gvr schema.GroupVersionResource) admission.Attributes {
	return admission.NewAttributesRecord(&unstructured.Unstructured{}, nil, schema.GroupVersionKind{}, "ns", "name", gvr, "", admission.Create, nil, false, &user.DefaultInfo{})
}

func TestLoadConfiguration(t *testing.T) {
	testcases := []struct {
		description string
		input       string
		expected    *objectcountapi.Configuration
		expectedErr string
	}{
		{
			description: "all fields are specified",
			input: `apiVersion: objectcount.admission.k8s.io/__internal
kind: Configuration
refreshIntervalSeconds: 30
defaultLimit: 500000
limits:
  events: 10000`,
			expected: &objectcountapi.Configuration{
				RefreshIntervalSeconds: func() *int64 { v := int64(30); return &v }(),
				DefaultLimit:           func() *int64 { v := int64(500000); return &v }(),
				Limits: map[string]int64{
					"events": 10000,
				},
			},
		},
		{
			description: "some fields are omitted",
			input: `apiVersion: objectcount.admission.k8s.io/__internal
kind: Configuration
limits:
  events: 10000`,
			expected: &objectcountapi.Configuration{
				RefreshIntervalSeconds: func() *int64 { v := int64(90); return &v }(),
				Limits: map[string]int64{
					"events": 10000,
				},
			},
		},
		{
			description: "all fields are omitted",
			input: `apiVersion: objectcount.admission.k8s.io/__internal
kind: Configuration`,
			expected: &objectcountapi.Configuration{
				RefreshIntervalSeconds: func() *int64 { v := int64(90); return &v }(),
			},
		},
		{
			description: "unknown resource version",
			input: `apiVersion: objectcount.admission.k8s.io/v1
kind: Configuration`,
			expectedErr: `no kind "Configuration" is registered for version "objectcount.admission.k8s.io/v1" in scheme`,
		},
	}
	for _, tc := range testcases {
		config, err := LoadConfiguration(strings.NewReader(tc.input))
		if tc.expectedErr == "" && err != nil {
			t.Errorf("expected no error but got: %v", err)
		} else if !strings.Contains(fmt.Sprintf("%v", err), tc.expectedErr) {
			t.Errorf("expected: %v to contain: %v", err, tc.expectedErr)
			continue
		}
		if !reflect.DeepEqual(tc.expected, config) {
			t.Errorf("expected: %#v but got: %#v", tc.expected, config)
		}
	}
}
