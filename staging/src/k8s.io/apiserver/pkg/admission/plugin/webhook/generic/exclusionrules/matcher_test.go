package exclusionrules

import (
	"fmt"
	"testing"

	adreg "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authentication/user"
)

type exclusionRuleTest struct {
	exclusionRule ExclusionRule
	match         []admission.Attributes
	noMatch       []admission.Attributes
}
type tests map[string]exclusionRuleTest

func attrList(a ...admission.Attributes) []admission.Attributes {
	return a
}

func a(group, version, kind, namespace, name, username string) admission.Attributes {
	return admission.NewAttributesRecord(
		nil, nil,
		schema.GroupVersionKind{Group: group, Version: version, Kind: kind},
		namespace, name,
		schema.GroupVersionResource{Group: group, Version: version, Resource: ""}, "",
		"",
		nil,
		false,
		&user.DefaultInfo{
			Name: username,
		},
	)
}

func namespacedAttributes(group, version, resource, subresource, name string, operation admission.Operation, operationOptions runtime.Object) admission.Attributes {
	return admission.NewAttributesRecord(
		nil, nil,
		schema.GroupVersionKind{Group: group, Version: version, Kind: "k" + resource},
		"ns", name,
		schema.GroupVersionResource{Group: group, Version: version, Resource: resource}, subresource,
		operation,
		operationOptions,
		false,
		nil,
	)
}

func clusterScopedAttributes(group, version, resource, subresource, name string, operation admission.Operation, operationOptions runtime.Object) admission.Attributes {
	return admission.NewAttributesRecord(
		nil, nil,
		schema.GroupVersionKind{Group: group, Version: version, Kind: "k" + resource},
		"", name,
		schema.GroupVersionResource{Group: group, Version: version, Resource: resource}, subresource,
		operation,
		operationOptions,
		false,
		nil,
	)
}

func TestGroup(t *testing.T) {
	table := tests{
		"wildcard": {
			exclusionRule: ExclusionRule{
				APIGroup: "*",
			},
			noMatch: attrList(
				a("g1", "v1", "Lease", "test", "kube-scheduler", "test:user"),
			),
		},
		"exact": {
			exclusionRule: ExclusionRule{
				APIGroup: "g1",
			},
			match: attrList(
				a("g1", "v", "r", "", "name", "test:user"),
				a("g1", "v2", "r3", "", "name", "test:user"),
			),
			noMatch: attrList(
				a("g3", "v", "r", "", "name", "test:user"),
				a("g4", "v", "r", "", "name", "test:user"),
			),
		},
	}

	for name, tt := range table {
		for i, m := range tt.match {
			t.Run(fmt.Sprintf("%s_match_%d", name, i), func(t *testing.T) {
				r := Matcher{tt.exclusionRule, m}
				if !r.group() {
					t.Errorf("%v: expected match %#v", name, m)
				}
			})
		}
		for i, m := range tt.noMatch {
			t.Run(fmt.Sprintf("%s_match_%d", name, i), func(t *testing.T) {
				r := Matcher{tt.exclusionRule, m}
				if r.group() {
					t.Errorf("%v: expected match %#v", name, m)
				}
			})
		}
	}
}

func TestVersion(t *testing.T) {
	table := tests{
		"wildcard": {
			exclusionRule: ExclusionRule{
				APIVersion: "*",
			},
			match: attrList(
				a("g1", "v1", "Lease", "", "kube-scheduler", "test:user"),
			),
		},
		"exact": {
			exclusionRule: ExclusionRule{
				APIVersion: "v1",
			},
			match: attrList(
				a("g1", "v1", "r", "", "name", "test:user"),
				a("g2", "v1", "r", "", "name", "test:user"),
			),
			noMatch: attrList(
				a("g1", "v3", "r", "", "name", "test:user"),
				a("g2", "v4", "r", "", "name", "test:user"),
			),
		},
	}
	for name, tt := range table {
		for i, m := range tt.match {
			t.Run(fmt.Sprintf("%s_match_%d", name, i), func(t *testing.T) {
				r := Matcher{tt.exclusionRule, m}
				if !r.version() {
					t.Errorf("%v: expected match %#v", name, m)
				}
			})
		}
		for i, m := range tt.noMatch {
			t.Run(fmt.Sprintf("%s_match_%d", name, i), func(t *testing.T) {
				r := Matcher{tt.exclusionRule, m}
				if r.version() {
					t.Errorf("%v: expected match %#v", name, m)
				}
			})
		}
	}
}

