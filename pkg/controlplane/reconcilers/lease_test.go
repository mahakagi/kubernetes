/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package reconcilers

/*
Original Source:
https://github.com/openshift/origin/blob/bb340c5dd5ff72718be86fb194dedc0faed7f4c7/pkg/cmd/server/election/lease_endpoint_reconciler_test.go
*/

import (
	"context"
	"net"
	"os"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/apitesting"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/storage"
	etcd3testing "k8s.io/apiserver/pkg/storage/etcd3/testing"
	"k8s.io/apiserver/pkg/storage/storagebackend/factory"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/kubernetes/pkg/apis/core"
	az "k8s.io/kubernetes/pkg/kubeapiserver/az"
	netutils "k8s.io/utils/net"
)

func init() {
	var scheme = runtime.NewScheme()

	metav1.AddToGroupVersion(scheme, metav1.SchemeGroupVersion)
	utilruntime.Must(core.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(scheme.SetVersionPriority(corev1.SchemeGroupVersion))

	codecs = serializer.NewCodecFactory(scheme)
}

var codecs serializer.CodecFactory

type fakeLeases struct {
	storageLeases
}

var _ Leases = &fakeLeases{}

func newFakeLeases(t *testing.T, s storage.Interface) *fakeLeases {
	// use the same base key used by the controlplane, but add a random
	// prefix so we can reuse the etcd instance for subtests independently.
	// pkg/controlplane/instance.go:268:
	// masterLeases, err := reconcilers.NewLeases(config, "/masterleases/", ttl)
	// ref: https://issues.k8s.io/114049
	base := "/" + uuid.New().String() + "/masterleases/"
	return &fakeLeases{
		storageLeases{
			storage:   s,
			destroyFn: func() {},
			baseKey:   base,
			leaseTime: 1 * time.Minute, // avoid the lease to timeout on tests
		},
	}
}

func (f *fakeLeases) SetKeys(keys []string) error {
	for _, ip := range keys {
		if err := f.UpdateLease(ip); err != nil {
			return err
		}
	}
	return nil
}

func TestLeaseEndpointReconciler(t *testing.T) {
	server, sc := etcd3testing.NewUnsecuredEtcd3TestClientServer(t)
	t.Cleanup(func() { server.Terminate(t) })

	newFunc := func() runtime.Object { return &corev1.Endpoints{} }
	newListFunc := func() runtime.Object { return &corev1.EndpointsList{} }
	sc.Codec = apitesting.TestStorageCodec(codecs, corev1.SchemeGroupVersion)

	s, dFunc, err := factory.Create(*sc.ForResource(schema.GroupResource{Resource: "endpoints"}), newFunc, newListFunc, "")
	if err != nil {
		t.Fatalf("Error creating storage: %v", err)
	}
	t.Cleanup(dFunc)

	reconcileTests := []struct {
		testName      string
		serviceName   string
		ip            string
		endpointPorts []corev1.EndpointPort
		endpointKeys  []string
		initialState  []runtime.Object
		expectUpdate  []runtime.Object
		expectCreate  []runtime.Object
		expectLeases  []string
	}{
		{
			testName:      "no existing endpoints",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			initialState:  nil,
			expectCreate:  makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases:  []string{"1.2.3.4"},
		},
		{
			testName:      "existing endpoints satisfy",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			initialState:  makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases:  []string{"1.2.3.4"},
		},
		{
			testName:      "existing endpoints satisfy, no endpointslice",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			initialState: []runtime.Object{
				makeEndpoints("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			},
			expectCreate: []runtime.Object{
				makeEndpointSlice("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			},
			expectLeases: []string{"1.2.3.4"},
		},
		{
			testName:      "existing endpointslice satisfies, no endpoints",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			initialState: []runtime.Object{
				makeEndpointSlice("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			},
			expectCreate: []runtime.Object{
				makeEndpoints("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			},
			expectLeases: []string{"1.2.3.4"},
		},
		{
			testName:      "existing endpoints satisfy, endpointslice is wrong",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			initialState: []runtime.Object{
				makeEndpoints("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
				makeEndpointSlice("foo", []string{"4.3.2.1"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			},
			expectUpdate: []runtime.Object{
				makeEndpointSlice("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			},
			expectLeases: []string{"1.2.3.4"},
		},
		{
			testName:      "existing endpointslice satisfies, endpoints is wrong",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			initialState: []runtime.Object{
				makeEndpoints("foo", []string{"4.3.2.1"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
				makeEndpointSlice("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			},
			expectUpdate: []runtime.Object{
				makeEndpoints("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			},
			expectLeases: []string{"1.2.3.4"},
		},
		{
			testName:      "existing endpoints satisfy + refresh existing key",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"1.2.3.4"},
			initialState:  makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases:  []string{"1.2.3.4"},
		},
		{
			testName:      "existing endpoints satisfy but too many",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			initialState:  makeEndpointsArray("foo", []string{"1.2.3.4", "4.3.2.1"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectUpdate:  makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases:  []string{"1.2.3.4"},
		},
		{
			testName:      "existing endpoints satisfy but too many + extra masters",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"1.2.3.4", "4.3.2.2", "4.3.2.3", "4.3.2.4"},
			initialState:  makeEndpointsArray("foo", []string{"1.2.3.4", "4.3.2.1", "4.3.2.2", "4.3.2.3", "4.3.2.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectUpdate:  makeEndpointsArray("foo", []string{"1.2.3.4", "4.3.2.2", "4.3.2.3", "4.3.2.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases:  []string{"1.2.3.4", "4.3.2.2", "4.3.2.3", "4.3.2.4"},
		},
		{
			testName:      "existing endpoints satisfy but too many + extra masters + delete first",
			serviceName:   "foo",
			ip:            "4.3.2.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"4.3.2.1", "4.3.2.2", "4.3.2.3", "4.3.2.4"},
			initialState:  makeEndpointsArray("foo", []string{"1.2.3.4", "4.3.2.1", "4.3.2.2", "4.3.2.3", "4.3.2.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectUpdate:  makeEndpointsArray("foo", []string{"4.3.2.1", "4.3.2.2", "4.3.2.3", "4.3.2.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases:  []string{"4.3.2.1", "4.3.2.2", "4.3.2.3", "4.3.2.4"},
		},
		{
			testName:      "existing endpoints current IP missing",
			serviceName:   "foo",
			ip:            "4.3.2.2",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"4.3.2.1"},
			initialState:  makeEndpointsArray("foo", []string{"4.3.2.1"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectUpdate:  makeEndpointsArray("foo", []string{"4.3.2.1", "4.3.2.2"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases:  []string{"4.3.2.1", "4.3.2.2"},
		},
		{
			testName:      "existing endpoints wrong name",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			initialState:  makeEndpointsArray("bar", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectCreate:  makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases:  []string{"1.2.3.4"},
		},
		{
			testName:      "existing endpoints wrong IP",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			initialState:  makeEndpointsArray("foo", []string{"4.3.2.1"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectUpdate:  makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases:  []string{"1.2.3.4"},
		},
		{
			testName:      "existing endpoints wrong port",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			initialState:  makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 9090, Protocol: "TCP"}}),
			expectUpdate:  makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases:  []string{"1.2.3.4"},
		},
		{
			testName:      "existing endpoints wrong protocol",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			initialState:  makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "UDP"}}),
			expectUpdate:  makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases:  []string{"1.2.3.4"},
		},
		{
			testName:      "existing endpoints wrong port name",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "baz", Port: 8080, Protocol: "TCP"}},
			initialState:  makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectUpdate:  makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "baz", Port: 8080, Protocol: "TCP"}}),
			expectLeases:  []string{"1.2.3.4"},
		},
		{
			testName:      "existing endpoints without skip mirror label",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			initialState: []runtime.Object{
				// can't use makeEndpointsArray() here because we don't want the
				// skip-mirror label
				&corev1.Endpoints{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: metav1.NamespaceDefault,
						Name:      "foo",
					},
					Subsets: []corev1.EndpointSubset{{
						Addresses: []corev1.EndpointAddress{{IP: "1.2.3.4"}},
						Ports:     []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
					}},
				},
				makeEndpointSlice("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			},
			expectUpdate: []runtime.Object{
				makeEndpoints("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
				// EndpointSlice does not get updated because it was already correct
			},
			expectLeases: []string{"1.2.3.4"},
		},
		{
			testName:    "existing endpoints extra service ports satisfy",
			serviceName: "foo",
			ip:          "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{
				{Name: "foo", Port: 8080, Protocol: "TCP"},
				{Name: "bar", Port: 1000, Protocol: "TCP"},
				{Name: "baz", Port: 1010, Protocol: "TCP"},
			},
			initialState: makeEndpointsArray("foo", []string{"1.2.3.4"},
				[]corev1.EndpointPort{
					{Name: "foo", Port: 8080, Protocol: "TCP"},
					{Name: "bar", Port: 1000, Protocol: "TCP"},
					{Name: "baz", Port: 1010, Protocol: "TCP"},
				},
			),
			expectLeases: []string{"1.2.3.4"},
		},
		{
			testName:    "existing endpoints extra service ports missing port",
			serviceName: "foo",
			ip:          "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{
				{Name: "foo", Port: 8080, Protocol: "TCP"},
				{Name: "bar", Port: 1000, Protocol: "TCP"},
			},
			initialState: makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectUpdate: makeEndpointsArray("foo", []string{"1.2.3.4"},
				[]corev1.EndpointPort{
					{Name: "foo", Port: 8080, Protocol: "TCP"},
					{Name: "bar", Port: 1000, Protocol: "TCP"},
				},
			),
			expectLeases: []string{"1.2.3.4"},
		},
	}
	for _, test := range reconcileTests {
		t.Run(test.testName, func(t *testing.T) {
			fakeLeases := newFakeLeases(t, s)
			err := fakeLeases.SetKeys(test.endpointKeys)
			if err != nil {
				t.Errorf("unexpected error creating keys: %v", err)
			}
			clientset := fake.NewSimpleClientset(test.initialState...)

			epAdapter := NewEndpointsAdapter(clientset.CoreV1(), clientset.DiscoveryV1())
			r := NewLeaseEndpointReconciler(epAdapter, fakeLeases)
			err = r.ReconcileEndpoints(test.serviceName, netutils.ParseIPSloppy(test.ip), test.endpointPorts, true)
			if err != nil {
				t.Errorf("unexpected error reconciling: %v", err)
			}

			err = verifyCreatesAndUpdates(clientset, test.expectCreate, test.expectUpdate)
			if err != nil {
				t.Errorf("unexpected error in side effects: %v", err)
			}

			leases, err := fakeLeases.ListLeases()
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			// sort for comparison
			sort.Strings(leases)
			sort.Strings(test.expectLeases)
			if !reflect.DeepEqual(leases, test.expectLeases) {
				t.Errorf("expected %v got: %v", test.expectLeases, leases)
			}
		})
	}

	nonReconcileTests := []struct {
		testName      string
		serviceName   string
		ip            string
		endpointPorts []corev1.EndpointPort
		endpointKeys  []string
		initialState  []runtime.Object
		expectUpdate  []runtime.Object
		expectCreate  []runtime.Object
		expectLeases  []string
	}{
		{
			testName:    "existing endpoints extra service ports missing port no update",
			serviceName: "foo",
			ip:          "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{
				{Name: "foo", Port: 8080, Protocol: "TCP"},
				{Name: "bar", Port: 1000, Protocol: "TCP"},
			},
			initialState: makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectUpdate: nil,
			expectLeases: []string{"1.2.3.4"},
		},
		{
			testName:    "existing endpoints extra service ports, wrong ports, wrong IP",
			serviceName: "foo",
			ip:          "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{
				{Name: "foo", Port: 8080, Protocol: "TCP"},
				{Name: "bar", Port: 1000, Protocol: "TCP"},
			},
			initialState: makeEndpointsArray("foo", []string{"4.3.2.1"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectUpdate: makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases: []string{"1.2.3.4"},
		},
		{
			testName:      "no existing endpoints",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			initialState:  nil,
			expectCreate:  makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases:  []string{"1.2.3.4"},
		},
	}
	for _, test := range nonReconcileTests {
		t.Run(test.testName, func(t *testing.T) {
			fakeLeases := newFakeLeases(t, s)
			err := fakeLeases.SetKeys(test.endpointKeys)
			if err != nil {
				t.Errorf("unexpected error creating keys: %v", err)
			}
			clientset := fake.NewSimpleClientset(test.initialState...)
			epAdapter := NewEndpointsAdapter(clientset.CoreV1(), clientset.DiscoveryV1())
			r := NewLeaseEndpointReconciler(epAdapter, fakeLeases)
			err = r.ReconcileEndpoints(test.serviceName, netutils.ParseIPSloppy(test.ip), test.endpointPorts, false)
			if err != nil {
				t.Errorf("unexpected error reconciling: %v", err)
			}

			err = verifyCreatesAndUpdates(clientset, test.expectCreate, test.expectUpdate)
			if err != nil {
				t.Errorf("unexpected error in side effects: %v", err)
			}

			leases, err := fakeLeases.ListLeases()
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			// sort for comparison
			sort.Strings(leases)
			sort.Strings(test.expectLeases)
			if !reflect.DeepEqual(leases, test.expectLeases) {
				t.Errorf("expected %v got: %v", test.expectLeases, leases)
			}
		})
	}
}

func TestLeaseEndpointReconcilerForAZEvacuation(t *testing.T) {
	server, sc := etcd3testing.NewUnsecuredEtcd3TestClientServer(t)
	t.Cleanup(func() { server.Terminate(t) })

	newFunc := func() runtime.Object { return &corev1.Endpoints{} }
	newListFunc := func() runtime.Object { return &corev1.EndpointsList{} }

	sc.Codec = apitesting.TestStorageCodec(codecs, corev1.SchemeGroupVersion)

	s, dFunc, err := factory.Create(*sc.ForResource(schema.GroupResource{Resource: "endpoints"}), newFunc, newListFunc, "")
	if err != nil {
		t.Fatalf("Error creating storage: %v", err)
	}
	t.Cleanup(dFunc)

	ns := corev1.NamespaceDefault
	futureExpiryEpoch := strconv.FormatInt(time.Now().Add(time.Hour).UTC().Unix(), 10)
	om := func(name string, evacuateAZ string, applyLabel bool) metav1.ObjectMeta {
		o := metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels: map[string]string{
				discoveryv1.LabelSkipMirror:     "true",
				az.ZoneEvacuationExpiryLabelKey: futureExpiryEpoch,
			},
		}
		if applyLabel {
			o.Labels[az.ZoneEvacuationLabelKey] = evacuateAZ
		}

		return o
	}
	reconcileTests := []struct {
		testName        string
		serviceName     string
		ip              string
		endpointPorts   []corev1.EndpointPort
		endpointKeys    []string
		endpoints       *corev1.EndpointsList
		expectEndpoints *corev1.Endpoints // nil means none expected
		evacuateZone    string
		currentZone     string
		expectLeases    []string
	}{
		{
			testName:      "existing endpoints current AZ is not evacuated AZ",
			serviceName:   "foo",
			ip:            "4.3.2.2",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"4.3.2.1"},
			endpoints: &corev1.EndpointsList{
				Items: []corev1.Endpoints{{
					ObjectMeta: om("foo", "az2", true),
					Subsets: []corev1.EndpointSubset{{
						Addresses: []corev1.EndpointAddress{
							{IP: "4.3.2.1"},
						},
						Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
					}},
				}},
			},
			expectEndpoints: &corev1.Endpoints{
				ObjectMeta: om("foo", "az2", true),
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{
						{IP: "4.3.2.1"},
						{IP: "4.3.2.2"},
					},
					Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
				}},
			},
			currentZone:  "az1",
			evacuateZone: "az2",
			expectLeases: []string{"4.3.2.1", "4.3.2.2"},
		},
		{
			testName:      "one other existing endpoints current AZ is equal to evacuated AZ",
			serviceName:   "foo",
			ip:            "4.3.2.2",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"4.3.2.1"},
			endpoints: &corev1.EndpointsList{
				Items: []corev1.Endpoints{{
					ObjectMeta: om("foo", "az1", true),
					Subsets: []corev1.EndpointSubset{{
						Addresses: []corev1.EndpointAddress{
							{IP: "4.3.2.1"},
						},
						Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
					}},
				}},
			},
			expectEndpoints: &corev1.Endpoints{
				ObjectMeta: om("foo", "az1", true),
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{
						{IP: "4.3.2.1"},
					},
					Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
				}},
			},
			currentZone:  "az1",
			evacuateZone: "az1",
			expectLeases: []string{"4.3.2.1"},
		},
		{
			testName:      "same endpoint existing current AZ is equal to evacuated AZ(fail open)",
			serviceName:   "foo",
			ip:            "4.3.2.1",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"4.3.2.1"},
			endpoints: &corev1.EndpointsList{
				Items: []corev1.Endpoints{{
					ObjectMeta: om("foo", "az1", true),
					Subsets: []corev1.EndpointSubset{{
						Addresses: []corev1.EndpointAddress{
							{IP: "4.3.2.1"},
						},
						Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
					}},
				}},
			},
			expectEndpoints: &corev1.Endpoints{
				ObjectMeta: om("foo", "az1", true),
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{
						{IP: "4.3.2.1"},
					},
					Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
				}},
			},
			currentZone:  "az1",
			evacuateZone: "az1",
			expectLeases: []string{"4.3.2.1"},
		},
		{
			testName:      "No endpoint existing current AZ is equal to evacuated AZ(fail open)",
			serviceName:   "foo",
			ip:            "4.3.2.1",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{},
			endpoints:     nil,
			expectEndpoints: &corev1.Endpoints{
				ObjectMeta: om("foo", "", false),
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{
						{IP: "4.3.2.1"},
					},
					Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
				}},
			},
			currentZone:  "az1",
			evacuateZone: "az1",
			expectLeases: []string{"4.3.2.1"},
		},
		{
			testName:      "same endpoint(ipv6) existing current AZ is equal to evacuated AZ(fail open)",
			serviceName:   "foo",
			ip:            net.ParseIP("2001:0db8:0001:0000:0000:0ab9:C0A8:0102").String(),
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"2001:db8:1::ab9:c0a8:102"},
			endpoints: &corev1.EndpointsList{
				Items: []corev1.Endpoints{{
					ObjectMeta: om("foo", "az1", true),
					Subsets: []corev1.EndpointSubset{{
						Addresses: []corev1.EndpointAddress{
							{IP: "2001:0db8:0001:0000:0000:0ab9:C0A8:0102"},
						},
						Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
					}},
				}},
			},
			expectEndpoints: &corev1.Endpoints{
				ObjectMeta: om("foo", "az1", true),
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{
						{IP: "2001:db8:1::ab9:c0a8:102"},
					},
					Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
				}},
			},
			currentZone:  "az1",
			evacuateZone: "az1",
			expectLeases: []string{"2001:db8:1::ab9:c0a8:102"},
		},
		{
			testName:      "existing endpoints current AZ and evacuated AZ are empty",
			serviceName:   "foo",
			ip:            "4.3.2.2",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"4.3.2.1"},
			endpoints: &corev1.EndpointsList{
				Items: []corev1.Endpoints{{
					ObjectMeta: om("foo", "", true),
					Subsets: []corev1.EndpointSubset{{
						Addresses: []corev1.EndpointAddress{
							{IP: "4.3.2.1"},
						},
						Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
					}},
				}},
			},
			expectEndpoints: &corev1.Endpoints{
				ObjectMeta: om("foo", "", true),
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{
						{IP: "4.3.2.1"},
						{IP: "4.3.2.2"},
					},
					Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
				}},
			},
			currentZone:  "",
			evacuateZone: "",
			expectLeases: []string{"4.3.2.1", "4.3.2.2"},
		},
	}
	for _, test := range reconcileTests {
		t.Run(test.testName, func(t *testing.T) {
			fakeLeases := newFakeLeases(t, s)
			err := fakeLeases.SetKeys(test.endpointKeys)
			if err != nil {
				t.Errorf("unexpected error creating keys: %v", err)
			}

			os.Setenv(az.CurrentZoneEnvironmentKey, test.currentZone)
			defer os.Unsetenv(az.CurrentZoneEnvironmentKey)
			clientset := fake.NewSimpleClientset()
			if test.endpoints != nil {
				for _, ep := range test.endpoints.Items {
					if _, err := clientset.CoreV1().Endpoints(ep.Namespace).Create(context.TODO(), &ep, metav1.CreateOptions{}); err != nil {
						t.Errorf("case %q: unexpected error: %v", test.testName, err)
						continue
					}
				}
			}

			epAdapter := NewEndpointsAdapter(clientset.CoreV1(), clientset.DiscoveryV1())
			r := NewLeaseEndpointReconciler(epAdapter, fakeLeases)
			err = r.ReconcileEndpoints(test.serviceName, net.ParseIP(test.ip), test.endpointPorts, true)
			if err != nil {
				t.Errorf("case %q: unexpected error: %v", test.testName, err)
			}
			actualEndpoints, err := clientset.CoreV1().Endpoints(corev1.NamespaceDefault).Get(context.TODO(), test.serviceName, metav1.GetOptions{})
			if err != nil {
				t.Errorf("case %q: unexpected error: %v", test.testName, err)
			}
			if test.expectEndpoints != nil {
				delete(actualEndpoints.Labels, az.ZoneEvacuationExpiryLabelKey)
				delete(test.expectEndpoints.Labels, az.ZoneEvacuationExpiryLabelKey)
				if e, a := test.expectEndpoints, actualEndpoints; !reflect.DeepEqual(e, a) {
					t.Errorf("case %q: expected update:\n%#v\ngot:\n%#v\n", test.testName, e, a)
				}
			}

			leases, err := fakeLeases.ListLeases()
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			// sort for comparison
			sort.Strings(leases)
			sort.Strings(test.expectLeases)
			if !reflect.DeepEqual(leases, test.expectLeases) {
				t.Errorf("expected %v got: %v", test.expectLeases, leases)
			}
		})
	}
}

func TestLeaseEndpointReconcilerForAZEvacuationExpiryScenarios(t *testing.T) {
	server, sc := etcd3testing.NewUnsecuredEtcd3TestClientServer(t)
	t.Cleanup(func() { server.Terminate(t) })

	newFunc := func() runtime.Object { return &corev1.Endpoints{} }
	newListFunc := func() runtime.Object { return &corev1.EndpointsList{} }
	sc.Codec = apitesting.TestStorageCodec(codecs, corev1.SchemeGroupVersion)

	s, dFunc, err := factory.Create(*sc.ForResource(schema.GroupResource{Resource: "endpoints"}), newFunc, newListFunc, "")
	if err != nil {
		t.Fatalf("Error creating storage: %v", err)
	}
	t.Cleanup(dFunc)

	ns := corev1.NamespaceDefault
	expiredExpiryEpoch := strconv.FormatInt(time.Now().Add(-time.Hour).UTC().Unix(), 10)
	om := func(name string, evacuateAZ string, applyLabel bool) metav1.ObjectMeta {
		o := metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels: map[string]string{
				discoveryv1.LabelSkipMirror:     "true",
				az.ZoneEvacuationExpiryLabelKey: expiredExpiryEpoch,
			},
		}
		if applyLabel {
			o.Labels[az.ZoneEvacuationLabelKey] = evacuateAZ
		}

		return o
	}
	reconcileTests := []struct {
		testName        string
		serviceName     string
		ip              string
		endpointPorts   []corev1.EndpointPort
		endpointKeys    []string
		endpoints       *corev1.EndpointsList
		expectEndpoints *corev1.Endpoints // nil means none expected
		evacuateZone    string
		currentZone     string
		expectLeases    []string
	}{
		{
			testName:      "existing endpoints current AZ is not evacuated AZ",
			serviceName:   "foo",
			ip:            "4.3.2.2",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"4.3.2.1"},
			endpoints: &corev1.EndpointsList{
				Items: []corev1.Endpoints{{
					ObjectMeta: om("foo", "az2", true),
					Subsets: []corev1.EndpointSubset{{
						Addresses: []corev1.EndpointAddress{
							{IP: "4.3.2.1"},
						},
						Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
					}},
				}},
			},
			expectEndpoints: &corev1.Endpoints{
				ObjectMeta: om("foo", "az2", true),
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{
						{IP: "4.3.2.1"},
						{IP: "4.3.2.2"},
					},
					Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
				}},
			},
			currentZone:  "az1",
			evacuateZone: "az2",
			expectLeases: []string{"4.3.2.1", "4.3.2.2"},
		},
		{
			testName:      "one other existing endpoints current AZ is equal to evacuated AZ",
			serviceName:   "foo",
			ip:            "4.3.2.2",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"4.3.2.1"},
			endpoints: &corev1.EndpointsList{
				Items: []corev1.Endpoints{{
					ObjectMeta: om("foo", "az1", true),
					Subsets: []corev1.EndpointSubset{{
						Addresses: []corev1.EndpointAddress{
							{IP: "4.3.2.1"},
						},
						Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
					}},
				}},
			},
			expectEndpoints: &corev1.Endpoints{
				ObjectMeta: om("foo", "az1", true),
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{
						{IP: "4.3.2.1"},
						{IP: "4.3.2.2"},
					},
					Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
				}},
			},
			currentZone:  "az1",
			evacuateZone: "az1",
			expectLeases: []string{"4.3.2.1", "4.3.2.2"},
		},
		{
			testName:      "same endpoint existing current AZ is equal to evacuated AZ(fail open)",
			serviceName:   "foo",
			ip:            "4.3.2.1",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"4.3.2.1"},
			endpoints: &corev1.EndpointsList{
				Items: []corev1.Endpoints{{
					ObjectMeta: om("foo", "az1", true),
					Subsets: []corev1.EndpointSubset{{
						Addresses: []corev1.EndpointAddress{
							{IP: "4.3.2.1"},
						},
						Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
					}},
				}},
			},
			expectEndpoints: &corev1.Endpoints{
				ObjectMeta: om("foo", "az1", true),
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{
						{IP: "4.3.2.1"},
					},
					Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
				}},
			},
			currentZone:  "az1",
			evacuateZone: "az1",
			expectLeases: []string{"4.3.2.1"},
		},
		{
			testName:      "No endpoint existing current AZ is equal to evacuated AZ(fail open)",
			serviceName:   "foo",
			ip:            "4.3.2.1",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{},
			endpoints:     nil,
			expectEndpoints: &corev1.Endpoints{
				ObjectMeta: om("foo", "", false),
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{
						{IP: "4.3.2.1"},
					},
					Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
				}},
			},
			currentZone:  "az1",
			evacuateZone: "az1",
			expectLeases: []string{"4.3.2.1"},
		},
		{
			testName:      "existing endpoints current AZ and evacuated AZ are empty",
			serviceName:   "foo",
			ip:            "4.3.2.2",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"4.3.2.1"},
			endpoints: &corev1.EndpointsList{
				Items: []corev1.Endpoints{{
					ObjectMeta: om("foo", "", true),
					Subsets: []corev1.EndpointSubset{{
						Addresses: []corev1.EndpointAddress{
							{IP: "4.3.2.1"},
						},
						Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
					}},
				}},
			},
			expectEndpoints: &corev1.Endpoints{
				ObjectMeta: om("foo", "", true),
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{
						{IP: "4.3.2.1"},
						{IP: "4.3.2.2"},
					},
					Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
				}},
			},
			currentZone:  "",
			evacuateZone: "",
			expectLeases: []string{"4.3.2.1", "4.3.2.2"},
		},
	}
	for _, test := range reconcileTests {
		t.Run(test.testName, func(t *testing.T) {
			fakeLeases := newFakeLeases(t, s)
			err := fakeLeases.SetKeys(test.endpointKeys)
			if err != nil {
				t.Errorf("unexpected error creating keys: %v", err)
			}

			os.Setenv(az.CurrentZoneEnvironmentKey, test.currentZone)
			defer os.Unsetenv(az.CurrentZoneEnvironmentKey)
			clientset := fake.NewSimpleClientset()
			if test.endpoints != nil {
				for _, ep := range test.endpoints.Items {
					if _, err := clientset.CoreV1().Endpoints(ep.Namespace).Create(context.TODO(), &ep, metav1.CreateOptions{}); err != nil {
						t.Errorf("case %q: unexpected error: %v", test.testName, err)
						continue
					}
				}
			}

			epAdapter := NewEndpointsAdapter(clientset.CoreV1(), clientset.DiscoveryV1())
			r := NewLeaseEndpointReconciler(epAdapter, fakeLeases)
			err = r.ReconcileEndpoints(test.serviceName, net.ParseIP(test.ip), test.endpointPorts, true)
			if err != nil {
				t.Errorf("case %q: unexpected error: %v", test.testName, err)
			}
			actualEndpoints, err := clientset.CoreV1().Endpoints(corev1.NamespaceDefault).Get(context.TODO(), test.serviceName, metav1.GetOptions{})
			if err != nil {
				t.Errorf("case %q: unexpected error: %v", test.testName, err)
			}
			if test.expectEndpoints != nil {
				delete(actualEndpoints.Labels, az.ZoneEvacuationExpiryLabelKey)
				delete(test.expectEndpoints.Labels, az.ZoneEvacuationExpiryLabelKey)
				if e, a := test.expectEndpoints, actualEndpoints; !reflect.DeepEqual(e, a) {
					t.Errorf("case %q: expected update:\n%#v\ngot:\n%#v\n", test.testName, e, a)
				}
			}

			leases, err := fakeLeases.ListLeases()
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			// sort for comparison
			sort.Strings(leases)
			sort.Strings(test.expectLeases)
			if !reflect.DeepEqual(leases, test.expectLeases) {
				t.Errorf("expected %v got: %v", test.expectLeases, leases)
			}
		})
	}
}

