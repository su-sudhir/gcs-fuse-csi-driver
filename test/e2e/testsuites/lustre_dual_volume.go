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

	"github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
	"local/test/e2e/specs"
)

// The following vars describe a pre-provisioned Managed Lustre instance used
// by the Lustre + GCS Fuse dual CSI volume tests. They are overridden via
// --lustre-volume-handle, --lustre-ip, --lustre-filesystem, and
// --lustre-capacity in e2e_test.go. If LustreVolumeHandle is empty, the
// dual-volume Lustre tests are skipped (e.g. on clusters without a Lustre
// instance provisioned).
var (
	LustreVolumeHandle string
	LustreIP           string
	LustreFilesystem   string
	LustreCapacity     = "100Gi"
)

const (
	lustreCSIDriverName = "lustre.csi.storage.gke.io"
	lustreMountPath     = "/mnt/lustre"
	lustreVolName       = "lustre-vol"
	gcsFuseMountPath    = "/mnt/gcs"
	gcsFuseVolName      = "gcs-vol"
)

type gcsFuseCSILustreDualVolumeTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

// InitGcsFuseCSILustreDualVolumeTestSuite returns gcsFuseCSILustreDualVolumeTestSuite
// that implements TestSuite interface.
func InitGcsFuseCSILustreDualVolumeTestSuite() storageframework.TestSuite {
	return &gcsFuseCSILustreDualVolumeTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "lustre-dual-csi-volume",
			// PreprovisionedPV only: the Lustre PV/PVC are statically bound to a
			// pre-existing Managed Lustre instance, independent of the
			// storageframework volume pattern.
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsPreprovisionedPV,
			},
		},
	}
}

func (t *gcsFuseCSILustreDualVolumeTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *gcsFuseCSILustreDualVolumeTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

func (t *gcsFuseCSILustreDualVolumeTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		config          *storageframework.PerTestConfig
		gcsFuseResource *storageframework.VolumeResource
	}
	var l local
	ctx := context.Background()

	f := framework.NewFrameworkWithCustomTimeouts("lustre-dual-csi-volume", storageframework.GetDriverTimeouts(driver))
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

	// skipIfLustreNotAvailable skips the test when no pre-provisioned Lustre
	// instance has been configured, or the Lustre CSIDriver is not installed
	// on the cluster.
	skipIfLustreNotAvailable := func(testName string) {
		if LustreVolumeHandle == "" {
			e2eskipper.Skipf("--lustre-volume-handle not set, skipping %s", testName)
		}
		if _, err := f.ClientSet.StorageV1().CSIDrivers().Get(ctx, lustreCSIDriverName, metav1.GetOptions{}); err != nil {
			e2eskipper.Skipf("%s CSIDriver not found, skipping %s: %v", lustreCSIDriverName, testName, err)
		}
	}

	// createLustrePVPVC statically provisions a PV/PVC pair bound to the
	// pre-existing Managed Lustre instance described by LustreVolumeHandle,
	// LustreIP, and LustreFilesystem. Returns the PVC and a cleanup func that
	// deletes both the PVC and the PV.
	createLustrePVPVC := func() (*corev1.PersistentVolumeClaim, func()) {
		capacity := resource.MustParse(LustreCapacity)
		pvcName := "lustre-pvc-" + f.Namespace.Name

		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "lustre-pv-",
			},
			Spec: corev1.PersistentVolumeSpec{
				Capacity: corev1.ResourceList{
					corev1.ResourceStorage: capacity,
				},
				AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
				VolumeMode:                    ptrVolumeMode(corev1.PersistentVolumeFilesystem),
				ClaimRef: &corev1.ObjectReference{
					Namespace: f.Namespace.Name,
					Name:      pvcName,
				},
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						Driver:       lustreCSIDriverName,
						VolumeHandle: LustreVolumeHandle,
						VolumeAttributes: map[string]string{
							"ip":         LustreIP,
							"filesystem": LustreFilesystem,
						},
					},
				},
			},
		}
		pv, err := f.ClientSet.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		scName := ""
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcName,
				Namespace: f.Namespace.Name,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				StorageClassName: &scName,
				VolumeName:       pv.Name,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: capacity,
					},
				},
			},
		}
		pvc, err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Create(ctx, pvc, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		return pvc, func() {
			framework.ExpectNoError(f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Delete(
				ctx, pvc.Name, metav1.DeleteOptions{}))
			framework.ExpectNoError(f.ClientSet.CoreV1().PersistentVolumes().Delete(
				ctx, pv.Name, metav1.DeleteOptions{}))
		}
	}

	// Same-pod dual mount + R/W: a Lustre PVC and a GCS Fuse volume are mounted
	// in a single pod. The test writes to and reads back from each mount
	// independently, verifying both volumes are accessible and writable
	// without conflict.
	ginkgo.It("should mount both a Lustre PVC and a GCS Fuse volume in a single pod and read/write independently on both", func() {
		skipIfLustreNotAvailable("same-pod dual mount + R/W test")

		init()
		defer cleanup()

		ginkgo.By("Creating a statically provisioned PVC bound to the pre-existing Lustre instance")
		pvc, cleanupPVC := createLustrePVPVC()
		defer cleanupPVC()

		ginkgo.By("Configuring the pod with both GCS Fuse and Lustre volumes")
		tPod := specs.NewTestPod(f.ClientSet, f.Namespace)
		tPod.SetupVolume(l.gcsFuseResource, gcsFuseVolName, gcsFuseMountPath, false)
		tPod.SetupVolume(&storageframework.VolumeResource{Pvc: pvc}, lustreVolName, lustreMountPath, false)

		ginkgo.By("Deploying the pod")
		tPod.Create(ctx)
		defer tPod.Cleanup(ctx)

		ginkgo.By("Waiting for the pod to be running")
		tPod.WaitForRunning(ctx)

		ginkgo.By("Writing a file to the GCS Fuse mount")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'gcs-fuse-data' > %v/gcs-data.txt", gcsFuseMountPath))

		ginkgo.By("Writing a file to the Lustre mount")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'lustre-data' > %v/lustre-data.txt", lustreMountPath))

		ginkgo.By("Verifying the file written to the GCS Fuse mount is readable back")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("grep 'gcs-fuse-data' %v/gcs-data.txt", gcsFuseMountPath))

		ginkgo.By("Verifying the file written to the Lustre mount is readable back")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("grep 'lustre-data' %v/lustre-data.txt", lustreMountPath))

		ginkgo.By("Verifying the GCS Fuse mount does not contain the Lustre-only file")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("test ! -f %v/lustre-data.txt", gcsFuseMountPath))

		ginkgo.By("Verifying the Lustre mount does not contain the GCS Fuse-only file")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("test ! -f %v/gcs-data.txt", lustreMountPath))

		ginkgo.By("Verifying both mounts remain healthy and writable")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName, fmt.Sprintf("mount | grep %v", gcsFuseMountPath))
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName, fmt.Sprintf("mount | grep %v", lustreMountPath))
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'gcs-fuse-data-2' >> %v/gcs-data.txt && echo 'lustre-data-2' >> %v/lustre-data.txt",
				gcsFuseMountPath, lustreMountPath))
	})
}

func ptrVolumeMode(m corev1.PersistentVolumeMode) *corev1.PersistentVolumeMode {
	return &m
}