func TestKind(t *testing.T) {
	table := tests{
		"wildcard": {
			exclusionRule: ExclusionRule{
				Kind: "*",
			},
			noMatch: attrList(
				a("g1", "v1", "Lease", "test", "kube-scheduler", "test:user"),
			),
		},
		"exact": {
			exclusionRule: ExclusionRule{
				Kind: "Lease",
			},
			match: attrList(
				a("g1", "v1", "Lease", "", "name", "test:user"),
				a("g2", "v2", "Lease", "", "name", "test:user"),
			),
			noMatch: attrList(
				a("g1", "v3", "Deployment", "", "name", "test:user"),
				a("g2", "v4", "Pod", "", "name", "test:user"),
			),
		},
	}
	for name, tt := range table {
		for i, m := range tt.match {
			t.Run(fmt.Sprintf("%s_match_%d", name, i), func(t *testing.T) {
				r := Matcher{tt.exclusionRule, m}
				if !r.kind() {
					t.Errorf("%v: expected match %#v", name, m)
				}
			})
		}
		for i, m := range tt.noMatch {
			t.Run(fmt.Sprintf("%s_match_%d", name, i), func(t *testing.T) {
				r := Matcher{tt.exclusionRule, m}
				if r.kind() {
					t.Errorf("%v: expected match %#v", name, m)
				}
			})
		}
	}
}

func TestName(t *testing.T) {
	table := tests{
		"wildcard": {
			exclusionRule: ExclusionRule{
				Name: []string{"*"},
			},
			match: attrList(
				a("g1", "v1", "Lease", "", "kube-scheduler", "test:user"),
			),
		},
		"exact": {
			exclusionRule: ExclusionRule{
				Name: []string{"kube-scheduler"},
			},
			match: attrList(
				a("g1", "v1", "Lease", "", "kube-scheduler", "test:user"),
				a("g2", "v2", "Lease", "", "kube-scheduler", "test:user"),
			),
			noMatch: attrList(
				a("g1", "v3", "Deployment", "", "something", "test:user"),
				a("g2", "v4", "Pod", "", "else", "test:user"),
			),
		},
	}
	for name, tt := range table {
		for i, m := range tt.match {
			t.Run(fmt.Sprintf("%s_match_%d", name, i), func(t *testing.T) {
				r := Matcher{tt.exclusionRule, m}
				if !r.name() {
					t.Errorf("%v: expected match %#v", name, m)
				}
			})
		}
		for i, m := range tt.noMatch {
			t.Run(fmt.Sprintf("%s_match_%d", name, i), func(t *testing.T) {
				r := Matcher{tt.exclusionRule, m}
				if r.name() {
					t.Errorf("%v: expected match %#v", name, m)
				}
			})
		}
	}
}

func TestUserName(t *testing.T) {
	table := tests{
		"wildcard": {
			exclusionRule: ExclusionRule{
				Username: "*",
			},
			match: attrList(
				a("g1", "v1", "Lease", "", "kube-scheduler", "test:user"),
			),
		},
		"exact": {
			exclusionRule: ExclusionRule{
				Username: "exempt:user",
			},
			match: attrList(
				a("g1", "v1", "Lease", "", "kube-scheduler", "exempt:user"),
				a("g2", "v2", "Lease", "", "kube-scheduler", "exempt:user"),
			),
			noMatch: attrList(
				a("g1", "v3", "Deployment", "", "something", "test:user1"),
				a("g2", "v4", "Pod", "", "else", "test:user2"),
			),
		},
	}
	for name, tt := range table {
		for i, m := range tt.match {
			t.Run(fmt.Sprintf("%s_match_%d", name, i), func(t *testing.T) {
				r := Matcher{tt.exclusionRule, m}
				if !r.username() {
					t.Errorf("%v: expected match %#v", name, m)
				}
			})
		}
		for i, m := range tt.noMatch {
			t.Run(fmt.Sprintf("%s_match_%d", name, i), func(t *testing.T) {
				r := Matcher{tt.exclusionRule, m}
				if r.username() {
					t.Errorf("%v: expected match %#v", name, m)
				}
			})
		}
	}
}

func TestNamespace(t *testing.T) {
	table := tests{
		"wildcard": {
			exclusionRule: ExclusionRule{
				Namespace: "*",
			},
			noMatch: attrList(
				a("g1", "v1", "Lease", "test", "kube-scheduler", "test:user"),
			),
		},
		"exact": {
			exclusionRule: ExclusionRule{
				Namespace: "kube-system",
			},
			match: attrList(
				a("g1", "v1", "Lease", "kube-system", "", "test:user"),
				a("g2", "v2", "Endpoint", "kube-system", "", "test:user"),
			),
			noMatch: attrList(
				a("g1", "v3", "Deployment", "something", "something", "test:user"),
				a("g2", "v4", "Pod", "else", "else", "test:user"),
			),
		},
	}
	for name, tt := range table {
		for i, m := range tt.match {
			t.Run(fmt.Sprintf("%s_match_%d", name, i), func(t *testing.T) {
				r := Matcher{tt.exclusionRule, m}
				if !r.namespace() {
					t.Errorf("%v: expected match %#v", name, m)
				}
			})
		}
		for i, m := range tt.noMatch {
			t.Run(fmt.Sprintf("%s_match_%d", name, i), func(t *testing.T) {
				r := Matcher{tt.exclusionRule, m}
				if r.namespace() {
					t.Errorf("%v: expected match %#v", name, m)
				}
			})
		}
	}
}

