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

package snapshotter

import (
	"fmt"

	"github.com/golang/glog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/pkg/util/goroutinemap"
	"k8s.io/kubernetes/pkg/util/goroutinemap/exponentialbackoff"

	tprv1 "github.com/rootfs/snapshot/pkg/apis/tpr/v1"
	"github.com/rootfs/snapshot/pkg/controller/cache"
	"github.com/rootfs/snapshot/pkg/volume"
)

const (
	defaultExponentialBackOffOnError = true
)

// VolumeSnapshotter does the "heavy lifting": it spawns gouroutines that talk to the
// backend to actually perform the operations on the storage devices.
// It creates and deletes the snapshots and promotes snapshots to volumes (PV). The create
// and delete operations need to be idempotent and count with the fact the API object writes
type VolumeSnapshotter interface {
	CreateVolumeSnapshot(snapshotName string, snapshotSpec *tprv1.VolumeSnapshotSpec)
	DeleteVolumeSnapshot(snapshotName string, snapshotSpec *tprv1.VolumeSnapshotSpec)
	PromoteVolumeSnapshotToPV(snapshotName string, snapshotSpec *tprv1.VolumeSnapshotSpec)
	UpdateVolumeSnapshot(snapshotName string) error
}

type volumeSnapshotter struct {
	restClient         *rest.RESTClient
	coreClient         kubernetes.Interface
	scheme             *runtime.Scheme
	actualStateOfWorld cache.ActualStateOfWorld
	runningOperation   goroutinemap.GoRoutineMap
	volumePlugins      *map[string]volume.VolumePlugin
}

const (
	snapshotOpCreatePrefix  string = "create"
	snapshotOpDeletePrefix  string = "delete"
	snapshotOpPromotePrefix string = "promote"
)

func NewVolumeSnapshotter(
	restClient *rest.RESTClient,
	scheme *runtime.Scheme,
	clientset kubernetes.Interface,
	asw cache.ActualStateOfWorld,
	volumePlugins *map[string]volume.VolumePlugin) VolumeSnapshotter {
	return &volumeSnapshotter{
		restClient:         restClient,
		coreClient:         clientset,
		scheme:             scheme,
		actualStateOfWorld: asw,
		runningOperation:   goroutinemap.NewGoRoutineMap(defaultExponentialBackOffOnError),
		volumePlugins:      volumePlugins,
	}
}

// Helper function to get PV from VolumeSnapshot
func (vs *volumeSnapshotter) getPVFromVolumeSnapshot(snapshotName string, snapshotSpec *tprv1.VolumeSnapshotSpec) (*v1.PersistentVolume, error) {
	pvcName := snapshotSpec.PersistentVolumeClaimName
	if pvcName == "" {
		return nil, fmt.Errorf("The PVC name is not specified in snapshot %s", snapshotName)
	}
	snapNameSpace, _, err := cache.GetNameAndNameSpaceFromSnapshotName(snapshotName)
	if err != nil {
		return nil, fmt.Errorf("Snapshot %s is malformed", snapshotName)
	}
	pvc, err := vs.coreClient.CoreV1().PersistentVolumeClaims(snapNameSpace).Get(pvcName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("Failed to retrieve PVC %s from the API server: %q", pvcName, err)
	}
	if pvc.Status.Phase != v1.ClaimBound {
		return nil, fmt.Errorf("The PVC %s not yet bound to a PV, will not attempt to take a snapshot yet.")
	}

	pvName := pvc.Spec.VolumeName
	pv, err := vs.coreClient.CoreV1().PersistentVolumes().Get(pvName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("Failed to retrieve PV %s from the API server: %q", pvName, err)
	}
	return pv, nil
}

// Helper function that looks up VolumeSnapshotData for a VolumeSnapshot named snapshotName
func (vs *volumeSnapshotter) getSnapshotDataFromSnapshotName(snapshotName string) *tprv1.VolumeSnapshotData {
	var snapshotDataList tprv1.VolumeSnapshotDataList
	var snapshotDataObj tprv1.VolumeSnapshotData
	var found bool

	err := vs.restClient.Get().
		Resource(tprv1.VolumeSnapshotDataResourcePlural).
		Namespace(v1.NamespaceDefault).
		Do().Into(&snapshotDataList)
	if err != nil {
		glog.Errorf("Error retrieving the VolumeSnapshotData objects from API server: %v", err)
		return nil
	}
	if len(snapshotDataList.Items) == 0 {
		glog.Errorf("Error: no VolumeSnapshotData objects found on the API server")
		return nil
	}
	for _, snapData := range snapshotDataList.Items {
		if snapData.Spec.VolumeSnapshotRef.Name == snapshotName {
			snapshotDataObj = snapData
			found = true
			break
		}
	}
	if !found {
		glog.Errorf("Error: no VolumeSnapshotData for VolumeSnapshot %s found", snapshotName)
		return nil
	}

	return &snapshotDataObj
}

