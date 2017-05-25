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

package client

import (
	"time"

	tprv1 "github.com/rootfs/snapshot/apis/tpr/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	apiv1 "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/rest"
	// Uncomment the following line to load the gcp plugin (only required to authenticate against GKE clusters).
	// _ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
)

func CreateTPR(clientset kubernetes.Interface) error {
	tpr := &v1beta1.ThirdPartyResource{
		ObjectMeta: metav1.ObjectMeta{
			Name: "snapshot." + tprv1.GroupName,
		},
		Versions: []v1beta1.APIVersion{
			{Name: tprv1.SchemeGroupVersion.Version},
		},
		Description: "An Example ThirdPartyResource",
	}
	_, err := clientset.ExtensionsV1beta1().ThirdPartyResources().Create(tpr)
	return err
}

func WaitForSnapshotResource(snapshotClient *rest.RESTClient) error {
	return wait.Poll(100*time.Millisecond, 60*time.Second, func() (bool, error) {
		_, err := snapshotClient.Get().Namespace(apiv1.NamespaceDefault).Resource(tprv1.VolumeSnapshotResource).DoRaw()
		if err == nil {
			return true, nil
		}
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	})
}

func WaitForSnapshotProcessed(snapshotClient *rest.RESTClient, name string) error {
	return wait.Poll(100*time.Millisecond, 10*time.Second, func() (bool, error) {
		var snapshot tprv1.VolumeSnapshot
		err := snapshotClient.Get().
			Resource(tprv1.VolumeSnapshotResource).
			Namespace(apiv1.NamespaceDefault).
			Name(name).
			Do().Into(&snapshot)

		if err == nil && len(snapshot.Status.Conditions) > 0 && snapshot.Status.Conditions[0].Type == tprv1.VolumeSnapshotConditionReady {
			return true, nil
		}

		return false, err
	})
}
