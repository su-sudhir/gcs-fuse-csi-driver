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
	"k8s.io/apimachinery/pkg/types"
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
	pdCSIDriverName  = "pd.csi.storage.gke.io"
	gcsFuseMountPath = "/mnt/gcs"
	pdMountPath      = "/mnt/pd"
	gcsFuseVolName   = "gcs-vol"
	pdVolName        = "pd-vol"

	// largeFileSizeBytes is 1 GiB — large enough to stress both drivers.
	largeFileSizeBytes = 1 * 1024 * 1024 * 1024

	snapshotReadyTimeout = 5 * time.Minute
	pvcResizeTimeout     = 5 * time.Minute
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
		// Skip the CSI pre-mount bucket access check so the test works on
		// OSS clusters where the test pod has no credential configmap annotation.
		// The WIF IAM binding on the bucket still grants real GCSFuse access.
		l.config.Prefix = specs.SkipCSIBucketAccessCheckPrefix
		l.gcsFuseResource = storageframework.CreateVolumeResource(ctx, driver, l.config, pattern, e2evolume.SizeRange{})
	}

	cleanup := func() {
		if l.gcsFuseResource != nil {
			framework.ExpectNoError(l.gcsFuseResource.CleanupResource(ctx))
		}
	}

	// createPDPVC creates a PD-backed PVC and returns it with a cleanup func.
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

	// skipIfPDCSINotInstalled skips when pd.csi.storage.gke.io CSIDriver is absent.
	skipIfPDCSINotInstalled := func(testName string) {
		_, err := f.ClientSet.StorageV1().CSIDrivers().Get(ctx, pdCSIDriverName, metav1.GetOptions{})
		if err != nil {
			e2eskipper.Skipf("%s CSIDriver not found, skipping %s: %v", pdCSIDriverName, testName, err)
		}
	}

	// ── Test 1: Large file transfer (GCS → PD, 1 GiB) ───────────────────────────
	//
	// A 1 GiB file is pre-seeded in GCS via the GCS API. The pod copies it from
	// the GCS Fuse mount to the PD volume. The test verifies the transfer completes
	// without timeout or mount disruption on either driver, and that the sizes match.
	ginkgo.It("should transfer a 1 GiB file from the GCS Fuse mount to a PD-backed volume without timeout or mount disruption", func() {
		skipIfPDCSINotInstalled("large file transfer test")

		init()
		defer cleanup()

		bucketName := l.gcsFuseResource.Pv.Spec.CSI.VolumeHandle
		const largeFileName = "large-transfer-1g.bin"

		gcsfuseDriver, ok := driver.(*specs.GCSFuseCSITestDriver)
		if !ok {
			framework.Failf("driver is not *specs.GCSFuseCSITestDriver, cannot pre-seed GCS object")
		}

		ginkgo.By(fmt.Sprintf("Pre-seeding 1 GiB file %q in GCS bucket %q", largeFileName, bucketName))
		gcsfuseDriver.CreateTestFileWithSizeInBucket(ctx, largeFileName, bucketName, largeFileSizeBytes)

		pvc, cleanupPVC := createPDPVC("large-transfer-pd-pvc-", "5Gi")
		defer cleanupPVC()

		tPod := specs.NewTestPod(f.ClientSet, f.Namespace)
		tPod.SetupVolume(l.gcsFuseResource, gcsFuseVolName, gcsFuseMountPath, false)
		tPod.SetupVolume(&storageframework.VolumeResource{Pvc: pvc}, pdVolName, pdMountPath, false)

		tPod.Create(ctx)
		defer tPod.Cleanup(ctx)
		tPod.WaitForRunning(ctx)

		ginkgo.By("Verifying the 1 GiB file is readable on the GCS Fuse mount")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("test -f %v/%v", gcsFuseMountPath, largeFileName))

		ginkgo.By("Copying the 1 GiB file from the GCS Fuse mount to the PD mount")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("cp %v/%v %v/%v", gcsFuseMountPath, largeFileName, pdMountPath, largeFileName))

		ginkgo.By("Verifying the transferred file size matches the original on GCS")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("test $(stat -c%%s %v/%v) -eq $(stat -c%%s %v/%v)",
				pdMountPath, largeFileName, gcsFuseMountPath, largeFileName))

		ginkgo.By("Verifying the GCS Fuse mount is still accessible after the large transfer")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("test -f %v/%v", gcsFuseMountPath, largeFileName))
	})

	// ── Test 2: VolumeSnapshot of PD PVC while GCS Fuse is mounted ──────────────
	//
	// A pod writes to the PD volume while a GCS Fuse volume is also mounted.
	// A VolumeSnapshot of the PD PVC is triggered mid-run. The test verifies the
	// snapshot becomes ready and the GCS Fuse mount remains unaffected throughout.
	ginkgo.It("should create a VolumeSnapshot of the PD PVC without disrupting the GCS Fuse mount", func() {
		skipIfPDCSINotInstalled("snapshot-while-mounted test")

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
		vsClass, err := dc.Resource(storageutils.SnapshotClassGVR).Create(ctx, vsClass, metav1.CreateOptions{})
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

	// ── Test 3: Online PD PVC resize while GCS Fuse is mounted ──────────────────
	//
	// A pod has both a PD-backed PVC and a GCS Fuse volume mounted. The PD PVC is
	// expanded online while the pod is running. The test verifies the resize
	// completes, the new capacity is visible inside the pod, and the GCS Fuse
	// mount remains unaffected throughout.
	ginkgo.It("should expand the PD PVC online without pod restart and without disrupting the GCS Fuse mount", func() {
		skipIfPDCSINotInstalled("online PD resize test")

		init()
		defer cleanup()

		pvc, cleanupPVC := createPDPVC("resize-pd-pvc-", "5Gi")
		defer cleanupPVC()

		tPod := specs.NewTestPod(f.ClientSet, f.Namespace)
		tPod.SetupVolume(l.gcsFuseResource, gcsFuseVolName, gcsFuseMountPath, false)
		tPod.SetupVolume(&storageframework.VolumeResource{Pvc: pvc}, pdVolName, pdMountPath, false)

		tPod.Create(ctx)
		defer tPod.Cleanup(ctx)
		tPod.WaitForRunning(ctx)

		ginkgo.By("Writing a marker file to the GCS Fuse mount before resize")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'pre-resize-gcs' > %v/gcs-marker.txt", gcsFuseMountPath))

		ginkgo.By("Writing a file to the PD volume before resize")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'pre-resize-pd' > %v/pd-data.txt", pdMountPath))

		ginkgo.By(fmt.Sprintf("Expanding PVC %q from 5Gi to 10Gi", pvc.Name))
		patch := []byte(`{"spec":{"resources":{"requests":{"storage":"10Gi"}}}}`)
		pvc, err := f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Patch(
			ctx, pvc.Name, types.MergePatchType, patch, metav1.PatchOptions{},
		)
		framework.ExpectNoError(err)

		ginkgo.By("Waiting for the PVC to reflect the new 10Gi capacity")
		framework.ExpectNoError(waitForPVCCapacity(ctx, f, pvc.Name, resource.MustParse("10Gi"), pvcResizeTimeout))

		ginkgo.By("Verifying the GCS Fuse mount is still accessible after resize")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("grep 'pre-resize-gcs' %v/gcs-marker.txt", gcsFuseMountPath))

		ginkgo.By("Verifying the PD volume data is intact and the mount is still writable after resize")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("grep 'pre-resize-pd' %v/pd-data.txt && echo 'post-resize-pd' >> %v/pd-data.txt",
				pdMountPath, pdMountPath))

		ginkgo.By("Verifying the expanded PD capacity is visible inside the pod")
		// df reports in 1K blocks; 10Gi ≈ 10485760 KiB. Allow 5% tolerance (~9961472 KiB).
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf(`df %v | awk 'NR==2{if($2 >= 9961472) exit 0; else exit 1}'`, pdMountPath))
	})
}

// waitForPVCCapacity polls until the PVC's status.capacity.storage reaches at
// least the requested quantity or the timeout expires.
func waitForPVCCapacity(ctx context.Context, f *framework.Framework, pvcName string, requested resource.Quantity, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pvc, err := f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Get(ctx, pvcName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if cap, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
			if cap.Cmp(requested) >= 0 {
				return nil
			}
		}
		framework.Logf("PVC %q capacity not yet %v, retrying...", pvcName, requested)
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("PVC %q did not reach capacity %v within %v", pvcName, requested, timeout)
}