// This is the function responsible for determining the correct volume plugin to use,
// asking it to make a snapshot and assignig it some name that it returns to the caller.
func (vs *volumeSnapshotter) takeSnapshot(spec *v1.PersistentVolumeSpec) (*tprv1.VolumeSnapshotDataSource, error) {
	volumeType := tprv1.GetSupportedVolumeFromPVSpec(spec)
	if len(volumeType) == 0 {
		return nil, fmt.Errorf("unsupported volume type found in PV %#v", spec)
	}
	plugin, ok := (*vs.volumePlugins)[volumeType]
	if !ok {
		return nil, fmt.Errorf("%s is not supported volume for %#v", volumeType, spec)
	}
	snap, err := plugin.SnapshotCreate(spec)
	if err != nil {
		glog.Warningf("failed to snapshot %#v, err: %v", spec, err)
	} else {
		glog.Infof("snapshot %#v to snap %#v", spec, snap)
		return snap, nil
	}
	return nil, nil
}

// This is the function responsible for determining the correct volume plugin to use,
// asking it to make a snapshot and assignig it some name that it returns to the caller.
func (vs *volumeSnapshotter) deleteSnapshot(spec *v1.PersistentVolumeSpec, source *tprv1.VolumeSnapshotDataSource) error {
	volumeType := tprv1.GetSupportedVolumeFromPVSpec(spec)
	if len(volumeType) == 0 {
		return fmt.Errorf("unsupported volume type found in PV %#v", spec)
	}
	plugin, ok := (*vs.volumePlugins)[volumeType]
	if !ok {
		return fmt.Errorf("%s is not supported volume for %#v", volumeType, spec)
	}
	err := plugin.SnapshotDelete(source, nil /* *v1.PersistentVolume */)
	if err != nil {
		return fmt.Errorf("failed to delete snapshot %#v, err: %v", source, err)
	}
	glog.Infof("snapshot %#v deleted", source)

	return nil
}

// Below are the closures meant to build the functions for the GoRoutineMap operations.

func (vs *volumeSnapshotter) getSnapshotCreateFunc(snapshotName string, snapshotSpec *tprv1.VolumeSnapshotSpec) func() error {
	// Create a snapshot:
	// 1. If Snapshot referencs SnapshotData object, try to find it
	//   1a. If doesn't exist, log error and finish, if it exists already, check its SnapshotRef
	//   1b. If it's empty, check its Spec UID (or fins out what PV/PVC does and copyt the mechanism)
	//   1c. If it matches the user (TODO: how to find out?), bind the two objects and finish
	//   1d. If it doesn't match, log error and finish.
	// 2. Create the SnapshotData object
	// 3. Ask the backend to create the snapshot (device)
	// 4. If OK, update the SnapshotData and Snapshot objects
	// 5. Add the Snapshot to the ActualStateOfWorld
	// 6. Finish (we have created snapshot for an user)
	return func() error {
		if snapshotSpec.SnapshotDataName != "" {
			// FIXME: what does it mean imported admin-created snapshots
			vs.actualStateOfWorld.AddSnapshot(snapshotName, snapshotSpec)
			glog.Infof("snapshot %s already binds to snapshotdata %s", snapshotName, snapshotSpec.SnapshotDataName)
			return nil
		}
		pv, err := vs.getPVFromVolumeSnapshot(snapshotName, snapshotSpec)
		if err != nil {
			return err
		}
		pvName := pv.Name
		snapshotDataSource, err := vs.takeSnapshot(&pv.Spec)
		if err != nil || snapshotDataSource == nil {
			return fmt.Errorf("Failed to take snapshot of the volume %s: %q", pvName, err)
		}
		// Snapshot has been created, made an object for it
		readyCondition := tprv1.VolumeSnapshotDataCondition{
			Type:    tprv1.VolumeSnapshotDataConditionReady,
			Status:  v1.ConditionTrue,
			Message: "Snapsot created succsessfully",
		}
		snapName := "k8s-volume-snapshot-" + string(uuid.NewUUID())

		snapshotData := &tprv1.VolumeSnapshotData{
			Metadata: metav1.ObjectMeta{
				Name: snapName,
			},
			Spec: tprv1.VolumeSnapshotDataSpec{
				VolumeSnapshotRef: &v1.ObjectReference{
					Kind: "VolumeSnapshot",
					Name: snapshotName,
				},
				PersistentVolumeRef: &v1.ObjectReference{
					Kind: "PersistentVolume",
					Name: pvName,
				},
				VolumeSnapshotDataSource: *snapshotDataSource,
			},
			Status: tprv1.VolumeSnapshotDataStatus{
				Conditions: []tprv1.VolumeSnapshotDataCondition{
					readyCondition,
				},
			},
		}
		var result metav1.Status
		err = vs.restClient.Post().
			Resource(tprv1.VolumeSnapshotDataResourcePlural).
			Namespace(v1.NamespaceDefault).
			Body(snapshotData).
			Do().Into(&result)

		if err != nil {
			//FIXME: Errors writing to the API server are common: this needs to be re-tried
			glog.Errorf("Error creating the VolumeSnapshotData %s: %v", snapshotName, err)
		}
		vs.actualStateOfWorld.AddSnapshot(snapshotName, snapshotSpec)
		// Update the VolumeSnapshot object too
		err = vs.UpdateVolumeSnapshot(snapshotName)
		if err != nil {
			glog.Errorf("Error updating volume snapshot %s: %v", snapshotName, err)
		}

		return nil
	}
}

