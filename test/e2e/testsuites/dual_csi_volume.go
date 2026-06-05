/*
Copyright 2018 The Kubernetes Authors.
Copyright 2022 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package testsuites

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	storageutils "k8s.io/kubernetes/test/e2e/storage/utils"
	admissionapi "k8s.io/pod-security-admission/api"
	"local/test/e2e/specs"
)

// PDStorageClass is the StorageClass used to provision the PD-backed PVC in
// dual CSI volume tests. Overridden via --pd-storage-class in e2e_test.go.
var PDStorageClass = "standard-rwo"

const (
	pdCSIDriverName      = "pd.csi.storage.gke.io"
	gcsFuseMountPath     = "/mnt/gcs"
	pdMountPath          = "/mnt/pd"
	gcsFuseVolName       = "gcs-vol"
	pdVolName            = "pd-vol"
	snapshotReadyTimeout = 5 * time.Minute
)

type gcsFuseCSIDualCSIVolumeTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

// InitGcsFuseCSIDualCSIVolumeTestSuite returns gcsFuseCSIDualCSIVolumeTestSuite
// that implements TestSuite interface.
func InitGcsFuseCSIDualCSIVolumeTestSuite() storageframework.TestSuite {
	return &gcsFuseCSIDualCSIVolumeTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "dual-csi-volume",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *gcsFuseCSIDualCSIVolumeTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *gcsFuseCSIDualCSIVolumeTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

func (t *gcsFuseCSIDualCSIVolumeTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		config          *storageframework.PerTestConfig
		gcsFuseResource *storageframework.VolumeResource
	}
	var l local
	ctx := context.Background()

	f := framework.NewFrameworkWithCustomTimeouts("dual-csi-volume", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityEnforceLevel = admissionapi.LevelPrivileged

	init := func() {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		l.config.Prefix = specs.SkipCSIBucketAccessCheckPrefix
		l.gcsFuseResource = storageframework.CreateVolumeResource(ctx, driver, l.config, pattern, e2evolume.SizeRange{})
	}

	cleanup := func() {
		if l.gcsFuseResource != nil {
			framework.ExpectNoError(l.gcsFuseResource.CleanupResource(ctx))
		}
	}

	createPDPVC := func(namePrefix, size string) (*corev1.PersistentVolumeClaim, func()) {
		scName := PDStorageClass
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: namePrefix,
				Namespace:    f.Namespace.Name,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: &scName,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse(size),
					},
				},
			},
		}
		pvc, err := f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Create(ctx, pvc, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		return pvc, func() {
			framework.ExpectNoError(f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Delete(
				ctx, pvc.Name, metav1.DeleteOptions{}))
		}
	}

	// Snapshot while mounted: a pod writes to the PD volume while a GCS Fuse volume
	// is also mounted. A VolumeSnapshot of the PD PVC is triggered mid-run. The test
	// verifies the snapshot becomes ready and the GCS Fuse mount remains unaffected.
	ginkgo.It("should create a VolumeSnapshot of the PD PVC without disrupting the GCS Fuse mount", func() {
		_, err := f.ClientSet.StorageV1().CSIDrivers().Get(ctx, pdCSIDriverName, metav1.GetOptions{})
		if err != nil {
			e2eskipper.Skipf("%s CSIDriver not found, skipping snapshot-while-mounted test: %v", pdCSIDriverName, err)
		}

		dc := f.DynamicClient
		if _, err := dc.Resource(storageutils.SnapshotClassGVR).List(ctx, metav1.ListOptions{}); err != nil {
			e2eskipper.Skipf("VolumeSnapshot CRDs not available, skipping snapshot test: %v", err)
		}

		init()
		defer cleanup()

		pvc, cleanupPVC := createPDPVC("snapshot-pd-pvc-", "5Gi")
		defer cleanupPVC()

		tPod := specs.NewTestPod(f.ClientSet, f.Namespace)
		tPod.SetupVolume(l.gcsFuseResource, gcsFuseVolName, gcsFuseMountPath, false)
		tPod.SetupVolume(&storageframework.VolumeResource{Pvc: pvc}, pdVolName, pdMountPath, false)

		tPod.Create(ctx)
		defer tPod.Cleanup(ctx)
		tPod.WaitForRunning(ctx)

		ginkgo.By("Writing a file to the PD volume to simulate active workload")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'pre-snapshot-data' > %v/data.txt", pdMountPath))

		ginkgo.By("Writing a marker file to the GCS Fuse mount before snapshot")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'gcs-marker' > %v/marker.txt", gcsFuseMountPath))

		ginkgo.By("Creating VolumeSnapshotClass for pd.csi.storage.gke.io")
		vsClass := storageutils.GenerateSnapshotClassSpec(pdCSIDriverName, map[string]string{}, f.Namespace.Name)
		vsClass, err = dc.Resource(storageutils.SnapshotClassGVR).Create(ctx, vsClass, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		defer func() {
			if err := dc.Resource(storageutils.SnapshotClassGVR).Delete(ctx, vsClass.GetName(), metav1.DeleteOptions{}); err != nil {
				framework.Logf("Failed to delete VolumeSnapshotClass %q: %v", vsClass.GetName(), err)
			}
		}()

		ginkgo.By(fmt.Sprintf("Creating VolumeSnapshot of PD PVC %q", pvc.Name))
		snapshot := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": storageutils.SnapshotAPIVersion,
				"kind":       "VolumeSnapshot",
				"metadata": map[string]interface{}{
					"generateName": "pd-snapshot-",
					"namespace":    f.Namespace.Name,
				},
				"spec": map[string]interface{}{
					"volumeSnapshotClassName": vsClass.GetName(),
					"source": map[string]interface{}{
						"persistentVolumeClaimName": pvc.Name,
					},
				},
			},
		}
		snapshot, err = dc.Resource(storageutils.SnapshotGVR).Namespace(f.Namespace.Name).Create(ctx, snapshot, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		defer func() {
			if err := storageutils.DeleteAndWaitSnapshot(ctx, dc, f.Namespace.Name, snapshot.GetName(), framework.Poll, snapshotReadyTimeout); err != nil {
				framework.Logf("Failed to delete VolumeSnapshot %q: %v", snapshot.GetName(), err)
			}
		}()

		ginkgo.By("Waiting for the VolumeSnapshot to become ready")
		framework.ExpectNoError(
			storageutils.WaitForSnapshotReady(ctx, dc, f.Namespace.Name, snapshot.GetName(), framework.Poll, snapshotReadyTimeout),
		)

		ginkgo.By("Verifying the GCS Fuse mount is still accessible after snapshot")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("grep 'gcs-marker' %v/marker.txt", gcsFuseMountPath))

		ginkgo.By("Verifying the PD volume is still writable after snapshot")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'post-snapshot-data' >> %v/data.txt && grep 'post-snapshot-data' %v/data.txt",
				pdMountPath, pdMountPath))
	})
}
