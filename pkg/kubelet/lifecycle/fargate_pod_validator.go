package lifecycle

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	fargateSchedulerName = "fargate-scheduler"
)

// Linux capabilities permitted in container security contexts.
// Copied from https://github.com/containerd/containerd/blob/257a7498d00827fbca08078f664cc6b4be27d7aa/oci/spec.go#L93
var permittedCaps = map[string]bool{
	"AUDIT_WRITE":      true,
	"CHOWN":            true,
	"DAC_OVERRIDE":     true,
	"FOWNER":           true,
	"FSETID":           true,
	"KILL":             true,
	"MKNOD":            true,
	"NET_BIND_SERVICE": true,
	"NET_RAW":          true,
	"SETFCAP":          true,
	"SETGID":           true,
	"SETPCAP":          true,
	"SETUID":           true,
	"SYS_CHROOT":       true,
}

var advancedPermittedCaps = map[string]bool{
	"IPC_LOCK":        true,
	"LINUX_IMMUTABLE": true,
	"SYS_PTRACE":      true,
	"SYS_RESOURCE":    true,
}

// Err towards more retries here since the node_authorizer/kubelet race causes a terminal failure starting the Fargate pod
var kubeApiGetRetries = wait.Backoff{
	Steps:    20,
	Duration: 10 * time.Millisecond,
	Factor:   5.0,
	Jitter:   0.1,
	Cap:      2 * time.Minute,
}

type validationFuncs func(*corev1.Pod) (bool, string)

// PodValidator validates pods to be launched on Fargate.
type PodValidator interface {
	Validate(*corev1.Pod) error
	ValidateContainer(container *corev1.Container) (bool, error)
}

type podSpecValidator struct {
	clientset.Interface
}

// NewPodSpecValidator returns a PodValidator.
func NewPodSpecValidator(client clientset.Interface) PodValidator {
	return &podSpecValidator{client}
}

// Validate checks if the pod is eligible to run on Fargate.
func (v *podSpecValidator) Validate(pod *corev1.Pod) error {
	var messages []string

	// Run through all validators to communicate all violations.
	validators := []validationFuncs{
		validateSchedulerName,
		validateOwnerReferences,
		validateTopLevelFields,
		v.validateVolumes,
		validateSecurityContexts,
		validateVolumeDevices,
		validatePorts,
	}

	for _, fn := range validators {
		admit, message := fn(pod)
		if !admit {
			messages = append(messages, message)
		}
	}

	// All validators must pass for the pod to be admitted.
	var err error
	if len(messages) != 0 {
		err = fmt.Errorf("Pod not supported: %s", strings.Join(messages, ", "))
	}

	return err
}

func validateSchedulerName(pod *corev1.Pod) (bool, string) {
	// Scheduler name must be Fargate.
	if pod.Spec.SchedulerName != fargateSchedulerName {
		return false, fmt.Sprintf("SchedulerName is not %s", fargateSchedulerName)
	}
	return true, ""
}

func validateOwnerReferences(pod *corev1.Pod) (bool, string) {
	ownerReferences := pod.ObjectMeta.OwnerReferences
	if len(ownerReferences) > 0 {
		for _, reference := range ownerReferences {
			if reference.Kind == "DaemonSet" {
				return false, "DaemonSet not supported"
			}
		}
	}
	return true, ""
}

func validateSecurityContexts(pod *corev1.Pod) (bool, string) {
	var invalidFields []string

	for _, container := range pod.Spec.InitContainers {
		admitted, message := validateContainerSecurityContext(container.SecurityContext)
		if !admitted {
			invalidFields = append(invalidFields, message)
		}
	}
	for _, container := range pod.Spec.Containers {
		admitted, message := validateContainerSecurityContext(container.SecurityContext)
		if !admitted {
			invalidFields = append(invalidFields, message)
		}
	}

	admitted, message := validatePodSecurityContext(pod.Spec.SecurityContext)
	if !admitted {
		invalidFields = append(invalidFields, message)
	}

	if len(invalidFields) != 0 {
		message := fmt.Sprintf("invalid SecurityContext fields: %s", strings.Join(invalidFields, ","))
		return false, message
	}
	return true, ""
}

