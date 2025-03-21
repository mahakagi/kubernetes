/*
Copyright 2018 The Kubernetes Authors.

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

package apiserver

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/pkg/apis/core/validation"
	"k8s.io/kubernetes/test/integration/framework"
)

// TestProxyCidrAllowlist tests the --proxy-cidr-allowlist flag added by an eks
// patch. The proxy subresource exists for nodes, pods, and services. Requests
// for this subresource are effectively requests for apiserver to connect to
// the given node/pod/service and act as a proxy for the client. apiserver must
// not connect to IPs outside the allowlist.
// This test should be ignorant of implementation details but they're explained
// below in case they're useful for navigating the patch, understanding the
// test and/or debugging failures.
// For node proxy subresource requests, whether apiserver is allowed to connect
// to and proxy the location should be dictated by:
//  1. IsProxyableHostname
//  2. --proxy-cidr-allowlist
//
// For pods, by:
//  1. IsProxyableIP
//  2. --proxy-cidr-allowlist
//
// For services, by:
//  1. ValidateNonSpecialIP
//  2. --proxy-cidr-allowlist
//
// IsProxyableHostname, IsProxyableIP, and ValidateNonSpecialIP should filter
// out special IPs like loopback and link-local regardless of the value of the
// allowlist.
// Then the ResourceLocation implementations of node, pod, and service RESTs in
// pkg/registry/core/node, pkg/registry/core/pod, pkg/registry/core/service
// should respect the allowlist by returning a proxy handler http.Transport
// that respects the allowlist (see CreateOutboundDialer).
// However there is a case in the node ResourceLocation implementation where it
// may not return a proxy handler if the port requested is the same as
// kubelet's (default 10250). Ultimately the returned handler is still wrapped
// in another proxy handler that doesn't know to respect the allowlist. So the
// function should check the allowlist before returning. Test cases cover both
// the "same port" (nodePort is not specified) and "different port" (nodePort
// is specified to something not equal to 10250, 10251).
func TestProxyCidrAllowlist(t *testing.T) {
	var testcases = []struct {
		name               string
		proxyCidrAllowlist string
		ip                 string
		nodePort           int
		// These tests rely on assertProxyAllowed and proxyAllowed (see their
		// comments below) to translate errors from proxy requests into explicit
		// true/false allow/deny decisions. In short, these tests do not bother
		// testing for error messages (e.g. expectError=nil or expectError="address
		// is not allowed") because they can't, so they only test for allow/deny.
		expectNodeProxyAllowed    bool
		expectPodProxyAllowed     bool
		expectServiceProxyAllowed bool
	}{
		{
			name:                      "allowlist empty, localhost blocked",
			proxyCidrAllowlist:        "",
			ip:                        "127.0.0.1",
			expectNodeProxyAllowed:    false,
			expectPodProxyAllowed:     false,
			expectServiceProxyAllowed: false,
		},
		{
			name:                      "allowlist contains localhost, localhost blocked",
			proxyCidrAllowlist:        "127.0.0.1/32",
			ip:                        "127.0.0.1",
			expectNodeProxyAllowed:    false,
			expectPodProxyAllowed:     false,
			expectServiceProxyAllowed: false,
		},
		{
			name:                      "allowlist contains localhost, node port is non-default, localhost blocked",
			proxyCidrAllowlist:        "127.0.0.1/32",
			ip:                        "127.0.0.1",
			nodePort:                  10251,
			expectNodeProxyAllowed:    false,
			expectPodProxyAllowed:     false,
			expectServiceProxyAllowed: false,
		},
		{
			name:                      "allowlist omits localhost, localhost blocked",
			proxyCidrAllowlist:        "198.51.100.0/24",
			ip:                        "127.0.0.1",
			expectNodeProxyAllowed:    false,
			expectPodProxyAllowed:     false,
			expectServiceProxyAllowed: false,
		},
		{
			name:                      "allowlist omits localhost, node port is non-default, localhost blocked",
			proxyCidrAllowlist:        "198.51.100.0/24",
			ip:                        "127.0.0.1",
			nodePort:                  10251,
			expectNodeProxyAllowed:    false,
			expectPodProxyAllowed:     false,
			expectServiceProxyAllowed: false,
		},
		{
			name:                      "allowlist empty, 192.0.2.0 allowed",
			proxyCidrAllowlist:        "",
			ip:                        "192.0.2.0",
			expectNodeProxyAllowed:    true,
			expectPodProxyAllowed:     true,
			expectServiceProxyAllowed: true,
		},
		{
			name:                      "allowlist contains 192.0.2.0, 192.0.2.0 allowed",
			proxyCidrAllowlist:        "198.51.100.0/24,192.0.2.0/24",
			ip:                        "192.0.2.0",
			expectNodeProxyAllowed:    true,
			expectPodProxyAllowed:     true,
			expectServiceProxyAllowed: true,
		},
		{
			name:                      "allowlist contains 192.0.2.0, node port is non-default, 192.0.2.0 allowed",
			proxyCidrAllowlist:        "198.51.100.0/24,192.0.2.0/24",
			ip:                        "192.0.2.0",
			nodePort:                  10251,
			expectNodeProxyAllowed:    true,
			expectPodProxyAllowed:     true,
			expectServiceProxyAllowed: true,
		},
		{
			name:                      "allowlist omits 192.0.2.0, 192.0.2.0 blocked",
			proxyCidrAllowlist:        "198.51.100.0/24",
			ip:                        "192.0.2.0",
			expectNodeProxyAllowed:    false,
			expectPodProxyAllowed:     false,
			expectServiceProxyAllowed: false,
		},
		{
			name:                      "allowlist omits 192.0.2.0, node port is non-default, 192.0.2.0 blocked",
			proxyCidrAllowlist:        "198.51.100.0/24",
			ip:                        "192.0.2.0",
			nodePort:                  10251,
			expectNodeProxyAllowed:    false,
			expectPodProxyAllowed:     false,
			expectServiceProxyAllowed: false,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			apiserver, clientset := startTestServerOrDie(t, framework.SharedEtcd(), tc.proxyCidrAllowlist)
			defer apiserver.TearDownFn()

			node := createNodeOrDie(t, clientset, tc.ip)
			ctx, cancel := context.WithTimeout(context.TODO(), 100*time.Millisecond)
			defer cancel()
			nodeName := node.GetName()
			if tc.nodePort != 0 {
				// https://kubernetes.io/docs/tasks/access-application-cluster/access-cluster/#manually-constructing-apiserver-proxy-urls
				nodeName += ":" + strconv.Itoa(tc.nodePort)
			}
			result := clientset.CoreV1().RESTClient().Get().Resource("nodes").Name(nodeName).SubResource("proxy").Do(ctx)
			assertProxyAllowed(t, "node", tc.ip, tc.expectNodeProxyAllowed, result)

			namespace := "ns"

			pod := createPodOrDie(t, clientset, tc.ip, namespace, node.Name)
			ctx, cancel = context.WithTimeout(context.TODO(), 100*time.Millisecond)
			defer cancel()
			result = clientset.CoreV1().RESTClient().Get().Namespace(namespace).Resource("pods").Name(pod.GetName()).SubResource("proxy").Do(ctx)
			assertProxyAllowed(t, "pod", tc.ip, tc.expectPodProxyAllowed, result)

			// Only test the service case if endpoints creation (in
			// createServiceOrDie) would get past ValidateNonSpecialIP. Otherwise
			// endpoints creation fails and there is nothing to test anyway.
			errs := validation.ValidateNonSpecialIP(tc.ip, field.NewPath("ip"))
			if len(errs) == 0 {
				service := createServiceOrDie(t, clientset, tc.ip, namespace, pod.Name)
				ctx, cancel = context.WithTimeout(context.TODO(), 100*time.Millisecond)
				defer cancel()
				result = clientset.CoreV1().RESTClient().Get().Namespace(namespace).Resource("services").Name(service.GetName()).SubResource("proxy").Do(ctx)
				assertProxyAllowed(t, "service", tc.ip, tc.expectServiceProxyAllowed, result)
			}

		})
	}
}

func assertProxyAllowed(t *testing.T, kind, ip string, expect bool, result restclient.Result) {
	allowed, err := proxyAllowed(result.Error())
	if err != nil {
		t.Errorf("error determining if apiserver proxy=%q ip=%q result=%q was allowed: %v", kind, ip, result.Error(), err)
	} else if allowed != expect {
		t.Errorf("expected apiserver proxy=%q ip=%q result=%q to be allowed=%v but got allowed=%v", kind, ip, result.Error(), expect, allowed)
	}
}

// proxyAllowed translates errors from proxy requests into explicit true/false
// decisions for whether apiserver was allowed to connect to and proxy the
// location or not. It is necessary because in this test environment we cannot
// simply say that err=nil means apiserver was allowed to connect and err!=nil
// means apiserver was denied. For example, attempts to proxy localhost will
// probably return "connection refused" when apiserver attempts to connect to
// it (unless you happen to be running a webserver in your test environment).
// In this case, err is not nil but apiserver obviously made a connection
// attempt, hence this returns true.
func proxyAllowed(err error) (bool, error) {
	if err == nil {
		// apiserver was allowed to connect, the server OK'd. This should not
		// happen in practice because there should be no server running at
		// 127.0.0.1 or 192.0.2.0
		return true, nil
	}

	if strings.Contains(err.Error(), "connection refused") {
		// apiserver was allowed to connect, the server (127.0.0.1) is just refusing
		return true, nil
	}

	if strings.Contains(err.Error(), "context deadline exceeded") {
		// Assume context time out to mean "true": the apiserver was allowed to
		// connect to and proxy the location, and was in the process of dialing it.
		// This assumption is needed because there is no easy way for this test to
		// verify apiserver really dialed the location. The test can't for example
		// start a local httptest server for apiserver to dial because IsProxyableIP
		// would filter it before the allowlist logic could get exercised. Context
		// time out could also mean "unknown": if time out is observed before the
		// allowlist logic got exercised, then what the allowlist logic would have
		// decided is unknown. But it's okay to treat "unknown" as "true" because it
		// means cases where the allowlist is too permissive are always caught.
		return true, nil
	}

	if strings.Contains(err.Error(), "address not allowed") {
		// IsProxyable denied apiserver and returned ErrAddressNotAllowed
		return false, nil
	}

	if strings.Contains(err.Error(), "Address is not allowed") {
		// Allowlist dialer denied apiserver
		return false, nil
	}

	return false, fmt.Errorf("unrecognized error %v", err)
}

func startTestServerOrDie(t *testing.T, etcd *storagebackend.Config, proxyCidrAllowlist string) (*kubeapiservertesting.TestServer, *kubernetes.Clientset) {
	proxyCidrAllowlistFlag := "--proxy-cidr-allowlist=" + proxyCidrAllowlist
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, []string{proxyCidrAllowlistFlag}, etcd)

	clientset, err := kubernetes.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatalf("error creating client: %v", err)
	}

	return server, clientset
}

func createNodeOrDie(t *testing.T, clientset *kubernetes.Clientset, ip string) *corev1.Node {
	node, err := clientset.CoreV1().Nodes().Create(context.TODO(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("error creating node: %v", err)
	}
	node.Status = corev1.NodeStatus{
		Addresses: []corev1.NodeAddress{
			{
				Type:    corev1.NodeExternalIP,
				Address: ip,
			},
		},
		DaemonEndpoints: corev1.NodeDaemonEndpoints{
			KubeletEndpoint: corev1.DaemonEndpoint{
				Port: int32(10250),
			},
		},
	}
	node, err = clientset.CoreV1().Nodes().UpdateStatus(context.TODO(), node, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("error updating node status: %v", err)
	}
	return node
}

func createPodOrDie(t *testing.T, clientset *kubernetes.Clientset, ip, namespace, nodeName string) *corev1.Pod {
	_, err := clientset.CoreV1().Namespaces().Create(context.TODO(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("error creating namespace: %v", err)
	}

	_, err = clientset.CoreV1().ServiceAccounts(namespace).Create(context.TODO(), &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: namespace},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("error creating serviceaccount: %v", err)
	}

	falseRef := false
	pod, err := clientset.CoreV1().Pods(namespace).Create(context.TODO(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: namespace},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "foo",
					Image: "some/image:latest",
				},
			},
			NodeName:                     nodeName,
			AutomountServiceAccountToken: &falseRef,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("error creating pod: %v", err)
	}
	pod.Status = corev1.PodStatus{PodIPs: []corev1.PodIP{{ip}}}
	pod, err = clientset.CoreV1().Pods(namespace).UpdateStatus(context.TODO(), pod, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("error updating pod status: %v", err)
	}
	return pod
}

func createServiceOrDie(t *testing.T, clientset *kubernetes.Clientset, ip, namespace, podName string) *corev1.Service {
	service, err := clientset.CoreV1().Services(namespace).Create(context.TODO(), &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service",
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Port: int32(80),
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("error creating service: %v", err)
	}

	_, err = clientset.CoreV1().Endpoints(namespace).Create(context.TODO(), &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service",
			Namespace: namespace,
		},
		Subsets: []corev1.EndpointSubset{{
			Ports: []corev1.EndpointPort{{
				Port: 80,
			}},
			Addresses: []corev1.EndpointAddress{{
				IP: ip,
				TargetRef: &corev1.ObjectReference{
					APIVersion: "v1",
					Kind:       "Pod",
					Namespace:  namespace,
					Name:       podName,
				},
			}},
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("error creating endpoints: %v", err)
	}
	return service
}