func TestLeaseEndpointReconcilerForAZEvacuationExpiryNotSetShouldFailOpen(t *testing.T) {
	server, sc := etcd3testing.NewUnsecuredEtcd3TestClientServer(t)
	t.Cleanup(func() { server.Terminate(t) })

	newFunc := func() runtime.Object { return &corev1.Endpoints{} }
	newListFunc := func() runtime.Object { return &corev1.EndpointsList{} }
	sc.Codec = apitesting.TestStorageCodec(codecs, corev1.SchemeGroupVersion)

	s, dFunc, err := factory.Create(*sc.ForResource(schema.GroupResource{Resource: "endpoints"}), newFunc, newListFunc, "")
	if err != nil {
		t.Fatalf("Error creating storage: %v", err)
	}
	t.Cleanup(dFunc)

	ns := corev1.NamespaceDefault
	om := func(name string, labels map[string]string) metav1.ObjectMeta {
		o := metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
		}

		o.SetLabels(labels)
		o.Labels[discoveryv1.LabelSkipMirror] = "true"

		return o
	}
	reconcileTests := []struct {
		testName        string
		serviceName     string
		ip              string
		endpointPorts   []corev1.EndpointPort
		endpointKeys    []string
		endpoints       *corev1.EndpointsList
		expectEndpoints *corev1.Endpoints // nil means none expected
		evacuateZone    string
		currentZone     string
		expectLeases    []string
	}{
		{
			testName:      "current AZ is evacuated AZ expiry unset",
			serviceName:   "foo",
			ip:            "4.3.2.2",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"4.3.2.1"},
			endpoints: &corev1.EndpointsList{
				Items: []corev1.Endpoints{{
					ObjectMeta: om("foo", map[string]string{az.ZoneEvacuationLabelKey: "az1"}),
					Subsets: []corev1.EndpointSubset{{
						Addresses: []corev1.EndpointAddress{
							{IP: "4.3.2.1"},
						},
						Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
					}},
				}},
			},
			expectEndpoints: &corev1.Endpoints{
				ObjectMeta: om("foo", map[string]string{az.ZoneEvacuationLabelKey: "az1"}),
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{
						{IP: "4.3.2.1"},
						{IP: "4.3.2.2"},
					},
					Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
				}},
			},
			currentZone:  "az1",
			evacuateZone: "az1",
			expectLeases: []string{"4.3.2.1", "4.3.2.2"},
		},
		{
			testName:      "current AZ is evacuated AZ expiry empty",
			serviceName:   "foo",
			ip:            "4.3.2.2",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"4.3.2.1"},
			endpoints: &corev1.EndpointsList{
				Items: []corev1.Endpoints{{
					ObjectMeta: om("foo", map[string]string{az.ZoneEvacuationLabelKey: "az1", az.ZoneEvacuationExpiryLabelKey: ""}),
					Subsets: []corev1.EndpointSubset{{
						Addresses: []corev1.EndpointAddress{
							{IP: "4.3.2.1"},
						},
						Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
					}},
				}},
			},
			expectEndpoints: &corev1.Endpoints{
				ObjectMeta: om("foo", map[string]string{az.ZoneEvacuationLabelKey: "az1", az.ZoneEvacuationExpiryLabelKey: ""}),
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{
						{IP: "4.3.2.1"},
						{IP: "4.3.2.2"},
					},
					Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
				}},
			},
			currentZone:  "az1",
			evacuateZone: "az1",
			expectLeases: []string{"4.3.2.1", "4.3.2.2"},
		},
		{
			testName:      "current AZ is evacuated AZ expiry invalid",
			serviceName:   "foo",
			ip:            "4.3.2.2",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"4.3.2.1"},
			endpoints: &corev1.EndpointsList{
				Items: []corev1.Endpoints{{
					ObjectMeta: om("foo", map[string]string{az.ZoneEvacuationLabelKey: "az1", az.ZoneEvacuationExpiryLabelKey: "abc"}),
					Subsets: []corev1.EndpointSubset{{
						Addresses: []corev1.EndpointAddress{
							{IP: "4.3.2.1"},
						},
						Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
					}},
				}},
			},
			expectEndpoints: &corev1.Endpoints{
				ObjectMeta: om("foo", map[string]string{az.ZoneEvacuationLabelKey: "az1", az.ZoneEvacuationExpiryLabelKey: "abc"}),
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{
						{IP: "4.3.2.1"},
						{IP: "4.3.2.2"},
					},
					Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
				}},
			},
			currentZone:  "az1",
			evacuateZone: "az1",
			expectLeases: []string{"4.3.2.1", "4.3.2.2"},
		},
	}
	for _, test := range reconcileTests {
		t.Run(test.testName, func(t *testing.T) {
			fakeLeases := newFakeLeases(t, s)
			err := fakeLeases.SetKeys(test.endpointKeys)
			if err != nil {
				t.Errorf("unexpected error creating keys: %v", err)
			}

			os.Setenv(az.CurrentZoneEnvironmentKey, test.currentZone)
			defer os.Unsetenv(az.CurrentZoneEnvironmentKey)
			clientset := fake.NewSimpleClientset()
			if test.endpoints != nil {
				for _, ep := range test.endpoints.Items {
					if _, err := clientset.CoreV1().Endpoints(ep.Namespace).Create(context.TODO(), &ep, metav1.CreateOptions{}); err != nil {
						t.Errorf("case %q: unexpected error: %v", test.testName, err)
						continue
					}
				}
			}

			epAdapter := NewEndpointsAdapter(clientset.CoreV1(), clientset.DiscoveryV1())
			r := NewLeaseEndpointReconciler(epAdapter, fakeLeases)
			err = r.ReconcileEndpoints(test.serviceName, net.ParseIP(test.ip), test.endpointPorts, true)
			if err != nil {
				t.Errorf("case %q: unexpected error: %v", test.testName, err)
			}
			actualEndpoints, err := clientset.CoreV1().Endpoints(corev1.NamespaceDefault).Get(context.TODO(), test.serviceName, metav1.GetOptions{})
			if err != nil {
				t.Errorf("case %q: unexpected error: %v", test.testName, err)
			}
			if test.expectEndpoints != nil {
				delete(actualEndpoints.Labels, az.ZoneEvacuationExpiryLabelKey)
				delete(test.expectEndpoints.Labels, az.ZoneEvacuationExpiryLabelKey)
				if e, a := test.expectEndpoints, actualEndpoints; !reflect.DeepEqual(e, a) {
					t.Errorf("case %q: expected update:\n%#v\ngot:\n%#v\n", test.testName, e, a)
				}
			}

			leases, err := fakeLeases.ListLeases()
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			// sort for comparison
			sort.Strings(leases)
			sort.Strings(test.expectLeases)
			if !reflect.DeepEqual(leases, test.expectLeases) {
				t.Errorf("expected %v got: %v", test.expectLeases, leases)
			}
		})
	}
}

