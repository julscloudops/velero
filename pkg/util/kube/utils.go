/*
Copyright the Velero contributors.

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

package kube

import (
	"context"
	"fmt"
	"time"

	"github.com/pkg/errors"
	corev1api "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// NamespaceAndName returns a string in the format <namespace>/<name>
func NamespaceAndName(objMeta metav1.Object) string {
	if objMeta.GetNamespace() == "" {
		return objMeta.GetName()
	}
	return fmt.Sprintf("%s/%s", objMeta.GetNamespace(), objMeta.GetName())
}

// EnsureNamespaceExistsAndIsReady attempts to create the provided Kubernetes namespace.
// It returns three values: a bool indicating whether or not the namespace is ready,
// a bool indicating whether or not the namespace was created and an error if the creation failed
// for a reason other than that the namespace already exists. Note that in the case where the
// namespace already exists and is not ready, this function will return (false, false, nil).
// If the namespace exists and is marked for deletion, this function will wait up to the timeout for it to fully delete.
func EnsureNamespaceExistsAndIsReady(namespace *corev1api.Namespace, client corev1client.NamespaceInterface, timeout time.Duration) (bool, bool, error) {
	// nsCreated tells whether the namespace was created by this method
	// required for keeping track of number of restored items
	var nsCreated bool
	var ready bool
	err := wait.PollImmediate(time.Second, timeout, func() (bool, error) {
		clusterNS, err := client.Get(context.TODO(), namespace.Name, metav1.GetOptions{})

		if apierrors.IsNotFound(err) {
			// Namespace isn't in cluster, we're good to create.
			return true, nil
		}

		if err != nil {
			// Return the err and exit the loop.
			return true, err
		}

		if clusterNS != nil && (clusterNS.GetDeletionTimestamp() != nil || clusterNS.Status.Phase == corev1api.NamespaceTerminating) {
			// Marked for deletion, keep waiting
			return false, nil
		}

		// clusterNS found, is not nil, and not marked for deletion, therefore we shouldn't create it.
		ready = true
		return true, nil
	})

	// err will be set if we timed out or encountered issues retrieving the namespace,
	if err != nil {
		return false, nsCreated, errors.Wrapf(err, "error getting namespace %s", namespace.Name)
	}

	// In the case the namespace already exists and isn't marked for deletion, assume it's ready for use.
	if ready {
		return true, nsCreated, nil
	}

	clusterNS, err := client.Create(context.TODO(), namespace, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		if clusterNS != nil && (clusterNS.GetDeletionTimestamp() != nil || clusterNS.Status.Phase == corev1api.NamespaceTerminating) {
			// Somehow created after all our polling and marked for deletion, return an error
			return false, nsCreated, errors.Errorf("namespace %s created and marked for termination after timeout", namespace.Name)
		}
	} else if err != nil {
		return false, nsCreated, errors.Wrapf(err, "error creating namespace %s", namespace.Name)
	} else {
		nsCreated = true
	}

	// The namespace created successfully
	return true, nsCreated, nil
}

// GetVolumeDirectory gets the name of the directory on the host, under /var/lib/kubelet/pods/<podUID>/volumes/,
// where the specified volume lives.
// For volumes with a CSIVolumeSource, append "/mount" to the directory name.
func GetVolumeDirectory(pod *corev1api.Pod, volumeName string, pvcLister corev1listers.PersistentVolumeClaimLister, pvLister corev1listers.PersistentVolumeLister) (string, error) {
	var volume *corev1api.Volume

	for _, item := range pod.Spec.Volumes {
		if item.Name == volumeName {
			volume = &item
			break
		}
	}

	if volume == nil {
		return "", errors.New("volume not found in pod")
	}

	// This case implies the administrator created the PV and attached it directly, without PVC.
	// Note that only one VolumeSource can be populated per Volume on a pod
	if volume.VolumeSource.PersistentVolumeClaim == nil {
		if volume.VolumeSource.CSI != nil {
			return volume.Name + "/mount", nil
		}
		return volume.Name, nil
	}

	// Most common case is that we have a PVC VolumeSource, and we need to check the PV it points to for a CSI source.
	pvc, err := pvcLister.PersistentVolumeClaims(pod.Namespace).Get(volume.VolumeSource.PersistentVolumeClaim.ClaimName)
	if err != nil {
		return "", errors.WithStack(err)
	}

	pv, err := pvLister.Get(pvc.Spec.VolumeName)
	if err != nil {
		return "", errors.WithStack(err)
	}

	// PV's been created with a CSI source.
	if pv.Spec.CSI != nil {
		return pvc.Spec.VolumeName + "/mount", nil
	}

	return pvc.Spec.VolumeName, nil
}

// IsV1CRDReady checks a v1 CRD to see if it's ready, with both the Established and NamesAccepted conditions.
func IsV1CRDReady(crd *apiextv1.CustomResourceDefinition) bool {
	var isEstablished, namesAccepted bool
	for _, cond := range crd.Status.Conditions {
		if cond.Type == apiextv1.Established && cond.Status == apiextv1.ConditionTrue {
			isEstablished = true
		}
		if cond.Type == apiextv1.NamesAccepted && cond.Status == apiextv1.ConditionTrue {
			namesAccepted = true
		}
	}

	return (isEstablished && namesAccepted)
}

// IsV1Beta1CRDReady checks a v1beta1 CRD to see if it's ready, with both the Established and NamesAccepted conditions.
func IsV1Beta1CRDReady(crd *apiextv1beta1.CustomResourceDefinition) bool {
	var isEstablished, namesAccepted bool
	for _, cond := range crd.Status.Conditions {
		if cond.Type == apiextv1beta1.Established && cond.Status == apiextv1beta1.ConditionTrue {
			isEstablished = true
		}
		if cond.Type == apiextv1beta1.NamesAccepted && cond.Status == apiextv1beta1.ConditionTrue {
			namesAccepted = true
		}
	}

	return (isEstablished && namesAccepted)
}

// IsCRDReady triggers IsV1Beta1CRDReady/IsV1CRDReady according to the version of the input param
func IsCRDReady(crd *unstructured.Unstructured) (bool, error) {
	ver := crd.GroupVersionKind().Version
	switch ver {
	case "v1beta1":
		v1beta1crd := &apiextv1beta1.CustomResourceDefinition{}
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(crd.Object, v1beta1crd)
		if err != nil {
			return false, err
		}
		return IsV1Beta1CRDReady(v1beta1crd), nil
	case "v1":
		v1crd := &apiextv1.CustomResourceDefinition{}
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(crd.Object, v1crd)
		if err != nil {
			return false, err
		}
		return IsV1CRDReady(v1crd), nil
	default:
		return false, fmt.Errorf("unable to handle CRD with version %s", ver)
	}
}
