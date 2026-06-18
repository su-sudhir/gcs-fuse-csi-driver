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
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
	"local/test/e2e/specs"
)

// LustreStorageClass is the StorageClass used to dynamically provision
// Lustre-backed PVCs in the dual-volume tests. Overridden via
// --lustre-storage-class in e2e_test.go. If empty, the Lustre tests are
// skipped.
var LustreStorageClass = "lustre-rwx"

const (
	lustreCSIDriverName = "lustre.csi.storage.gke.io"
	lustreMountPath     = "/mnt/lustre"
	lustreVolName       = "lustre-vol"
	gcsFuseMountPath    = "/mnt/gcs"
	gcsFuseVolName      = "gcs-vol"
	lustrePVCSize       = "9000Gi"
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
		l.config.Prefix = specs.SkipCSIBucketAccessCheckPrefix
		l.gcsFuseResource = storageframework.CreateVolumeResource(ctx, driver, l.config, pattern, e2evolume.SizeRange{})
	}

	cleanup := func() {
		if l.gcsFuseResource != nil {
			framework.ExpectNoError(l.gcsFuseResource.CleanupResource(ctx))
		}
	}

	// skipIfLustreNotAvailable skips the test when the Lustre CSIDriver is not
	// installed on the cluster or no StorageClass is configured.
	skipIfLustreNotAvailable := func(testName string) {
		if LustreStorageClass == "" {
			e2eskipper.Skipf("--lustre-storage-class not set, skipping %s", testName)
		}
		if _, err := f.ClientSet.StorageV1().CSIDrivers().Get(ctx, lustreCSIDriverName, metav1.GetOptions{}); err != nil {
			e2eskipper.Skipf("%s CSIDriver not found, skipping %s: %v", lustreCSIDriverName, testName, err)
		}
	}

	// createLustrePVC dynamically provisions a Lustre-backed PVC via
	// LustreStorageClass and returns it with a cleanup func.
	createLustrePVC := func(namePrefix string) (*corev1.PersistentVolumeClaim, func()) {
		scName := LustreStorageClass
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: namePrefix,
				Namespace:    f.Namespace.Name,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				StorageClassName: &scName,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse(lustrePVCSize),
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

	// Same-pod dual mount + R/W: a Lustre PVC and a GCS Fuse volume are mounted
	// in a single pod. The test writes to and reads back from each mount
	// independently, verifying both volumes are accessible and writable
	// without conflict.
	ginkgo.It("should mount both a Lustre PVC and a GCS Fuse volume in a single pod and read/write independently on both", func() {
		skipIfLustreNotAvailable("same-pod dual mount + R/W test")

		init()
		defer cleanup()

		ginkgo.By("Creating a dynamically provisioned Lustre PVC")
		pvc, cleanupPVC := createLustrePVC("lustre-dual-pvc-")
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

	// Multi-pod shared RWX: two pods mount the same Lustre PVC (RWX) and the
	// same GCS Fuse bucket (RWX) concurrently. Pod-1 writes a dataset shard to
	// Lustre and a manifest to GCS; Pod-2 must see both files immediately.
	ginkgo.It("should allow two pods to share the same Lustre PVC (RWX) and GCS Fuse bucket (RWX) and see each other's writes", func() {
		skipIfLustreNotAvailable("multi-pod shared RWX test")

		init()
		defer cleanup()

		ginkgo.By("Creating a dynamically provisioned Lustre PVC")
		pvc, cleanupPVC := createLustrePVC("lustre-rwx-pvc-")
		defer cleanupPVC()

		ginkgo.By("Creating Pod-1 with both volumes")
		tPod1 := specs.NewTestPod(f.ClientSet, f.Namespace)
		tPod1.SetupVolume(l.gcsFuseResource, gcsFuseVolName, gcsFuseMountPath, false)
		tPod1.SetupVolume(&storageframework.VolumeResource{Pvc: pvc}, lustreVolName, lustreMountPath, false)
		tPod1.Create(ctx)
		defer tPod1.Cleanup(ctx)
		tPod1.WaitForRunning(ctx)

		ginkgo.By("Pod-1 writes a shard to Lustre and a manifest to GCS")
		tPod1.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'shard-data' > %v/shard.txt", lustreMountPath))
		tPod1.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'manifest-data' > %v/manifest.txt", gcsFuseMountPath))

		ginkgo.By("Creating Pod-2 mounting the same Lustre PVC and GCS bucket")
		tPod2 := specs.NewTestPod(f.ClientSet, f.Namespace)
		tPod2.SetupVolume(l.gcsFuseResource, gcsFuseVolName, gcsFuseMountPath, false)
		tPod2.SetupVolume(&storageframework.VolumeResource{Pvc: pvc}, lustreVolName, lustreMountPath, false)
		tPod2.Create(ctx)
		defer tPod2.Cleanup(ctx)
		tPod2.WaitForRunning(ctx)

		ginkgo.By("Pod-2 sees the shard written by Pod-1 on the Lustre mount")
		tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("grep 'shard-data' %v/shard.txt", lustreMountPath))

		ginkgo.By("Pod-2 sees the manifest written by Pod-1 on the GCS Fuse mount")
		tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("grep 'manifest-data' %v/manifest.txt", gcsFuseMountPath))

		ginkgo.By("Pod-2 writes back to both volumes")
		tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'pod2-shard' > %v/pod2-shard.txt", lustreMountPath))
		tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'pod2-manifest' > %v/pod2-manifest.txt", gcsFuseMountPath))

		ginkgo.By("Pod-1 sees the files written by Pod-2 on the Lustre mount")
		tPod1.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("grep 'pod2-shard' %v/pod2-shard.txt", lustreMountPath))

		ginkgo.By("Pod-1 sees the files written by Pod-2 on the GCS Fuse mount")
		tPod1.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("grep 'pod2-manifest' %v/pod2-manifest.txt", gcsFuseMountPath))
	})

	// Pod restart + persistence: data written to both volumes in Pod-1 survives
	// a pod deletion and is fully readable in a fresh Pod-2 that binds the same
	// Lustre PVC and GCS bucket.
	ginkgo.It("should persist data on both Lustre PVC and GCS Fuse volume across a pod restart", func() {
		skipIfLustreNotAvailable("pod restart persistence test")

		init()
		defer cleanup()

		ginkgo.By("Creating a dynamically provisioned Lustre PVC")
		pvc, cleanupPVC := createLustrePVC("lustre-persist-pvc-")
		defer cleanupPVC()

		ginkgo.By("Creating Pod-1 and writing data to both volumes")
		tPod1 := specs.NewTestPod(f.ClientSet, f.Namespace)
		tPod1.SetupVolume(l.gcsFuseResource, gcsFuseVolName, gcsFuseMountPath, false)
		tPod1.SetupVolume(&storageframework.VolumeResource{Pvc: pvc}, lustreVolName, lustreMountPath, false)
		tPod1.Create(ctx)
		tPod1.WaitForRunning(ctx)

		tPod1.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'persistent-lustre' > %v/persist.txt", lustreMountPath))
		tPod1.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'persistent-gcs' > %v/persist.txt", gcsFuseMountPath))

		ginkgo.By("Deleting Pod-1")
		tPod1.Cleanup(ctx)

		ginkgo.By("Creating Pod-2 mounting the same Lustre PVC and GCS bucket")
		tPod2 := specs.NewTestPod(f.ClientSet, f.Namespace)
		tPod2.SetupVolume(l.gcsFuseResource, gcsFuseVolName, gcsFuseMountPath, false)
		tPod2.SetupVolume(&storageframework.VolumeResource{Pvc: pvc}, lustreVolName, lustreMountPath, false)
		tPod2.Create(ctx)
		defer tPod2.Cleanup(ctx)
		tPod2.WaitForRunning(ctx)

		ginkgo.By("Verifying Lustre data persisted in Pod-2")
		tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("grep 'persistent-lustre' %v/persist.txt", lustreMountPath))

		ginkgo.By("Verifying GCS Fuse data persisted in Pod-2")
		tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("grep 'persistent-gcs' %v/persist.txt", gcsFuseMountPath))
	})

	// Node drain / reschedule remount: after draining the node running the
	// dual-mount pod, a replacement pod on a different node must remount both
	// volumes and find the data intact.
	ginkgo.It("should remount both Lustre PVC and GCS Fuse volume with data intact after the node is drained", func() {
		skipIfLustreNotAvailable("node drain remount test")

		init()
		defer cleanup()

		ginkgo.By("Creating a dynamically provisioned Lustre PVC")
		pvc, cleanupPVC := createLustrePVC("lustre-drain-pvc-")
		defer cleanupPVC()

		ginkgo.By("Creating the dual-mount pod and waiting for it to run")
		tPod1 := specs.NewTestPod(f.ClientSet, f.Namespace)
		tPod1.SetupVolume(l.gcsFuseResource, gcsFuseVolName, gcsFuseMountPath, false)
		tPod1.SetupVolume(&storageframework.VolumeResource{Pvc: pvc}, lustreVolName, lustreMountPath, false)
		tPod1.Create(ctx)
		tPod1.WaitForRunning(ctx)

		nodeName := tPod1.GetNode()
		framework.Logf("Pod-1 scheduled on node %s", nodeName)

		ginkgo.By("Writing data to both volumes from Pod-1")
		tPod1.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'drain-lustre' > %v/drain.txt", lustreMountPath))
		tPod1.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("echo 'drain-gcs' > %v/drain.txt", gcsFuseMountPath))

		ginkgo.By(fmt.Sprintf("Cordoning node %s", nodeName))
		cordonPatch := []byte(`{"spec":{"unschedulable":true}}`)
		_, err := f.ClientSet.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, cordonPatch, metav1.PatchOptions{})
		framework.ExpectNoError(err)
		defer func() {
			ginkgo.By(fmt.Sprintf("Uncordoning node %s", nodeName))
			uncordonPatch := []byte(`{"spec":{"unschedulable":false}}`)
			_, patchErr := f.ClientSet.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, uncordonPatch, metav1.PatchOptions{})
			framework.ExpectNoError(patchErr)
		}()

		ginkgo.By(fmt.Sprintf("Evicting Pod-1 from node %s", nodeName))
		eviction := &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tPod1.GetPodName(),
				Namespace: tPod1.GetPodNamespace(),
			},
		}
		err = f.ClientSet.PolicyV1().Evictions(tPod1.GetPodNamespace()).Evict(ctx, eviction)
		framework.ExpectNoError(err)

		ginkgo.By("Waiting for Pod-1 to be fully removed")
		tPod1.WaitForPodNotFoundInNamespace(ctx)

		ginkgo.By("Creating Pod-2 with anti-affinity for the drained node")
		tPod2 := specs.NewTestPod(f.ClientSet, f.Namespace)
		tPod2.SetupVolume(l.gcsFuseResource, gcsFuseVolName, gcsFuseMountPath, false)
		tPod2.SetupVolume(&storageframework.VolumeResource{Pvc: pvc}, lustreVolName, lustreMountPath, false)
		tPod2.SetNodeAffinity(nodeName, false)
		tPod2.Create(ctx)
		defer tPod2.Cleanup(ctx)
		tPod2.WaitForRunning(ctx)

		framework.Logf("Pod-2 rescheduled on node %s", tPod2.GetNode())

		ginkgo.By("Verifying Lustre data is intact on the new node")
		tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("grep 'drain-lustre' %v/drain.txt", lustreMountPath))

		ginkgo.By("Verifying GCS Fuse data is intact on the new node")
		tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("grep 'drain-gcs' %v/drain.txt", gcsFuseMountPath))

		ginkgo.By("Verifying both mounts are functional on the new node")
		tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("mount | grep %v", lustreMountPath))
		tPod2.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("mount | grep %v", gcsFuseMountPath))
	})

	// Large file / high-throughput transfer: writes a 1 GB file on Lustre,
	// copies it to GCS Fuse and back, verifies checksums match in both
	// directions, and confirms both mounts remain stable throughout.
	ginkgo.It("should transfer a 1GB file between Lustre and GCS Fuse mounts with matching checksums and no mount instability", func() {
		skipIfLustreNotAvailable("large file high-throughput transfer test")

		init()
		defer cleanup()

		ginkgo.By("Creating a dynamically provisioned Lustre PVC")
		pvc, cleanupPVC := createLustrePVC("lustre-largefile-pvc-")
		defer cleanupPVC()

		ginkgo.By("Configuring the pod with both GCS Fuse and Lustre volumes")
		tPod := specs.NewTestPod(f.ClientSet, f.Namespace)
		tPod.SetupVolume(l.gcsFuseResource, gcsFuseVolName, gcsFuseMountPath, false)
		tPod.SetupVolume(&storageframework.VolumeResource{Pvc: pvc}, lustreVolName, lustreMountPath, false)
		tPod.Create(ctx)
		defer tPod.Cleanup(ctx)
		tPod.WaitForRunning(ctx)

		ginkgo.By("Writing a 1 GB file to the Lustre mount")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("dd if=/dev/urandom of=%v/large.bin bs=1M count=1024 conv=fsync", lustreMountPath))

		ginkgo.By("Computing checksum of the source file on Lustre")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("md5sum %v/large.bin > /tmp/lustre.md5", lustreMountPath))

		ginkgo.By("Copying the 1 GB file from Lustre to GCS Fuse")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("cp %v/large.bin %v/large.bin", lustreMountPath, gcsFuseMountPath))

		ginkgo.By("Verifying the checksum of the file on GCS Fuse matches the Lustre source")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf(
				"md5sum %v/large.bin > /tmp/gcs.md5 && "+
					"LUSTRE_SUM=$(awk '{print $1}' /tmp/lustre.md5) && "+
					"GCS_SUM=$(awk '{print $1}' /tmp/gcs.md5) && "+
					"[ \"$LUSTRE_SUM\" = \"$GCS_SUM\" ]",
				gcsFuseMountPath))

		ginkgo.By("Copying the file back from GCS Fuse to Lustre")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("cp %v/large.bin %v/large-from-gcs.bin", gcsFuseMountPath, lustreMountPath))

		ginkgo.By("Verifying the round-trip checksum matches the original")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf(
				"md5sum %v/large-from-gcs.bin > /tmp/back.md5 && "+
					"LUSTRE_SUM=$(awk '{print $1}' /tmp/lustre.md5) && "+
					"BACK_SUM=$(awk '{print $1}' /tmp/back.md5) && "+
					"[ \"$LUSTRE_SUM\" = \"$BACK_SUM\" ]",
				lustreMountPath))

		ginkgo.By("Verifying both mounts remain stable after large transfers")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("mount | grep %v", lustreMountPath))
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("mount | grep %v", gcsFuseMountPath))
	})

	// Mixed I/O pattern: many small-file reads/writes on GCS Fuse run
	// concurrently with sequential large-file I/O on Lustre. Verifies no
	// cross-driver resource contention and that both mounts remain healthy.
	ginkgo.It("should handle concurrent small-file I/O on GCS Fuse and sequential large-file I/O on Lustre without contention", func() {
		skipIfLustreNotAvailable("mixed I/O pattern test")

		init()
		defer cleanup()

		ginkgo.By("Creating a dynamically provisioned Lustre PVC")
		pvc, cleanupPVC := createLustrePVC("lustre-mixedio-pvc-")
		defer cleanupPVC()

		ginkgo.By("Configuring the pod with both GCS Fuse and Lustre volumes")
		tPod := specs.NewTestPod(f.ClientSet, f.Namespace)
		tPod.SetupVolume(l.gcsFuseResource, gcsFuseVolName, gcsFuseMountPath, false)
		tPod.SetupVolume(&storageframework.VolumeResource{Pvc: pvc}, lustreVolName, lustreMountPath, false)
		tPod.Create(ctx)
		defer tPod.Cleanup(ctx)
		tPod.WaitForRunning(ctx)

		ginkgo.By("Running concurrent GCS Fuse small-file writes and Lustre sequential large-file I/O")
		// Launches 100 small-file writes to GCS Fuse in the background while
		// simultaneously performing a 512 MB sequential write + read on Lustre,
		// then waits for the GCS workload to finish before asserting exit codes.
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf(
				"bash -c 'set -e; "+
					"for i in $(seq 1 100); do echo \"small-${i}\" > %v/small-${i}.txt; done & GCS_PID=$!; "+
					"dd if=/dev/zero of=%v/seq.bin bs=1M count=512 conv=fsync; "+
					"dd if=%v/seq.bin of=/dev/null bs=1M; "+
					"wait $GCS_PID'",
				gcsFuseMountPath, lustreMountPath, lustreMountPath))

		ginkgo.By("Verifying a sample of small files written to GCS Fuse are readable")
		for _, i := range []int{1, 25, 50, 75, 100} {
			tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
				fmt.Sprintf("grep 'small-%d' %v/small-%d.txt", i, gcsFuseMountPath, i))
		}

		ginkgo.By("Verifying the sequential Lustre file is intact")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("test -f %v/seq.bin && stat -c%%s %v/seq.bin | grep -q 536870912",
				lustreMountPath, lustreMountPath))

		ginkgo.By("Verifying both mounts remain healthy after mixed I/O")
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("mount | grep %v", lustreMountPath))
		tPod.VerifyExecInPodSucceed(f, specs.TesterContainerName,
			fmt.Sprintf("mount | grep %v", gcsFuseMountPath))
	})
}
