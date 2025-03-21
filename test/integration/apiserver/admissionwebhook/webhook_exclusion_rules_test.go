package admissionwebhook

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/generic"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/generic/exclusionrules"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
)

var (
	exclusionRulesFile           = "exclusion-rules-config.json"
	webhookNameExclusionRules    = "integration-exclusion-rules-test-webhook-config"
	deploymentNameExclusionRules = "integration-exclusion-rules-test-deployment"
	kubeSystem                   = "kube-system"
	kubeControllerManager        = "kube-controller-manager"
	kubeScheduler                = "kube-scheduler"
)

func TestWebhookExclusionRulesNoEnvVarSet(t *testing.T) {
	generic.LoadCriticalPathExcluder = new(sync.Once) //reset sync.Once to force behavior of new startup https://github.com/golang/go/issues/25955#issuecomment-398278056
	t.Logf("starting server")
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, nil, framework.SharedEtcd())
	defer server.TearDownFn()

	client, err := kubernetes.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	createBrokenWebhook(t, client)
	// Creating deployment that should be blocked by webhook
	_, err = client.AppsV1().Deployments("default").Create(context.TODO(), exampleDeploymentExclusionRules(deploymentNameExclusionRules), metav1.CreateOptions{})
	if err == nil {
		t.Fatalf("was able to create deployment but should've been blocked: %v", err)
	}
	// Default rules should be set
	validateDefaultExclusionRulesSet(t, client)
}

func TestWebhookExclusionRulesEnvVarSetNoFile(t *testing.T) {
	generic.LoadCriticalPathExcluder = new(sync.Once) //reset sync.Once to force behavior of new startup https://github.com/golang/go/issues/25955#issuecomment-398278056
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, nil, framework.SharedEtcd())
	defer server.TearDownFn()

	client, err := kubernetes.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Test env var set but no file, should be broken webhook
	err = os.Setenv(exclusionrules.ADMISSION_WEBHOOK_EXCLUSION_ENV_VAR, exclusionRulesFile)
	if err != nil {
		t.Fatalf("unexpected error clearing %v env var", exclusionrules.ADMISSION_WEBHOOK_EXCLUSION_ENV_VAR)
	}

	createBrokenWebhook(t, client)

	//Creating deployment that should be blocked by webhook
	_, err = client.AppsV1().Deployments("default").Create(context.TODO(), exampleDeploymentExclusionRules(deploymentNameExclusionRules), metav1.CreateOptions{})
	if err == nil {
		t.Fatalf("was able to create deployment but should've been blocked: %v", err)
	}
	// Default rules should be set
	validateDefaultExclusionRulesSet(t, client)
}

func TestWebhookExclusionRulesEnvVarSetBadFile(t *testing.T) {
	generic.LoadCriticalPathExcluder = new(sync.Once) //reset sync.Once to force behavior of new startup https://github.com/golang/go/issues/25955#issuecomment-398278056
	// Test env var set, bad file, should be broken webhook
	err := os.Setenv(exclusionrules.ADMISSION_WEBHOOK_EXCLUSION_ENV_VAR, exclusionRulesFile)
	if err != nil {
		t.Fatalf("unexpected error clearing %v env var", exclusionrules.ADMISSION_WEBHOOK_EXCLUSION_ENV_VAR)
	}

	if err := os.WriteFile(exclusionRulesFile, []byte("bad file"), os.FileMode(0755)); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(exclusionRulesFile)

	server := kubeapiservertesting.StartTestServerOrDie(t, nil, nil, framework.SharedEtcd())
	defer server.TearDownFn()

	client, err := kubernetes.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	createBrokenWebhook(t, client)

	//Creating deployment that should be blocked by webhook
	_, err = client.AppsV1().Deployments("default").Create(context.TODO(), exampleDeploymentExclusionRules(deploymentNameExclusionRules), metav1.CreateOptions{})
	if err == nil {
		t.Fatalf("was able to create deployment but should've been blocked: %v", err)
	}
	// Default rules should be set
	validateDefaultExclusionRulesSet(t, client)
}

