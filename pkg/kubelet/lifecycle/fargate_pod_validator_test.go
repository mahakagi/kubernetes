package lifecycle

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

var fixtureDir = "./fargate_pod_validator_testdata"

const (
	admitPodAnnotation                    = "fargate.eks.amazonaws.com/admit"
	messagePodAnnotation                  = "fargate.eks.amazonaws.com/admitMessage"
	advancedLinuxCapsPodAnnotation        = "fargate.eks.amazonaws.com/advancedLinuxCaps"
	blockedAdvancedLinuxCapsPodAnnotation = "fargate.eks.amazonaws.com/blockedAdvancedLinuxCaps"
	runContainerValidationAnnotation      = "fargate.eks.amazonaws.com/runContainerValidation"
	containerAdmitMessageAnnotation       = "fargate.eks.amazonaws.com/containerAdmitMessage"
)

// TestPodValidation tests the podSpecValidator.
func TestPodValidation(t *testing.T) {
	err := filepath.Walk(fixtureDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			t.Errorf("Error while walking test fixtures: %v", err)
			return err
		}
		if info.IsDir() {
			return nil
		}

		if filepath.Ext(info.Name()) != ".yaml" {
			return nil
		}
		pod, err := parseFile(path)
		if err != nil {
			t.Errorf("Error while parsing file %s: %v", info.Name(), err)
			return err
		}
		// instantiating test client here to make sure namespace matches the pod spec in the testdata .yaml
		stopCh := make(chan struct{})
		defer close(stopCh)
		client := getFakeKubeClientForVolumeTests(pod.Namespace)
		validator := NewPodSpecValidator(client)
		t.Run(fmt.Sprintf("Pod %s in file %s", pod.Name, path), func(t *testing.T) {
			expectedAdmitValue, ok := pod.Annotations[admitPodAnnotation]
			if !ok {
				t.Errorf("Pod %s in file %s is missing annotation %s", pod.Name, path, admitPodAnnotation)
			}
			os.Setenv("ADVANCED_LINUX_CAPS", pod.Annotations[advancedLinuxCapsPodAnnotation])
			os.Setenv("BLOCKED_LINUX_CAPS", pod.Annotations[blockedAdvancedLinuxCapsPodAnnotation])
			expectedAdmitMessage, _ := pod.Annotations[messagePodAnnotation]
			err = validator.Validate(pod)
			admit := (err == nil)
			var message string
			if err != nil {
				message = err.Error()
			}
			if expectedAdmitValue != fmt.Sprintf("%t", admit) {
				t.Errorf("Pod %s in file %s expected admit %s, got %t",
					pod.Name, path, expectedAdmitValue, admit)
			}
			if !admit {
				if expectedAdmitMessage != message {
					t.Errorf("Pod %s in file %s expected message '%s', got '%s'",
						pod.Name, path, expectedAdmitMessage, message)
				}
			}
			if _, ok := pod.Annotations[runContainerValidationAnnotation]; ok {
				admit, err = validator.ValidateContainer(&pod.Spec.Containers[0])
				assert.Equal(t, strconv.FormatBool(admit), pod.Annotations[admitPodAnnotation])
				assert.EqualError(t, err, pod.Annotations[containerAdmitMessageAnnotation])
			}
		})
		return nil
	})
	if err != nil {
		t.Errorf("Error while walking test fixtures: %v", err)
	}
}

func parseFile(filename string) (*corev1.Pod, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	pod := &corev1.Pod{}
	err = yaml.Unmarshal(data, pod)
	return pod, err
}

func getFakeKubeClientForVolumeTests(namespace string) clientset.Interface {
	efsPvName := "efs-pv"
	nonEfsPvName := "not-an-efs-pv"
	noCSIPVName := "no-csi-pv"
	validPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "efs-pvc", Namespace: namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName: efsPvName,
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
	}
	validPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: efsPvName},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: "efs.csi.aws.com",
			}},
		},
	}
	invalidPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-pvc", Namespace: namespace},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimPending,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName: nonEfsPvName,
		},
	}

	nonEFSPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-with-nonefs-pv", Namespace: namespace},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName: nonEfsPvName,
		},
	}

	nonEFSPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: nonEfsPvName},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: "ebs.csi.aws.com",
			}},
		},
	}

	noCSIPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-with-nocsi-pv", Namespace: namespace},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName: noCSIPVName,
		},
	}

	noCSIPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: noCSIPVName},
		Spec:       corev1.PersistentVolumeSpec{},
	}

	return fake.NewSimpleClientset(validPVC, validPV, invalidPVC, nonEFSPVC, nonEFSPV, noCSIPVC, noCSIPV)
}