func (vs *volumeSnapshotter) getSnapshotDeleteFunc(snapshotName string, snapshotSpec *tprv1.VolumeSnapshotSpec) func() error {
	// Delete a snapshot
	// 1. Find the SnapshotData corresponding to Snapshot
	//   1a: Not found => finish (it's been deleted already)
	// 2. Ask the backend to remove the snapshot device
	// 3. Delete the SnapshotData object
	// 4. Remove the Snapshot from ActualStateOfWorld
	// 5. Finish
	return func() error {
		// TODO: get VolumeSnapshotDataSource from associated VolumeSnapshotData
		// then call volume delete snapshot method to delete the ot
		snapshotDataObj := vs.getSnapshotDataFromSnapshotName(snapshotName)
		if snapshotDataObj == nil {
			return fmt.Errorf("Error getting VolumeSnapshotData for VolumeSnapshot %s", snapshotName)
		}

		pv, err := vs.getPVFromVolumeSnapshot(snapshotName, snapshotSpec)
		if err != nil {
			return err
		}

		err = vs.deleteSnapshot(&pv.Spec, &snapshotDataObj.Spec.VolumeSnapshotDataSource)
		if err != nil {
			return fmt.Errorf("Failed to delete snapshot %s: %q", snapshotName, err)
		}

		snapshotDataName := snapshotDataObj.Metadata.Name
		var result metav1.Status
		err = vs.restClient.Delete().
			Name(snapshotDataName).
			Resource(tprv1.VolumeSnapshotDataResourcePlural).
			Namespace(v1.NamespaceDefault).
			Do().Into(&result)
		if err != nil {
			return fmt.Errorf("Failed to delete VolumeSnapshotData %s from API server: %q", snapshotDataName, err)
		}

		vs.actualStateOfWorld.DeleteSnapshot(snapshotName)

		return nil
	}
}

func (vs *volumeSnapshotter) getSnapshotPromoteFunc(snapshotName string, snapshotSpec *tprv1.VolumeSnapshotSpec) func() error {
	// Promote snapshot to a PVC
	// 1. We have a PVC referencing a Snapshot object
	// 2. Find the SnapshotData corresponding to tha Snapshot
	// 3. Ask the backend to give us a device (PV) made from the snapshot device
	// 4. Bind it to the PVC
	// 5. Finish
	return func() error { return nil }
}

func (vs *volumeSnapshotter) CreateVolumeSnapshot(snapshotName string, snapshotSpec *tprv1.VolumeSnapshotSpec) {
	operationName := snapshotOpCreatePrefix + snapshotName + snapshotSpec.PersistentVolumeClaimName
	glog.Infof("Snapshotter is about to create volume snapshot operation named %s, spec %#v", operationName, snapshotSpec)

	err := vs.runningOperation.Run(operationName, vs.getSnapshotCreateFunc(snapshotName, snapshotSpec))

	if err != nil {
		switch {
		case goroutinemap.IsAlreadyExists(err):
			glog.V(4).Infof("operation %q is already running, skipping", operationName)
		case exponentialbackoff.IsExponentialBackoff(err):
			glog.V(4).Infof("operation %q postponed due to exponential backoff", operationName)
		default:
			glog.Errorf("Failed to schedule the operation %q: %v", operationName, err)
		}
	}
}