func TestLeaseEndpointReconcilerStopsForLocalhost(t *testing.T) {
	server, sc := etcd3testing.NewUnsecuredEtcd3TestClientServer(t)
	t.Cleanup(func() { server.Terminate(t) })

	newFunc := func() runtime.Object { return &corev1.Endpoints{} }
	newListFunc := func() runtime.Object { return &corev1.EndpointsList{} }
	sc.Codec = apitesting.TestStorageCodec(codecs, corev1.SchemeGroupVersion)

	s, dFunc, err := factory.Create(*sc.ForResource(schema.GroupResource{Resource: "endpoints"}), newFunc, newListFunc, "")
	if err != nil {
		t.Fatalf("Error creating storage: %v", err)
	}
	t.Cleanup(dFunc)

	ns := corev1.NamespaceDefault
	futureExpiryEpoch := strconv.FormatInt(time.Now().Add(time.Hour).UTC().Unix(), 10)
	om := func(name string, evacuateAZ string, applyLabel bool) metav1.ObjectMeta {
		o := metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels: map[string]string{
				discoveryv1.LabelSkipMirror:     "true",
				az.ZoneEvacuationExpiryLabelKey: futureExpiryEpoch,
			},
		}
		if applyLabel {
			o.Labels[az.ZoneEvacuationLabelKey] = evacuateAZ
		}

		return o
	}
	reconcileTests := []struct {
		testName        string
		serviceName     string
		ip              string
		endpointPorts   []corev1.EndpointPort
		endpointKeys    []string
		endpoints       *corev1.EndpointsList
		expectEndpoints *corev1.Endpoints // nil means none expected
		evacuateZone    string
		currentZone     string
		expectLeases    []string
	}{
		{
			testName:      "Skip Reconcile Localhost",
			serviceName:   "foo",
			ip:            "127.0.0.1",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"4.3.2.1"},
			endpoints: &corev1.EndpointsList{
				Items: []corev1.Endpoints{{
					ObjectMeta: om("foo", "az2", false),
					Subsets: []corev1.EndpointSubset{{
						Addresses: []corev1.EndpointAddress{
							{IP: "4.3.2.1"},
						},
						Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
					}},
				}},
			},
			expectEndpoints: &corev1.Endpoints{
				ObjectMeta: om("foo", "az2", false),
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{
						{IP: "4.3.2.1"},
					},
					Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
				}},
			},
			currentZone:  "az1",
			evacuateZone: "az2",
			expectLeases: []string{"4.3.2.1"},
		},
		{
			testName:      "Skip Reconcile Localhost Ipv6",
			serviceName:   "foo",
			ip:            "::1",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"2600:1f13:1bf:b00::"},
			endpoints: &corev1.EndpointsList{
				Items: []corev1.Endpoints{{
					ObjectMeta: om("foo", "az2", false),
					Subsets: []corev1.EndpointSubset{{
						Addresses: []corev1.EndpointAddress{
							{IP: "2600:1f13:1bf:b00::"},
						},
						Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
					}},
				}},
			},
			expectEndpoints: &corev1.Endpoints{
				ObjectMeta: om("foo", "az2", false),
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{
						{IP: "2600:1f13:1bf:b00::"},
					},
					Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
				}},
			},
			currentZone:  "az1",
			evacuateZone: "az2",
			expectLeases: []string{"2600:1f13:1bf:b00::"},
		},
		{
			testName:      "Localhost skips when current zone is evacuated zone",
			serviceName:   "foo",
			ip:            "127.0.0.1",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"4.3.2.1"},
			endpoints: &corev1.EndpointsList{
				Items: []corev1.Endpoints{{
					ObjectMeta: om("foo", "az1", true),
					Subsets: []corev1.EndpointSubset{{
						Addresses: []corev1.EndpointAddress{
							{IP: "4.3.2.1"},
						},
						Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
					}},
				}},
			},
			expectEndpoints: &corev1.Endpoints{
				ObjectMeta: om("foo", "az1", true),
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{
						{IP: "4.3.2.1"},
					},
					Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
				}},
			},
			currentZone:  "az1",
			evacuateZone: "az1",
			expectLeases: []string{"4.3.2.1"},
		},
		{
			testName:      "Localhost skips when current zone is not evacuated zone",
			serviceName:   "foo",
			ip:            "127.0.0.1",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"4.3.2.2"},
			endpoints: &corev1.EndpointsList{
				Items: []corev1.Endpoints{{
					ObjectMeta: om("foo", "az1", true),
					Subsets: []corev1.EndpointSubset{{
						Addresses: []corev1.EndpointAddress{
							{IP: "4.3.2.2"},
						},
						Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
					}},
				}},
			},
			expectEndpoints: &corev1.Endpoints{
				ObjectMeta: om("foo", "az1", true),
				Subsets: []corev1.EndpointSubset{{
					Addresses: []corev1.EndpointAddress{
						{IP: "4.3.2.2"},
					},
					Ports: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
				}},
			},
			currentZone:  "az1",
			evacuateZone: "az2",
			expectLeases: []string{"4.3.2.2"},
		},
	}
	for _, test := range reconcileTests {
		t.Run(test.testName, func(t *testing.T) {
			fakeLeases := newFakeLeases(t, s)
			err := fakeLeases.SetKeys(test.endpointKeys)
			if err != nil {
				t.Errorf("unexpected error creating keys: %v", err)
			}

			os.Setenv(az.CurrentZoneEnvironmentKey, test.currentZone)
			defer os.Unsetenv(az.CurrentZoneEnvironmentKey)
			clientset := fake.NewSimpleClientset()
			if test.endpoints != nil {
				for _, ep := range test.endpoints.Items {
					if _, err := clientset.CoreV1().Endpoints(ep.Namespace).Create(context.TODO(), &ep, metav1.CreateOptions{}); err != nil {
						t.Errorf("case %q: unexpected error: %v", test.testName, err)
						continue
					}
				}
			}

			epAdapter := NewEndpointsAdapter(clientset.CoreV1(), clientset.DiscoveryV1())
			r := NewLeaseEndpointReconciler(epAdapter, fakeLeases)
			err = r.ReconcileEndpoints(test.serviceName, net.ParseIP(test.ip), test.endpointPorts, true)
			if err != nil {
				t.Errorf("case %q: unexpected error: %v", test.testName, err)
			}
			actualEndpoints, err := clientset.CoreV1().Endpoints(corev1.NamespaceDefault).Get(context.TODO(), test.serviceName, metav1.GetOptions{})
			if err != nil {
				t.Errorf("case %q: unexpected error: %v", test.testName, err)
			}
			if test.expectEndpoints != nil {
				delete(actualEndpoints.Labels, az.ZoneEvacuationExpiryLabelKey)
				delete(test.expectEndpoints.Labels, az.ZoneEvacuationExpiryLabelKey)
				if e, a := test.expectEndpoints, actualEndpoints; !reflect.DeepEqual(e, a) {
					t.Errorf("case %q: expected update:\n%#v\ngot:\n%#v\n", test.testName, e, a)
				}
			}

			leases, err := fakeLeases.ListLeases()
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			// sort for comparison
			sort.Strings(leases)
			sort.Strings(test.expectLeases)
			if !reflect.DeepEqual(leases, test.expectLeases) {
				t.Errorf("expected %v got: %v", test.expectLeases, leases)
			}
		})
	}
}

