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
	"sync"

	"local/test/e2e/specs"

	"github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
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

	// ── Test 1: Concurrent writes from two pods to a shared GCS Fuse bucket ────
	// and their respective isolated PD volumes
	//
	// Two pods run simultaneously, each writing different files at the same
	// time to the shared GCS Fuse bucket mount and to its own PD-backed volume.
	// The test verifies neither driver corrupts data or surfaces mount errors
	// under concurrent load, and that every file lands intact on both the
	// shared GCS bucket and the writer's own PD volume.
	ginkgo.It("should support concurrent writes from two pods to a shared GCS Fuse bucket and their respective PD volumes without data corruption or mount errors", func() {
		skipIfPDCSINotInstalled("concurrent writes test")

		init()
		defer cleanup()

		pvc1, cleanupPVC1 := createPDPVC("concurrent-pd-pvc-1-", "5Gi")
		defer cleanupPVC1()
		pvc2, cleanupPVC2 := createPDPVC("concurrent-pd-pvc-2-", "5Gi")
		defer cleanupPVC2()

		ginkgo.By("Deploying the first pod with the shared GCS Fuse volume and its own PD volume")
		tPod1 := specs.NewTestPod(f.ClientSet, f.Namespace)
		tPod1.SetupVolume(l.gcsFuseResource, gcsFuseVolName, gcsFuseMountPath, false)
		tPod1.SetupVolume(&storageframework.VolumeResource{Pvc: pvc1}, pdVolName, pdMountPath, false)
		tPod1.Create(ctx)
		defer tPod1.Cleanup(ctx)
		tPod1.WaitForRunning(ctx)

		ginkgo.By("Deploying the second pod with the shared GCS Fuse volume and its own PD volume")
		tPod2 := specs.NewTestPod(f.ClientSet, f.Namespace)
		tPod2.SetupVolume(l.gcsFuseResource, gcsFuseVolName, gcsFuseMountPath, false)
		tPod2.SetupVolume(&storageframework.VolumeResource{Pvc: pvc2}, pdVolName, pdMountPath, false)
		tPod2.Create(ctx)
		defer tPod2.Cleanup(ctx)
		tPod2.WaitForRunning(ctx)

		ginkgo.By("Concurrently writing different files from both pods to the shared GCS Fuse mount and their own PD volumes")
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			defer ginkgo.GinkgoRecover()
			tPod1.VerifyExecInPodSucceed(f, specs.TesterContainerName,
				fmt.Sprintf("echo 'pod1-gcs-data' > %v/concurrent-pod1.txt && echo 'pod1-pd-data' > %v/pod1-data.txt",
					gcsFuseMountPath, pdMountPath))
		}()

		go func() {
			defer wg.Done()
			defer ginkgo.GinkgoRecover()
			tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName,
				fmt.Sprintf("echo 'pod2-gcs-data' > %v/concurrent-pod2.txt && echo 'pod2-pd-data' > %v/pod2-data.txt",
					gcsFuseMountPath, pdMountPath))
		}()

		wg.Wait()

		ginkgo.By("Verifying both pods' GCS Fuse mounts remain healthy after concurrent writes")
		tPod1.VerifyExecInPodSucceed(f, specs.TesterContainerName, fmt.Sprintf("mount | grep %v | grep rw,", gcsFuseMountPath))
		tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName, fmt.Sprintf("mount | grep %v | grep rw,", gcsFuseMountPath))

		ginkgo.By("Verifying each pod can read its own file back from the shared GCS bucket without corruption")
		tPod1.VerifyExecInPodSucceed(f, specs.TesterContainerName, fmt.Sprintf("grep 'pod1-gcs-data' %v/concurrent-pod1.txt", gcsFuseMountPath))
		tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName, fmt.Sprintf("grep 'pod2-gcs-data' %v/concurrent-pod2.txt", gcsFuseMountPath))

		ginkgo.By("Verifying each pod can see the other pod's file in the shared GCS bucket with uncorrupted content")
		tPod1.VerifyExecInPodSucceed(f, specs.TesterContainerName, fmt.Sprintf("grep 'pod2-gcs-data' %v/concurrent-pod2.txt", gcsFuseMountPath))
		tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName, fmt.Sprintf("grep 'pod1-gcs-data' %v/concurrent-pod1.txt", gcsFuseMountPath))

		ginkgo.By("Verifying each pod's own PD volume holds its uncorrupted data")
		tPod1.VerifyExecInPodSucceed(f, specs.TesterContainerName, fmt.Sprintf("grep 'pod1-pd-data' %v/pod1-data.txt", pdMountPath))
		tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName, fmt.Sprintf("grep 'pod2-pd-data' %v/pod2-data.txt", pdMountPath))
	})

	// ── Test 2: Multi-pod sharing of a GCS Fuse volume with isolated PD volumes ─
	//
	// Two pods run simultaneously, both mounting the same GCS Fuse volume (RWX)
	// while each also has its own PD-backed PVC (RWO). The test verifies a
	// write from one pod to the shared GCS volume becomes visible from the
	// other, while each pod's PD volume stays isolated and invisible to the
	// other pod.
	ginkgo.It("should share GCS Fuse volume writes across two pods while keeping their individual PD volumes isolated", func() {
		skipIfPDCSINotInstalled("multi-pod sharing test")

		init()
		defer cleanup()

		pvc1, cleanupPVC1 := createPDPVC("sharing-pd-pvc-1-", "5Gi")
		defer cleanupPVC1()
		pvc2, cleanupPVC2 := createPDPVC("sharing-pd-pvc-2-", "5Gi")
		defer cleanupPVC2()

		ginkgo.By("Deploying the first pod with the shared GCS Fuse volume and its own PD volume")
		tPod1 := specs.NewTestPod(f.ClientSet, f.Namespace)
		tPod1.SetupVolume(l.gcsFuseResource, gcsFuseVolName, gcsFuseMountPath, false)
		tPod1.SetupVolume(&storageframework.VolumeResource{Pvc: pvc1}, pdVolName, pdMountPath, false)
		tPod1.Create(ctx)
		defer tPod1.Cleanup(ctx)
		tPod1.WaitForRunning(ctx)

		ginkgo.By("Deploying the second pod with the same shared GCS Fuse volume and its own PD volume")
		tPod2 := specs.NewTestPod(f.ClientSet, f.Namespace)
		tPod2.SetupVolume(l.gcsFuseResource, gcsFuseVolName, gcsFuseMountPath, false)
		tPod2.SetupVolume(&storageframework.VolumeResource{Pvc: pvc2}, pdVolName, pdMountPath, false)
		tPod2.Create(ctx)
		defer tPod2.Cleanup(ctx)
		tPod2.WaitForRunning(ctx)

		ginkgo.By("Writing a file to the shared GCS Fuse volume from the first pod")
		tPod1.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'shared-from-pod1' > %v/shared.txt", gcsFuseMountPath))

		ginkgo.By("Verifying the second pod can see the file written by the first pod on the shared GCS volume")
		tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("grep 'shared-from-pod1' %v/shared.txt", gcsFuseMountPath))

		ginkgo.By("Writing a file to the shared GCS Fuse volume from the second pod")
		tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'shared-from-pod2' > %v/shared-back.txt", gcsFuseMountPath))

		ginkgo.By("Verifying the first pod can see the file written by the second pod on the shared GCS volume")
		tPod1.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("grep 'shared-from-pod2' %v/shared-back.txt", gcsFuseMountPath))

		ginkgo.By("Writing a file to each pod's own isolated PD volume")
		tPod1.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'pod1-private-pd-data' > %v/private.txt", pdMountPath))
		tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'pod2-private-pd-data' > %v/private.txt", pdMountPath))

		ginkgo.By("Verifying each pod's PD volume is isolated and not visible to the other pod")
		tPod1.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("grep 'pod1-private-pd-data' %v/private.txt && ! grep -q 'pod2-private-pd-data' %v/private.txt",
				pdMountPath, pdMountPath))
		tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("grep 'pod2-private-pd-data' %v/private.txt && ! grep -q 'pod1-private-pd-data' %v/private.txt",
				pdMountPath, pdMountPath))
	})
}