// placeholder method. All PodSecurityContext fields are safe for warmpool fargate
// at launch, but we may need to add restrictions based on what is added here later
func validatePodSecurityContext(sc *corev1.PodSecurityContext) (bool, string) {
	return true, ""
}

// Allow ambient capabilities to be added. This is useful if only one or more are desired
// while the rest are dropped. Ex:
//
//	securityContext:
//	  allowPrivilegeEscalation: false
//	  capabilities:
//	    add:
//	    - NET_BIND_SERVICE
//	    drop:
//	    - all
func validateAddedCapabilities(requested []corev1.Capability) (permitted bool, rejectedCaps []string) {
	currentPermittedCaps := getPermittedCapsWithAdvancedCaps()
	fmt.Println("Currently permitted linux capabilities:", currentPermittedCaps)

	for _, req := range requested {
		if accept, _ := currentPermittedCaps[string(req)]; !accept {
			rejectedCaps = append(rejectedCaps, string(req))
		}
	}
	if len(rejectedCaps) != 0 {
		return false, rejectedCaps
	}
	return true, nil
}

func getPermittedCapsWithAdvancedCaps() map[string]bool {
	allowedCaps := map[string]bool{}
	for key, value := range permittedCaps {
		allowedCaps[key] = value
	}
	if os.Getenv("ADVANCED_LINUX_CAPS") == "true" {
		for key, value := range advancedPermittedCaps {
			allowedCaps[key] = value
		}
	}
	if os.Getenv("BLOCKED_LINUX_CAPS") != "" {
		blockedCaps := strings.Split(os.Getenv("BLOCKED_LINUX_CAPS"), ",")
		for _, blockedCap := range blockedCaps {
			allowedCaps[blockedCap] = false
		}
	}
	return allowedCaps
}

func validateContainerSecurityContext(sc *corev1.SecurityContext) (bool, string) {
	var invalidFields []string

	if sc == nil {
		return true, ""
	}

	if sc.Capabilities != nil && len(sc.Capabilities.Add) != 0 {
		admit, rejectedCaps := validateAddedCapabilities(sc.Capabilities.Add)
		if !admit {
			invalidFields = append(invalidFields, fmt.Sprintf("Capabilities added: %s", strings.Join(rejectedCaps, ", ")))
		}
	}

	if sc.AllowPrivilegeEscalation != nil && *sc.AllowPrivilegeEscalation == true {
		invalidFields = append(invalidFields, "AllowPrivilegeEscalation")
	}

	if sc.Privileged != nil && *sc.Privileged == true {
		invalidFields = append(invalidFields, "Privileged")
	}

	if len(invalidFields) != 0 {
		return false, strings.Join(invalidFields, ", ")
	}
	return true, ""
}

func validateTopLevelFields(pod *corev1.Pod) (bool, string) {
	var invalidFields []string

	if pod.Spec.HostNetwork == true {
		invalidFields = append(invalidFields, "HostNetwork")
	}
	if pod.Spec.HostPID == true {
		invalidFields = append(invalidFields, "HostPID")
	}
	if pod.Spec.HostIPC == true {
		invalidFields = append(invalidFields, "HostIPC")
	}

	if len(invalidFields) != 0 {
		message := fmt.Sprintf("fields not supported: %s", strings.Join(invalidFields, ", "))
		return false, message
	}
	return true, ""
}

func (v *podSpecValidator) validateVolumes(pod *corev1.Pod) (bool, string) {
	var invalidVolumes []string
	namespace := pod.Namespace

	for _, volume := range pod.Spec.Volumes {
		if volume.EmptyDir == nil &&
			volume.Secret == nil &&
			volume.ConfigMap == nil &&
			volume.Projected == nil &&
			volume.DownwardAPI == nil &&
			volume.NFS == nil &&
			volume.PersistentVolumeClaim == nil {
			message := fmt.Sprintf("%v is of an unsupported volume Type", volume.Name)
			invalidVolumes = append(invalidVolumes, message)
			continue
		}
		if volume.PersistentVolumeClaim != nil {
			validpvc, err := v.validatePersistentVolumeClaim(volume.PersistentVolumeClaim, namespace)
			if err != nil || !validpvc {
				invalidVolumes = append(invalidVolumes, fmt.Sprintf("%v not supported because: %v", volume.Name, err))
			}
		}
	}

	if len(invalidVolumes) != 0 {
		message := fmt.Sprintf("%s", strings.Join(invalidVolumes, ", "))
		return false, message
	}
	return true, ""
}

