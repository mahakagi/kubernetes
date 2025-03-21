package exclusionrules

import (
	"testing"

	v1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authentication/user"
)

func getAttributes() admission.Attributes {
	return admission.NewAttributesRecord(
		nil,
		nil,
		schema.GroupVersionKind{"apps", "v1", "Deployment"},
		"ns",
		"testName",
		schema.GroupVersionResource{"apps", "v1", "deployments"},
		"",
		admission.Create,
		&metav1.CreateOptions{},
		false,
		&user.DefaultInfo{
			Name: "exempt:user",
		},
	)
}

func TestShouldSkipWebhookDueToExclusionRules(t *testing.T) {
	namespace := v1.NamespacedScope
	testcases := []struct {
		name           string
		exclusionRules []ExclusionRule
		result         bool
		attr           admission.Attributes
	}{
		{
			name: "Matches attribute first exclusion rule",
			exclusionRules: []ExclusionRule{
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "",
					APIVersion: "v1",
					Kind:       "Configmap",
					Name:       []string{"mismatch"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
			},
			result: true,
			attr:   getAttributes(),
		},
		{
			name: "Matches attribute second exclusion rule",
			exclusionRules: []ExclusionRule{
				{
					APIGroup:   "",
					APIVersion: "v1",
					Kind:       "Configmap",
					Name:       []string{"mismatch"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
			},
			result: true,
			attr:   getAttributes(),
		},
		{
			name: "Does not match exclusion rules",
			exclusionRules: []ExclusionRule{
				{
					APIGroup:   "",
					APIVersion: "v1",
					Kind:       "Configmap",
					Name:       []string{"mismatch"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"mismatch"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
			},
			result: false,
			attr:   getAttributes(),
		},
		{
			name:           "No exclusion rules exist",
			exclusionRules: []ExclusionRule{},
			result:         false,
			attr:           getAttributes(),
		},
	}
	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			criticalPathExcluder := CriticalPathExcluder{
				exclusionRules: testcase.exclusionRules,
			}
			result := criticalPathExcluder.ShouldSkipWebhookDueToExclusionRules(testcase.attr)
			if result != testcase.result {
				t.Fatalf("Unexpected result %v for test case %v", result, testcase.name)
			}
		})
	}
}

func TestFilterValidRules(t *testing.T) {
	cluster := v1.ClusterScope
	namespace := v1.NamespacedScope
	allscopes := v1.AllScopes
	testcases := []struct {
		name                string
		inputRules          []ExclusionRule
		expectedOutputRules int
	}{
		{
			name: "Allowed missing APIGroup for group \"\"",
			inputRules: []ExclusionRule{
				{
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "test",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
			},
			expectedOutputRules: 2,
		},
		{
			name: "No missing APIVersion",
			inputRules: []ExclusionRule{
				{
					APIGroup:  "v1",
					Kind:      "Deployment",
					Name:      []string{"testName"},
					Namespace: "ns",
					Scope:     &namespace,
					Username:  "*",
				},
				{
					APIGroup:   "test",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
			},
			expectedOutputRules: 1,
		},
		{
			name: "No missing KIND",
			inputRules: []ExclusionRule{
				{
					APIGroup:   "v1",
					APIVersion: "v1",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "test",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
			},
			expectedOutputRules: 1,
		},
		{
			name: "No empty KIND",
			inputRules: []ExclusionRule{
				{
					APIGroup:   "v1",
					APIVersion: "v1",
					Kind:       "",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "test",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
			},
			expectedOutputRules: 1,
		},
		{
			name: "No missing Name",
			inputRules: []ExclusionRule{
				{
					APIGroup:   "v1",
					APIVersion: "v1",
					Kind:       "Deployment",
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "test",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
			},
			expectedOutputRules: 1,
		},
		{
			name: "No empty Name",
			inputRules: []ExclusionRule{
				{
					APIGroup:   "v1",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "test",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
			},
			expectedOutputRules: 1,
		},
		{
			name: "No missing username",
			inputRules: []ExclusionRule{
				{
					APIGroup:   "v1",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"Deployment"},
					Namespace:  "ns",
					Scope:      &namespace,
				},
				{
					APIGroup:   "test",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
			},
			expectedOutputRules: 1,
		},
		{
			name: "No wildcard for APIGroup",
			inputRules: []ExclusionRule{
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "*",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "*",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "exempt:user",
				},
			},
			expectedOutputRules: 1,
		},
		{
			name: "No wildcard for APIVersion",
			inputRules: []ExclusionRule{
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "apps",
					APIVersion: "*",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "apps",
					APIVersion: "*",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "exempt:user",
				},
			},
			expectedOutputRules: 2,
		},
		{
			name: "No wildcard for Kind",
			inputRules: []ExclusionRule{
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "*",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "*",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "exempt:user",
				},
			},
			expectedOutputRules: 1,
		},
		{
			name: "wildcard is allowed for Namespace iif eks owned username is defined",
			inputRules: []ExclusionRule{
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "*",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "*",
					Scope:      &namespace,
					Username:   "exempt:user",
				},
				{
					APIGroup:   "networking.k8s.aws",
					APIVersion: "v1",
					Kind:       "PolicyEndpoint",
					Name:       []string{"*"},
					Namespace:  "*",
					Scope:      &namespace,
					Username:   "eks:network-policy-controller",
				},
				{
					APIGroup:   "vpcresources.k8s.aws",
					APIVersion: "*",
					Kind:       "PolicyEndpoint",
					Name:       []string{"*"},
					Namespace:  "*",
					Scope:      &namespace,
					Username:   "eks:vpc-resource-controller",
				},
				{
					APIGroup:   "networking.k8s.aws",
					APIVersion: "*",
					Kind:       "PolicyEndpoint",
					Name:       []string{"*"},
					Namespace:  "*",
					Scope:      &namespace,
					Username:   "eks:tester",
				},
			},
			expectedOutputRules: 3,
		},
		{
			name: "No Empty Scope",
			inputRules: []ExclusionRule{
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Username:   "*",
				},
			},
			expectedOutputRules: 1,
		},
		{
			name: "No AllScopes",
			inputRules: []ExclusionRule{
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &allscopes,
					Username:   "*",
				},
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &allscopes,
					Username:   "exempt:user",
				},
			},
			expectedOutputRules: 1,
		},
		{
			name: "No Name wildcard, not lease",
			inputRules: []ExclusionRule{
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"*"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
			},
			expectedOutputRules: 1,
		},
		{
			name: "No Name wildcard, not kube-node-lease",
			inputRules: []ExclusionRule{
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "coordination.k8s.io",
					APIVersion: "v1",
					Kind:       "Lease",
					Name:       []string{"*"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
			},
			expectedOutputRules: 1,
		},
		{
			name: "No Name wildcard, not coordination.k8s.io",
			inputRules: []ExclusionRule{
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "wrong",
					APIVersion: "v1",
					Kind:       "Lease",
					Name:       []string{"*"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
			},
			expectedOutputRules: 1,
		},
		{
			name: "No Name wildcard, not v1",
			inputRules: []ExclusionRule{
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "coordination.k8s.io",
					APIVersion: "v1beta1",
					Kind:       "Lease",
					Name:       []string{"*"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
			},
			expectedOutputRules: 1,
		},
		{
			name: "Allowed name wildcard",
			inputRules: []ExclusionRule{
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "coordination.k8s.io",
					APIVersion: "v1",
					Kind:       "Lease",
					Name:       []string{"*"},
					Namespace:  "kube-node-lease",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "test",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"*"},
					Namespace:  "kube-node-lease",
					Scope:      &namespace,
					Username:   "exempt:user",
				},
			},
			expectedOutputRules: 3,
		},
		{
			name: "Cluster scoped does not allow namespace",
			inputRules: []ExclusionRule{
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &cluster,
					Username:   "*",
				},
			},
			expectedOutputRules: 1,
		},
		{
			name: "Namespaced scoped requires namespace",
			inputRules: []ExclusionRule{
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "ns",
					Scope:      &namespace,
					Username:   "*",
				},
				{
					APIGroup:   "apps",
					APIVersion: "v1",
					Kind:       "Deployment",
					Name:       []string{"testName"},
					Namespace:  "",
					Scope:      &namespace,
					Username:   "*",
				},
			},
			expectedOutputRules: 1,
		},
	}
	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			result := filterValidRules(testcase.inputRules)
			if len(result) != testcase.expectedOutputRules {
				t.Fatalf("Unexpected result length of filtered rules %v for test case %v", len(result), testcase.name)
			}
		})
	}
}
