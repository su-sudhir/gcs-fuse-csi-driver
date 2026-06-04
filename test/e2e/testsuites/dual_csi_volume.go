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

	// This tests the following configuration:
	//
	//           [pod]
	//           /   \
	//    [gcs-vol] [pd-vol]
	//        |         |
	//     [GCS]   [PD disk]
	//
	// Both volumes are mounted in the same pod. The test verifies that writes
	// and reads succeed on each mount independently, with no cross-mount bleed.
	ginkgo.It("should mount a GCS Fuse volume and a PD-backed PVC in the same pod and allow independent R/W on both", func() {
		// Skip when pd.csi.storage.gke.io is not installed (e.g. OSS clusters
		// without the GCE PD CSI driver, or non-GCP environments).
		_, err := f.ClientSet.StorageV1().CSIDrivers().Get(ctx, pdCSIDriverName, metav1.GetOptions{})
		if err != nil {
			e2eskipper.Skipf("%s CSIDriver not found, skipping dual-driver test: %v", pdCSIDriverName, err)
		}

		init()
		defer cleanup()

		ginkgo.By(fmt.Sprintf("Creating PD-backed PVC using StorageClass %q", PDStorageClass))
		scName := PDStorageClass
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "dual-csi-pd-pvc-",
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
		pvc, err = f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Create(ctx, pvc, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		defer func() {
			framework.ExpectNoError(f.ClientSet.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Delete(
				ctx, pvc.Name, metav1.DeleteOptions{}))
		}()

		ginkgo.By("Configuring the pod with both GCS Fuse and PD volumes")
		tPod := specs.NewTestPod(f.ClientSet, f.Namespace)

		// GCS Fuse volume via the standard VolumeResource.
		tPod.SetupVolume(l.gcsFuseResource, gcsFuseVolName, gcsFuseMountPath, false)

		// PD volume: wrap the PVC in a minimal VolumeResource. setupVolume()
		// only reads Pvc.Name when Pvc != nil, so this is safe.
		tPod.SetupVolume(&storageframework.VolumeResource{Pvc: pvc}, pdVolName, pdMountPath, false)

		ginkgo.By("Deploying the pod")
		tPod.Create(ctx)
		defer tPod.Cleanup(ctx)

		ginkgo.By("Waiting for the pod to be running")
		tPod.WaitForRunning(ctx)

		ginkgo.By("Writing and reading a file on the GCS Fuse volume")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'hello-gcs' > %v/gcs-test.txt && grep 'hello-gcs' %v/gcs-test.txt",
				gcsFuseMountPath, gcsFuseMountPath))

		ginkgo.By("Writing and reading a file on the PD volume")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'hello-pd' > %v/pd-test.txt && grep 'hello-pd' %v/pd-test.txt",
				pdMountPath, pdMountPath))

		ginkgo.By("Verifying the GCS file is not visible on the PD mount")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("test ! -f %v/gcs-test.txt", pdMountPath))

		ginkgo.By("Verifying the PD file is not visible on the GCS Fuse mount")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("test ! -f %v/pd-test.txt", gcsFuseMountPath))
	})
}
