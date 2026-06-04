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

// PDStorageClass is the StorageClass used to provision the PD-backed PVC in
// dual CSI volume tests. Overridden via --pd-storage-class in e2e_test.go.
var PDStorageClass = "standard-rwo"

const (
	pdCSIDriverName  = "pd.csi.storage.gke.io"
	gcsFuseMountPath = "/mnt/gcs"
	pdMountPath      = "/mnt/pd"
	gcsFuseVolName   = "gcs-vol"
	pdVolName        = "pd-vol"
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
			// PreprovisionedPV only: PD PVC lifecycle is managed manually here,
			// independent of the storageframework volume pattern.
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

	// createPDPVC creates a PD-backed PVC in the test namespace and returns it along
	// with a cleanup function that deletes the PVC after the test.
	createPDPVC := func(generateNamePrefix string) (*corev1.PersistentVolumeClaim, func()) {
		scName := PDStorageClass
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: generateNamePrefix,
				Namespace:    f.Namespace.Name,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: &scName,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("1Gi"),
					},
				},
			},
		}
		pvc, err := f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Create(ctx, pvc, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		cleanupFn := func() {
			framework.ExpectNoError(f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Delete(
				ctx, pvc.Name, metav1.DeleteOptions{}))
		}
		return pvc, cleanupFn
	}

	// GCS → PD data pipeline test
	//
	//           [pod]
	//           /   \
	//    [gcs-vol] [pd-vol]
	//        |         |
	//     [GCS]   [PD disk]
	//
	// A file is pre-seeded directly into the GCS bucket via the GCS API before
	// the pod starts. The pod mounts both volumes and copies the file from the
	// GCS mount to the PD mount. The test verifies that the file is readable on
	// the GCS mount and that the copy on the PD mount has identical content.
	ginkgo.It("should copy a pre-seeded file from the GCS Fuse mount to a PD-backed volume (GCS to PD data pipeline)", func() {
		_, err := f.ClientSet.StorageV1().CSIDrivers().Get(ctx, pdCSIDriverName, metav1.GetOptions{})
		if err != nil {
			e2eskipper.Skipf("%s CSIDriver not found, skipping GCS-to-PD pipeline test: %v", pdCSIDriverName, err)
		}

		init()
		defer cleanup()

		// Get the bucket name from the provisioned PV so we can pre-seed a file
		// into it via the GCS API before the pod starts.
		bucketName := l.gcsFuseResource.Pv.Spec.CSI.VolumeHandle
		const seedFileName = "pipeline-seed.txt"

		gcsfuseDriver, ok := driver.(*specs.GCSFuseCSITestDriver)
		if !ok {
			framework.Failf("driver is not *specs.GCSFuseCSITestDriver, cannot pre-seed GCS object")
		}

		ginkgo.By(fmt.Sprintf("Pre-seeding file %q in GCS bucket %q via the GCS API", seedFileName, bucketName))
		gcsfuseDriver.CreateTestFileInBucket(ctx, seedFileName, bucketName)

		ginkgo.By(fmt.Sprintf("Creating PD-backed PVC using StorageClass %q", PDStorageClass))
		pvc, cleanupPVC := createPDPVC("pipeline-pd-pvc-")
		defer cleanupPVC()

		ginkgo.By("Configuring the pod with both GCS Fuse and PD volumes")
		tPod := specs.NewTestPod(f.ClientSet, f.Namespace)
		tPod.SetupVolume(l.gcsFuseResource, gcsFuseVolName, gcsFuseMountPath, false)
		tPod.SetupVolume(&storageframework.VolumeResource{Pvc: pvc}, pdVolName, pdMountPath, false)

		ginkgo.By("Deploying the pod")
		tPod.Create(ctx)
		defer tPod.Cleanup(ctx)

		ginkgo.By("Waiting for the pod to be running")
		tPod.WaitForRunning(ctx)

		ginkgo.By("Verifying the pre-seeded file is readable on the GCS Fuse mount")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("grep %q %v/%v", seedFileName, gcsFuseMountPath, seedFileName))

		ginkgo.By("Copying the file from the GCS Fuse mount to the PD mount")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("cp %v/%v %v/%v", gcsFuseMountPath, seedFileName, pdMountPath, seedFileName))

		ginkgo.By("Verifying the copied file on the PD mount has identical content")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("diff %v/%v %v/%v", gcsFuseMountPath, seedFileName, pdMountPath, seedFileName))
	})
}