func TestWebhookExclusionRules(t *testing.T) {
	generic.LoadCriticalPathExcluder = new(sync.Once) //reset sync.Once to force behavior of new startup https://github.com/golang/go/issues/25955#issuecomment-398278056
	err := os.Setenv(exclusionrules.ADMISSION_WEBHOOK_EXCLUSION_ENV_VAR, exclusionRulesFile)
	if err != nil {
		t.Fatalf("unexpected error clearing %v env var", exclusionrules.ADMISSION_WEBHOOK_EXCLUSION_ENV_VAR)
	}

	//test env var set, exclusion file should exclude
	configFile := fmt.Sprintf(`
[
	{
		"apiGroup": "apps",
		"apiVersion": "v1",
		"kind": "Deployment",
		"namespace": "default",
		"name": ["%v"],
		"scope": "Namespaced",
		"username": "*"
	}
]`, deploymentNameExclusionRules)

	if err := os.WriteFile(exclusionRulesFile, []byte(configFile), os.FileMode(0755)); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(exclusionRulesFile)

	server := kubeapiservertesting.StartTestServerOrDie(t, nil, nil, framework.SharedEtcd())
	defer server.TearDownFn()

	client, err := kubernetes.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	createBrokenWebhook(t, client)

	t.Logf("Creating Deployment which should be allowed due to exclusion rules")
	_, err = client.AppsV1().Deployments("default").Create(context.TODO(), exampleDeploymentExclusionRules(deploymentNameExclusionRules), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create deployment: %v", err)
	}

	t.Logf("Creating Deployment with different name which should be blocked due to webhook")
	_, err = client.AppsV1().Deployments("default").Create(context.TODO(), exampleDeploymentExclusionRules("test-different-name"), metav1.CreateOptions{})
	if err == nil {
		t.Fatalf("was able to create deployment but should've been blocked: %v", err)
	}
}

func validateDefaultExclusionRulesSet(t *testing.T, client *kubernetes.Clientset) {
	_, err := client.CoreV1().Endpoints("kube-system").Create(context.TODO(), exampleEndpoints(kubeControllerManager), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("was not able to create endpoint but should've been allowed: %v", err)
	}
	_, err = client.CoreV1().Endpoints("kube-system").Create(context.TODO(), exampleEndpoints(kubeScheduler), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("was not able to create endpoint but should've been allowed: %v", err)
	}
	_, err = client.CoordinationV1().Leases("kube-system").Create(context.TODO(), exampleLease(kubeControllerManager), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("was not able to create lease but should've been allowed: %v", err)
	}
	_, err = client.CoordinationV1().Leases("kube-system").Create(context.TODO(), exampleLease(kubeScheduler), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("was not able to create lease but should've been allowed: %v", err)
	}
}

func createBrokenWebhook(t *testing.T, client *kubernetes.Clientset) {
	t.Logf("Creating Broken Webhook that will block all operations on all objects")
	brokenWebhook := brokenWebhookConfigExclusionRules(webhookNameExclusionRules)
	_, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(context.TODO(), brokenWebhook, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to register broken webhook: %v", err)
	}

	for i := 0; i < 10; i++ {
		time.Sleep(2 * time.Second)
		_, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.TODO(), brokenWebhook.Name, metav1.GetOptions{})
		if err == nil {
			t.Log("Successfully registered broken webhook")
			return
		}
	}
	t.Fatal("Timed out waiting for test bad webhook to create")
}

func exampleDeploymentExclusionRules(name string) *appsv1.Deployment {
	var replicas int32 = 1
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"foo": "bar"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"foo": "bar"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "foo",
							Image: "foo",
						},
					},
				},
			},
		},
	}
}

func exampleEndpoints(name string) *corev1.Endpoints {
	return &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: kubeSystem,
		},
	}
}

func exampleLease(name string) *coordinationv1.Lease {
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: kubeSystem,
		},
	}
}

func brokenWebhookConfigExclusionRules(name string) *admissionregistrationv1.ValidatingWebhookConfiguration {
	var path string
	failurePolicy := admissionregistrationv1.Fail
	return &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{
			{
				Name: "broken-webhook.k8s.io",
				Rules: []admissionregistrationv1.RuleWithOperations{{
					Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.OperationAll},
					Rule: admissionregistrationv1.Rule{
						APIGroups:   []string{"*"},
						APIVersions: []string{"*"},
						Resources:   []string{"deployments"},
					},
				}},
				// This client config references a non existent service
				// so it should always fail.
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					Service: &admissionregistrationv1.ServiceReference{
						Namespace: "default",
						Name:      "invalid-webhook-service",
						Path:      &path,
					},
					CABundle: nil,
				},
				FailurePolicy:           &failurePolicy,
				SideEffects:             &noSideEffects,
				AdmissionReviewVersions: []string{"v1"},
			},
		},
	}
}