func (vs *volumeSnapshotter) DeleteVolumeSnapshot(snapshotName string, snapshotSpec *tprv1.VolumeSnapshotSpec) {
	operationName := snapshotOpDeletePrefix + snapshotName + snapshotSpec.PersistentVolumeClaimName
	glog.Infof("Snapshotter is about to create volume snapshot operation named %s", operationName)

	err := vs.runningOperation.Run(operationName, vs.getSnapshotDeleteFunc(snapshotName, snapshotSpec))

	if err != nil {
		switch {
		case goroutinemap.IsAlreadyExists(err):
			glog.V(4).Infof("operation %q is already running, skipping", operationName)
		case exponentialbackoff.IsExponentialBackoff(err):
			glog.V(4).Infof("operation %q postponed due to exponential backoff", operationName)
		default:
			glog.Errorf("Failed to schedule the operation %q: %v", operationName, err)
		}
	}
}

func (vs *volumeSnapshotter) PromoteVolumeSnapshotToPV(snapshotName string, snapshotSpec *tprv1.VolumeSnapshotSpec) {
	operationName := snapshotOpPromotePrefix + snapshotName + snapshotSpec.PersistentVolumeClaimName
	glog.Infof("Snapshotter is about to create volume snapshot operation named %s", operationName)

	err := vs.runningOperation.Run(operationName, vs.getSnapshotPromoteFunc(snapshotName, snapshotSpec))

	if err != nil {
		switch {
		case goroutinemap.IsAlreadyExists(err):
			glog.V(4).Infof("operation %q is already running, skipping", operationName)
		case exponentialbackoff.IsExponentialBackoff(err):
			glog.V(4).Infof("operation %q postponed due to exponential backoff", operationName)
		default:
			glog.Errorf("Failed to schedule the operation %q: %v", operationName, err)
		}
	}
}

func (vs *volumeSnapshotter) UpdateVolumeSnapshot(snapshotName string) error {
	var snapshotObj tprv1.VolumeSnapshot

	glog.Infof("In UpdateVolumeSnapshot")
	// Get a fresh copy of the VolumeSnapshotData from the API server
	// Get a fresh copy of the VolumeSnapshot from the API server
	snapNameSpace, snapName, err := cache.GetNameAndNameSpaceFromSnapshotName(snapshotName)
	if err != nil {
		return fmt.Errorf("Error getting namespace and name from VolumeSnapshot name %s: %v", snapshotName, err)
	}
	err = vs.restClient.Get().
		Name(snapName).
		Resource(tprv1.VolumeSnapshotResourcePlural).
		Namespace(snapNameSpace).
		Do().Into(&snapshotObj)

	objCopy, err := vs.scheme.DeepCopy(&snapshotObj)
	if err != nil {
		return fmt.Errorf("Error copying snapshot object %s object from API server: %v", snapshotName, err)
	}
	snapshotCopy, ok := objCopy.(*tprv1.VolumeSnapshot)
	if !ok {
		return fmt.Errorf("Error: expecting type VolumeSnapshot but received type %T", objCopy)
	}

	snapshotDataObj := vs.getSnapshotDataFromSnapshotName(snapshotName)
	if snapshotDataObj == nil {
		return fmt.Errorf("Error getting VolumeSnapshotData for VolumeSnapshot %s", snapshotName)
	}

	snapshotCopy.Spec.SnapshotDataName = snapshotDataObj.Metadata.Name
	snapshotCopy.Status.Conditions = []tprv1.VolumeSnapshotCondition{
		{
			Type:               tprv1.VolumeSnapshotConditionReady,
			Status:             v1.ConditionTrue,
			Message:            "Snapshot created succsessfully",
			LastTransitionTime: metav1.Now(),
		},
	}
	glog.Infof("Updating VolumeSnapshot object")
	// TODO: Make diff of the two objects and then use restClient.Patch to update it
	var result tprv1.VolumeSnapshot
	err = vs.restClient.Put().
		Name(snapName).
		Resource(tprv1.VolumeSnapshotResourcePlural).
		Namespace(snapNameSpace).
		Body(snapshotCopy).
		Do().Into(&result)
	if err != nil {
		return fmt.Errorf("Error updating snapshot object %s on the API server: %v", snapshotName, err)
	}
	// Update the spec in the actual state of world
	vs.actualStateOfWorld.AddSnapshot(snapshotName, &snapshotCopy.Spec)
	return nil
}