func TestScope(t *testing.T) {
	cluster := adreg.ClusterScope
	namespace := adreg.NamespacedScope
	allscopes := adreg.AllScopes
	table := tests{
		"cluster scope": {
			exclusionRule: ExclusionRule{
				Scope: &cluster,
			},
			match: attrList(
				clusterScopedAttributes("g", "v", "r", "", "name", admission.Create, &metav1.CreateOptions{}),
				clusterScopedAttributes("g", "v", "r", "exec", "name", admission.Create, &metav1.CreateOptions{}),
				clusterScopedAttributes("", "v1", "namespaces", "", "ns", admission.Create, &metav1.CreateOptions{}),
				clusterScopedAttributes("", "v1", "namespaces", "finalize", "ns", admission.Create, &metav1.CreateOptions{}),
				namespacedAttributes("", "v1", "namespaces", "", "ns", admission.Create, &metav1.CreateOptions{}),
				namespacedAttributes("", "v1", "namespaces", "finalize", "ns", admission.Create, &metav1.CreateOptions{}),
			),
			noMatch: attrList(
				namespacedAttributes("g", "v", "r", "", "name", admission.Create, &metav1.CreateOptions{}),
				namespacedAttributes("g", "v", "r", "exec", "name", admission.Create, &metav1.CreateOptions{}),
			),
		},
		"namespace scope": {
			exclusionRule: ExclusionRule{
				Scope: &namespace,
			},
			match: attrList(
				namespacedAttributes("g", "v", "r", "", "name", admission.Create, &metav1.CreateOptions{}),
				namespacedAttributes("g", "v", "r", "exec", "name", admission.Create, &metav1.CreateOptions{}),
			),
			noMatch: attrList(
				clusterScopedAttributes("", "v1", "namespaces", "", "ns", admission.Create, &metav1.CreateOptions{}),
				clusterScopedAttributes("", "v1", "namespaces", "finalize", "ns", admission.Create, &metav1.CreateOptions{}),
				namespacedAttributes("", "v1", "namespaces", "", "ns", admission.Create, &metav1.CreateOptions{}),
				namespacedAttributes("", "v1", "namespaces", "finalize", "ns", admission.Create, &metav1.CreateOptions{}),
				clusterScopedAttributes("g", "v", "r", "", "name", admission.Create, &metav1.CreateOptions{}),
				clusterScopedAttributes("g", "v", "r", "exec", "name", admission.Create, &metav1.CreateOptions{}),
			),
		},
		"all scopes": {
			exclusionRule: ExclusionRule{
				Scope: &allscopes,
			},
			noMatch: attrList(
				namespacedAttributes("g", "v", "r", "", "name", admission.Create, &metav1.CreateOptions{}),
				namespacedAttributes("g", "v", "r", "exec", "name", admission.Create, &metav1.CreateOptions{}),
				clusterScopedAttributes("g", "v", "r", "", "name", admission.Create, &metav1.CreateOptions{}),
				clusterScopedAttributes("g", "v", "r", "exec", "name", admission.Create, &metav1.CreateOptions{}),
				clusterScopedAttributes("", "v1", "namespaces", "", "ns", admission.Create, &metav1.CreateOptions{}),
				clusterScopedAttributes("", "v1", "namespaces", "finalize", "ns", admission.Create, &metav1.CreateOptions{}),
				namespacedAttributes("", "v1", "namespaces", "", "ns", admission.Create, &metav1.CreateOptions{}),
				namespacedAttributes("", "v1", "namespaces", "finalize", "ns", admission.Create, &metav1.CreateOptions{}),
			),
			match: attrList(),
		},
	}
	keys := sets.NewString()
	for name := range table {
		keys.Insert(name)
	}
	for _, name := range keys.List() {
		tt := table[name]
		for i, m := range tt.match {
			t.Run(fmt.Sprintf("%s_match_%d", name, i), func(t *testing.T) {
				r := Matcher{tt.exclusionRule, m}
				if !r.scope() {
					t.Errorf("%v: expected match %#v", name, m)
				}
			})
		}
		for i, m := range tt.noMatch {
			t.Run(fmt.Sprintf("%s_nomatch_%d", name, i), func(t *testing.T) {
				r := Matcher{tt.exclusionRule, m}
				if r.scope() {
					t.Errorf("%v: expected no match %#v", name, m)
				}
			})
		}
	}
}
