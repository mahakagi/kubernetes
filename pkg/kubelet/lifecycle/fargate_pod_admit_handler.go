package lifecycle

import (
	corev1 "k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
)

var (
	// unsupportedPodSpec is set on reason field for pods which cannot execute on this kubelet node
	unsupportedPodSpecMessage = "UnsupportedPodSpec"
)

func NewFargatePodAdmitHandler(client clientset.Interface) *fargatePodAdmitHandler {
	return &fargatePodAdmitHandler{
		podValidator: NewPodSpecValidator(client),
	}
}

// fargatePodAdmitHandler verifies security aspects of pod spec before admitting the pod.
type fargatePodAdmitHandler struct {
	podValidator PodValidator
}

// Admit checks security aspects of pod spec and decides whether pod spec is safe to run on this kubelet.
// Currently fargatePodAdmitHandler runs fixed set of validation to verify if pod can run on fargate kubelet.
func (f *fargatePodAdmitHandler) Admit(attrs *PodAdmitAttributes) PodAdmitResult {

	admit, message := f.validate(attrs.Pod)

	response := PodAdmitResult{
		Admit: admit,
	}
	if !admit {
		response.Message = message
		response.Reason = unsupportedPodSpecMessage
	}
	return response
}

func (f *fargatePodAdmitHandler) validate(pod *corev1.Pod) (bool, string) {
	err := f.podValidator.Validate(pod)
	if err != nil {
		return false, err.Error()
	}
	return true, ""
}
