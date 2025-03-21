package exclusionrules

import (
	v1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
)

var namespaceResource = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}

// Matcher determines if the Attr matches the Rule.
type Matcher struct {
	ExclusionRule ExclusionRule
	Attr          admission.Attributes
}

// Matches returns if the Attr matches the Rule.
func (r *Matcher) Matches() bool {
	return r.name() &&
		r.namespace() &&
		r.group() &&
		r.version() &&
		r.kind() &&
		r.username() &&
		r.scope()
}

func exactOrWildcardList(items []string, requested string) bool {
	for _, item := range items {
		if item == "*" {
			return true
		}
		if item == requested {
			return true
		}
	}

	return false
}

func exactOrWildcard(item string, requested string) bool {
	return exactOrWildcardList([]string{item}, requested)
}

func (r *Matcher) scope() bool {
	if r.ExclusionRule.Scope == nil || *r.ExclusionRule.Scope == v1.AllScopes {
		// Not valid
		return false
	}
	// attr.GetNamespace() is set to the name of the namespace for requests of the namespace object itself.
	switch *r.ExclusionRule.Scope {
	case v1.NamespacedScope:
		// first make sure that we are not requesting a namespace object (namespace objects are cluster-scoped)
		// this will return true for a resource that has a namespace and is not a Namespace itself
		// i.e. true for Deployment and Role
		//      false for Namespace and ClusterRole
		return r.Attr.GetResource() != namespaceResource && r.Attr.GetNamespace() != metav1.NamespaceNone
	case v1.ClusterScope:
		// also return true if the request is for a namespace object (namespace objects are cluster-scoped)
		// i.e. false for Deployment and Role
		//      true for Namespace and ClusterRole
		return r.Attr.GetResource() == namespaceResource || r.Attr.GetNamespace() == metav1.NamespaceNone
	default:
		return false
	}
}

func (r *Matcher) group() bool {
	return r.ExclusionRule.APIGroup == r.Attr.GetResource().Group
}

func (r *Matcher) name() bool {
	return exactOrWildcardList(r.ExclusionRule.Name, r.Attr.GetName())
}

func (r *Matcher) namespace() bool {
	return r.ExclusionRule.Namespace == r.Attr.GetNamespace()
}

func (r *Matcher) version() bool {
	return exactOrWildcard(r.ExclusionRule.APIVersion, r.Attr.GetResource().Version)
}

func (r *Matcher) kind() bool {
	return r.ExclusionRule.Kind == r.Attr.GetKind().Kind
}

func (r *Matcher) username() bool {
	return exactOrWildcard(r.ExclusionRule.Username, r.Attr.GetUserInfo().GetName())
}