func (v *podSpecValidator) validatePersistentVolumeClaim(claim *corev1.PersistentVolumeClaimVolumeSource, namespace string) (bool, error) {
	shouldRetryKubeApiGet := func(err error) bool {
		if errors.IsInvalid(err) ||
			errors.IsGone(err) ||
			errors.IsNotAcceptable(err) ||
			errors.IsNotFound(err) ||
			errors.IsBadRequest(err) {
			return false
		}
		// attempt retries on other errors, especially Unauthorized/Forbidden errors due to node_authorizer/Kubelet race
		// https://github.com/kubernetes/kubernetes/pull/87696
		return true
	}
	var pvc *corev1.PersistentVolumeClaim
	var pv *corev1.PersistentVolume
	err := retry.OnError(kubeApiGetRetries, shouldRetryKubeApiGet, func() (err error) {
		name := claim.ClaimName
		ctx := context.TODO()
		pvc, err = v.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		pv, err = v.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return isValidPVC(pvc, pv)
}

func isValidPVC(pvc *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume) (bool, error) {
	// only PVCs that are bound to an EFS CSI Driver PV are allowed as of now
	if pvc.Status.Phase != corev1.ClaimBound {
		return false, fmt.Errorf("PVC %v not bound", pvc.Name)
	}
	if pv.Spec.CSI == nil {
		return false, fmt.Errorf("PVC %v bound to invalid PV %v with nil CSI spec", pvc.Name, pv.Name)
	}
	if pv.Spec.CSI.Driver != "efs.csi.aws.com" {
		return false, fmt.Errorf("PVC %v bound to invalid PV %v with CSI Driver: %v", pvc.Name, pv.Name, pv.Spec.CSI.Driver)
	}
	return true, nil
}

func validateVolumeDevices(pod *corev1.Pod) (bool, string) {
	var invalidContainers []string

	for _, container := range pod.Spec.InitContainers {
		if len(container.VolumeDevices) > 0 {
			invalidContainers = append(invalidContainers, container.Name)
		}
	}
	for _, container := range pod.Spec.Containers {
		if len(container.VolumeDevices) > 0 {
			invalidContainers = append(invalidContainers, container.Name)
		}
	}
	if len(invalidContainers) > 0 {
		return false, "volumeDevices not supported"
	}
	return true, ""
}

func validatePort(port corev1.ContainerPort) bool {
	if port.HostPort > 0 {
		return false
	}
	if port.HostIP != "" {
		return false
	}
	return true
}

// TODO: return more specific port violation messages later.
func validatePorts(pod *corev1.Pod) (bool, string) {
	message := "port contains HostIP or HostPort"

	for _, container := range pod.Spec.InitContainers {
		for _, port := range container.Ports {
			if !validatePort(port) {
				return false, message
			}
		}
	}
	for _, container := range pod.Spec.Containers {
		for _, port := range container.Ports {
			if !validatePort(port) {
				return false, message
			}
		}
	}
	return true, ""
}

func (v *podSpecValidator) ValidateContainer(container *corev1.Container) (bool, error) {
	// validate securityContext
	admitted, message := validateContainerSecurityContext(container.SecurityContext)
	if !admitted {
		return false, fmt.Errorf("invalid SecurityContext fields: %s", message)
	}

	// validate volumeDevices
	if len(container.VolumeDevices) > 0 {
		return false, fmt.Errorf("volumeDevices not supported")
	}

	// validate ports, currently should be no-op for ephemeral containers
	// since fields such as ports, livenessProbe, readinessProbe are disallowed on ephemeral containers
	// https://kubernetes.io/docs/concepts/workloads/pods/ephemeral-containers
	for _, port := range container.Ports {
		if !validatePort(port) {
			return false, fmt.Errorf("port contains HostIP or HostPort")
		}
	}

	return true, nil
}