func TestLeaseRemoveEndpoints(t *testing.T) {
	server, sc := etcd3testing.NewUnsecuredEtcd3TestClientServer(t)
	t.Cleanup(func() { server.Terminate(t) })

	newFunc := func() runtime.Object { return &corev1.Endpoints{} }
	newListFunc := func() runtime.Object { return &corev1.EndpointsList{} }
	sc.Codec = apitesting.TestStorageCodec(codecs, corev1.SchemeGroupVersion)

	s, dFunc, err := factory.Create(*sc.ForResource(schema.GroupResource{Resource: "pods"}), newFunc, newListFunc, "")
	if err != nil {
		t.Fatalf("Error creating storage: %v", err)
	}
	t.Cleanup(dFunc)

	stopTests := []struct {
		testName         string
		serviceName      string
		ip               string
		endpointPorts    []corev1.EndpointPort
		endpointKeys     []string
		initialState     []runtime.Object
		expectUpdate     []runtime.Object
		expectLeases     []string
		apiServerStartup bool
	}{
		{
			testName:         "successful remove previous endpoints before apiserver starts",
			serviceName:      "foo",
			ip:               "1.2.3.4",
			endpointPorts:    []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:     []string{"1.2.3.4", "4.3.2.2", "4.3.2.3", "4.3.2.4"},
			initialState:     makeEndpointsArray("foo", []string{"1.2.3.4", "4.3.2.2", "4.3.2.3", "4.3.2.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectUpdate:     makeEndpointsArray("foo", []string{"4.3.2.2", "4.3.2.3", "4.3.2.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases:     []string{"4.3.2.2", "4.3.2.3", "4.3.2.4"},
			apiServerStartup: true,
		},
		{
			testName:      "successful stop reconciling",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"1.2.3.4", "4.3.2.2", "4.3.2.3", "4.3.2.4"},
			initialState:  makeEndpointsArray("foo", []string{"1.2.3.4", "4.3.2.2", "4.3.2.3", "4.3.2.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectUpdate:  makeEndpointsArray("foo", []string{"4.3.2.2", "4.3.2.3", "4.3.2.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases:  []string{"4.3.2.2", "4.3.2.3", "4.3.2.4"},
		},
		{
			testName:      "stop reconciling with ip not in endpoint ip list",
			serviceName:   "foo",
			ip:            "5.6.7.8",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"1.2.3.4", "4.3.2.2", "4.3.2.3", "4.3.2.4"},
			initialState:  makeEndpointsArray("foo", []string{"1.2.3.4", "4.3.2.2", "4.3.2.3", "4.3.2.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases:  []string{"1.2.3.4", "4.3.2.2", "4.3.2.3", "4.3.2.4"},
		},
		{
			testName:      "endpoint with no subset",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"1.2.3.4", "4.3.2.2", "4.3.2.3", "4.3.2.4"},
			initialState:  makeEndpointsArray("foo", nil, nil),
			expectUpdate:  makeEndpointsArray("foo", []string{"4.3.2.2", "4.3.2.3", "4.3.2.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases:  []string{"4.3.2.2", "4.3.2.3", "4.3.2.4"},
		},
		{
			testName:      "the last API server was shut down cleanly",
			serviceName:   "foo",
			ip:            "1.2.3.4",
			endpointPorts: []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:  []string{"1.2.3.4"},
			initialState:  makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectUpdate:  makeEndpointsArray("foo", []string{}, []corev1.EndpointPort{}),
			expectLeases:  []string{},
		},
	}
	for _, test := range stopTests {
		t.Run(test.testName, func(t *testing.T) {
			fakeLeases := newFakeLeases(t, s)
			err := fakeLeases.SetKeys(test.endpointKeys)
			if err != nil {
				t.Errorf("unexpected error creating keys: %v", err)
			}
			clientset := fake.NewSimpleClientset(test.initialState...)
			epAdapter := NewEndpointsAdapter(clientset.CoreV1(), clientset.DiscoveryV1())
			r := NewLeaseEndpointReconciler(epAdapter, fakeLeases)
			if !test.apiServerStartup {
				r.StopReconciling()
			}
			err = r.RemoveEndpoints(test.serviceName, netutils.ParseIPSloppy(test.ip), test.endpointPorts)
			// if the ip is not on the endpoints, it must return an storage error and stop reconciling
			if !contains(test.endpointKeys, test.ip) {
				if !storage.IsNotFound(err) {
					t.Errorf("expected error StorageError: key not found, Code: 1, Key: /registry/base/key/%s got:  %v", test.ip, err)
				}
			} else if err != nil {
				t.Errorf("unexpected error reconciling: %v", err)
			}

			err = verifyCreatesAndUpdates(clientset, nil, test.expectUpdate)
			if err != nil {
				t.Errorf("unexpected error in side effects: %v", err)
			}

			leases, err := fakeLeases.ListLeases()
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			// sort for comparison
			sort.Strings(leases)
			sort.Strings(test.expectLeases)
			if !reflect.DeepEqual(leases, test.expectLeases) {
				t.Errorf("expected %v got: %v", test.expectLeases, leases)
			}
		})
	}
}

func contains(s []string, str string) bool {
	for _, v := range s {
		if v == str {
			return true
		}
	}
	return false
}

func TestApiserverShutdown(t *testing.T) {
	server, sc := etcd3testing.NewUnsecuredEtcd3TestClientServer(t)
	t.Cleanup(func() { server.Terminate(t) })

	newFunc := func() runtime.Object { return &corev1.Endpoints{} }
	newListFunc := func() runtime.Object { return &corev1.EndpointsList{} }
	sc.Codec = apitesting.TestStorageCodec(codecs, corev1.SchemeGroupVersion)

	s, dFunc, err := factory.Create(*sc.ForResource(schema.GroupResource{Resource: "endpoints"}), newFunc, newListFunc, "")
	if err != nil {
		t.Fatalf("Error creating storage: %v", err)
	}
	t.Cleanup(dFunc)

	reconcileTests := []struct {
		testName                string
		serviceName             string
		ip                      string
		endpointPorts           []corev1.EndpointPort
		endpointKeys            []string
		initialState            []runtime.Object
		expectUpdate            []runtime.Object
		expectLeases            []string
		shutDownBeforeReconcile bool
	}{
		{
			testName:                "last apiserver shutdown after endpoint reconcile",
			serviceName:             "foo",
			ip:                      "1.2.3.4",
			endpointPorts:           []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:            []string{"1.2.3.4"},
			initialState:            makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectUpdate:            makeEndpointsArray("foo", []string{}, []corev1.EndpointPort{}),
			expectLeases:            []string{},
			shutDownBeforeReconcile: false,
		},
		{
			testName:                "last apiserver shutdown before endpoint reconcile",
			serviceName:             "foo",
			ip:                      "1.2.3.4",
			endpointPorts:           []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:            []string{"1.2.3.4"},
			initialState:            makeEndpointsArray("foo", []string{"1.2.3.4"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectUpdate:            makeEndpointsArray("foo", []string{}, []corev1.EndpointPort{}),
			expectLeases:            []string{},
			shutDownBeforeReconcile: true,
		},
		{
			testName:                "not the last apiserver which was shutdown before endpoint reconcile",
			serviceName:             "foo",
			ip:                      "1.2.3.4",
			endpointPorts:           []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:            []string{"1.2.3.4", "4.3.2.1"},
			initialState:            makeEndpointsArray("foo", []string{"1.2.3.4", "4.3.2.1"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectUpdate:            makeEndpointsArray("foo", []string{"4.3.2.1"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases:            []string{"4.3.2.1"},
			shutDownBeforeReconcile: true,
		},
		{
			testName:                "not the last apiserver which was shutdown after endpoint reconcile",
			serviceName:             "foo",
			ip:                      "1.2.3.4",
			endpointPorts:           []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}},
			endpointKeys:            []string{"1.2.3.4", "4.3.2.1"},
			initialState:            makeEndpointsArray("foo", []string{"1.2.3.4", "4.3.2.1"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectUpdate:            makeEndpointsArray("foo", []string{"4.3.2.1"}, []corev1.EndpointPort{{Name: "foo", Port: 8080, Protocol: "TCP"}}),
			expectLeases:            []string{"4.3.2.1"},
			shutDownBeforeReconcile: false,
		},
	}
	for _, test := range reconcileTests {
		t.Run(test.testName, func(t *testing.T) {
			fakeLeases := newFakeLeases(t, s)
			err := fakeLeases.SetKeys(test.endpointKeys)
			if err != nil {
				t.Errorf("unexpected error creating keys: %v", err)
			}
			clientset := fake.NewSimpleClientset(test.initialState...)

			epAdapter := NewEndpointsAdapter(clientset.CoreV1(), clientset.DiscoveryV1())
			r := NewLeaseEndpointReconciler(epAdapter, fakeLeases)

			if test.shutDownBeforeReconcile {
				// shutdown apiserver first
				r.StopReconciling()
				err = r.RemoveEndpoints(test.serviceName, netutils.ParseIPSloppy(test.ip), test.endpointPorts)
				if err != nil {
					t.Errorf("unexpected error remove endpoints: %v", err)
				}

				// reconcile endpoints in another goroutine
				err = r.ReconcileEndpoints(test.serviceName, netutils.ParseIPSloppy(test.ip), test.endpointPorts, false)
				if err != nil {
					t.Errorf("unexpected error reconciling: %v", err)
				}
			} else {
				// reconcile endpoints first
				err = r.ReconcileEndpoints(test.serviceName, netutils.ParseIPSloppy(test.ip), test.endpointPorts, false)
				if err != nil {
					t.Errorf("unexpected error reconciling: %v", err)
				}

				r.StopReconciling()
				err = r.RemoveEndpoints(test.serviceName, netutils.ParseIPSloppy(test.ip), test.endpointPorts)
				if err != nil {
					t.Errorf("unexpected error remove endpoints: %v", err)
				}
			}

			err = verifyCreatesAndUpdates(clientset, nil, test.expectUpdate)
			if err != nil {
				t.Errorf("unexpected error in side effects: %v", err)
			}

			leases, err := fakeLeases.ListLeases()
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			// sort for comparison
			sort.Strings(leases)
			sort.Strings(test.expectLeases)
			if !reflect.DeepEqual(leases, test.expectLeases) {
				t.Errorf("expected %v got: %v", test.expectLeases, leases)
			}
		})
	}
}
