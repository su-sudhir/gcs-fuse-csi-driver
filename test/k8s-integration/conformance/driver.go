/*
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

package conformance

// ossDriver is a minimal GCS Fuse test driver for OSS (non-GKE) clusters.
//
// Unlike the full GCSFuseCSITestDriver in test/e2e/specs/, this driver
// creates a dedicated GCS bucket per test namespace and binds that
// namespace's gcsfuse-csi-sa KSA to it via Workload Identity Federation,
// rather than relying on GKE-managed Workload Identity.
//
// Implements:
//   - storageframework.TestDriver                 (mandatory)
//   - storageframework.PreprovisionedPVTestDriver (enables PV/PVC test patterns)
//   - storageframework.EphemeralTestDriver        (enables CSI ephemeral volume test patterns)

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"local/test/e2e/utils"

	"cloud.google.com/go/iam"
	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"github.com/onsi/ginkgo/v2"
	"google.golang.org/api/iterator"
	storagev1 "k8s.io/api/storage/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	e2eframework "k8s.io/kubernetes/test/e2e/framework"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
)

const (
	csiDriver          = "gcsfuse.csi.storage.gke.io"
	keyBucketName      = "bucketName"
	keyMountOptions    = "mountOptions"
	testBucketLocation = "us-central1"
	implicitDirObject  = "implicit-dir/placeholder"

	// WIF pool used by test namespace KSAs to authenticate as GCS principals.
	// Override via env vars for other clusters.
	wifPoolIDEnv        = "WIF_POOL_ID"
	defaultWIFPoolID    = "wi-pool-k8s-cluster"
	wifProjectNumberEnv = "WIF_PROJECT_NUMBER"
	defaultWIFProjectNo = "202500739588"
)

// ossTestVolume holds the per-test state.
type ossTestVolume struct {
	bucket string
	// handleSuffix disambiguates the VolumeHandle when a single test creates
	// more than one volume against the same per-test bucket. Without this,
	// two PVs sharing a handle cause the kubelet to skip the second
	// NodePublishVolume call, leaving one volume permanently unmounted.
	handleSuffix string
	implicitDirs bool
	nonRoot      bool
}

func (v *ossTestVolume) DeleteVolume(_ context.Context) {}

// ossDriver implements the required storageframework interfaces.
type ossDriver struct {
	driverInfo storageframework.DriverInfo
	gcsClient  *storage.Client
	projectID  string
	buckets    map[string]string // namespace -> per-test bucket name
}

// Compile-time interface checks.
var _ storageframework.TestDriver = &ossDriver{}
var _ storageframework.PreprovisionedPVTestDriver = &ossDriver{}
var _ storageframework.EphemeralTestDriver = &ossDriver{}
var _ storageframework.DynamicPVTestDriver = &ossDriver{}

func initOSSDriver(projectID string) storageframework.TestDriver {
	gcsClient, err := storage.NewClient(context.Background())
	if err != nil {
		ginkgo.Fail(fmt.Sprintf("failed to create GCS client: %v", err))
	}
	return &ossDriver{
		gcsClient: gcsClient,
		projectID: projectID,
		buckets:   map[string]string{},
		driverInfo: storageframework.DriverInfo{
			Name:            csiDriver,
			SupportedFsType: sets.NewString(""),
			Capabilities: map[storageframework.Capability]bool{
				storageframework.CapPersistence: true,
				storageframework.CapExec:        true,
				storageframework.CapMultiPODs:   true,
				storageframework.CapRWX:         true,
				// The driver declares VOLUME_MOUNT_GROUP at the CSI protocol
				// level (pkg/csi_driver/gcs_fuse_driver.go), the real
				// mechanism this capability exercises.
				storageframework.CapFsGroup: true,
				// The driver's declared access modes don't include
				// SINGLE_NODE_SINGLE_WRITER/SINGLE_NODE_MULTI_WRITER (the
				// modes RWOP needs), so this is expected to surface a real
				// "unsupported access mode" failure rather than pass.
				storageframework.CapReadWriteOncePod: true,
			},
			SupportedSizeRange: e2evolume.SizeRange{
				Min: "1Ki",
			},
		},
	}
}

func (d *ossDriver) GetDriverInfo() *storageframework.DriverInfo {
	return &d.driverInfo
}

func (d *ossDriver) SkipUnsupportedTest(_ storageframework.TestPattern) {}

// provisionerSecretName is the k8s Secret name the dynamically-provisioned
// StorageClass points the CSI controller's CreateVolume/DeleteVolume calls at.
const provisionerSecretName = "gcsfuse-provisioner-secret"

// GetDynamicProvisionStorageClass satisfies storageframework.DynamicPVTestDriver.
// GCS Fuse's CreateVolume RPC provisions a brand-new bucket per PVC, which
// needs project-level bucket create/delete (roles/storage.admin) — a bigger
// grant than the bucket-scoped objectAdmin PrepareTest binds for
// pre-provisioned/ephemeral volumes. Bind it here, per test, and revoke it in
// DeferCleanup, mirroring the GKE Workload Identity pattern in
// test/e2e/specs/testdriver.go's GetDynamicProvisionStorageClass.
func (d *ossDriver) GetDynamicProvisionStorageClass(ctx context.Context, config *storageframework.PerTestConfig, _ string) *storagev1.StorageClass {
	namespace := config.Framework.Namespace.Name
	member := fmt.Sprintf(
		"principal://iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s/subject/system:serviceaccount:%s:%s",
		wifProjectNumber(), wifPoolID(), namespace, serviceAccountName)

	binding := utils.NewTestGCPProjectIAMPolicyBinding(d.projectID, member, "roles/storage.admin", "")
	binding.Create(ctx)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: provisionerSecretName},
		StringData: map[string]string{
			"projectID":               d.projectID,
			"serviceAccountName":      serviceAccountName,
			"serviceAccountNamespace": namespace,
		},
		Type: corev1.SecretTypeOpaque,
	}
	// Multi-volume tests call this once per volume in the same namespace, so
	// the secret (same content every time) may already exist.
	if _, err := config.Framework.ClientSet.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		e2eframework.Failf("failed to create provisioner secret %s/%s: %v", namespace, provisionerSecretName, err)
	}

	ginkgo.DeferCleanup(func(ctx context.Context) {
		if err := config.Framework.ClientSet.CoreV1().Secrets(namespace).Delete(ctx, provisionerSecretName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			e2eframework.Logf("failed to delete provisioner secret %s/%s: %v", namespace, provisionerSecretName, err)
		}
		binding.Cleanup(ctx)
	})

	parameters := map[string]string{
		"csi.storage.k8s.io/provisioner-secret-name":      provisionerSecretName,
		"csi.storage.k8s.io/provisioner-secret-namespace": "${pvc.namespace}",
	}
	bindingMode := storagev1.VolumeBindingWaitForFirstConsumer

	return &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{GenerateName: "gcsfuse-oss-dynamic-sc-"},
		Provisioner:       csiDriver,
		Parameters:        parameters,
		VolumeBindingMode: &bindingMode,
	}
}

// serviceAccountName is "default" because the upstream storage testsuites we
// run (test/e2e/framework/pod, volume/fixtures.go, etc.) create test pods
// with no explicit spec.serviceAccountName, which the k8s API server then
// defaults to the namespace's "default" ServiceAccount. Since we don't
// control pod creation in these generic upstream helpers (unlike our own
// removed custom suites, which explicitly set serviceAccountName), WIF
// bindings must target "default" — anything else never applies to the pods
// that actually mount volumes, causing NodePublishVolume PermissionDenied.
const serviceAccountName = "default"

func (d *ossDriver) PrepareTest(ctx context.Context, f *e2eframework.Framework) *storageframework.PerTestConfig {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceAccountName,
			Namespace: f.Namespace.Name,
		},
	}
	_, err := f.ClientSet.CoreV1().ServiceAccounts(f.Namespace.Name).Create(ctx, sa, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		e2eframework.Failf("failed to create SA %s/%s: %v", f.Namespace.Name, serviceAccountName, err)
	}

	bucket := d.createBucket(ctx, f.Namespace.Name)
	seedBucket(ctx, d.gcsClient, bucket)
	bindWorkloadIdentity(ctx, d.gcsClient, bucket, f.Namespace.Name)
	d.buckets[f.Namespace.Name] = bucket
	ginkgo.DeferCleanup(func(ctx context.Context) {
		deleteBucket(ctx, d.gcsClient, bucket)
	})

	return &storageframework.PerTestConfig{
		Driver: d,
		// PerTestConfig.Prefix is inserted as the first part of dynamically
		// generated pod/path names (e.g. "<prefix>-injector"). Left empty, it
		// produces invalid names like "-injector" and every test that injects
		// content into a pre-provisioned volume fails outright.
		Prefix:    "gcsfuse-oss",
		Framework: f,
	}
}

// createBucket creates a per-test GCS bucket for the given namespace.
//
// GCS bucket names are capped at 63 characters, so this can't simply
// concatenate <project>-gcsfuse-test-<namespace>-<uuid>: the project ID,
// namespace, and a full UUID alone routinely exceed that. Truncate the
// namespace and shorten the UUID to a random 8-character suffix, which is
// still unique enough to avoid collisions within a single test run.
func (d *ossDriver) createBucket(ctx context.Context, namespace string) string {
	ns := namespace
	if len(ns) > 20 {
		ns = ns[:20]
	}
	bucketName := fmt.Sprintf("gcsfuse-oss-%s-%s", ns, uuid.NewString()[:8])
	if err := d.gcsClient.Bucket(bucketName).Create(ctx, d.projectID, &storage.BucketAttrs{Location: testBucketLocation}); err != nil {
		e2eframework.Failf("failed to create bucket %q: %v", bucketName, err)
	}
	return bucketName
}

// seedBucket writes a placeholder object so gcsfuse's --implicit-dirs option
// sees implicit-dir as an existing directory from the start of the test.
func seedBucket(ctx context.Context, client *storage.Client, bucket string) {
	w := client.Bucket(bucket).Object(implicitDirObject).NewWriter(ctx)
	if err := w.Close(); err != nil {
		e2eframework.Failf("failed to seed bucket %q: %v", bucket, err)
	}
}

// bindWorkloadIdentity grants the test namespace's gcsfuse-csi-sa KSA
// (authenticating via the cluster's WIF pool) objectAdmin on the bucket, so
// the CSI driver's IAM access check passes without relying on node ADC.
func bindWorkloadIdentity(ctx context.Context, client *storage.Client, bucket, namespace string) {
	member := fmt.Sprintf(
		"principal://iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s/subject/system:serviceaccount:%s:%s",
		wifProjectNumber(), wifPoolID(), namespace, serviceAccountName)

	bh := client.Bucket(bucket)
	policy, err := bh.IAM().Policy(ctx)
	if err != nil {
		e2eframework.Failf("failed to get IAM policy for bucket %q: %v", bucket, err)
	}
	policy.Add(member, iam.RoleName("roles/storage.objectAdmin"))
	if err := bh.IAM().SetPolicy(ctx, policy); err != nil {
		e2eframework.Failf("failed to set IAM policy for bucket %q: %v", bucket, err)
	}
}

func wifPoolID() string {
	if v := os.Getenv(wifPoolIDEnv); v != "" {
		return v
	}
	return defaultWIFPoolID
}

func wifProjectNumber() string {
	if v := os.Getenv(wifProjectNumberEnv); v != "" {
		return v
	}
	return defaultWIFProjectNo
}

// deleteBucket deletes all objects in the bucket, then the bucket itself.
func deleteBucket(ctx context.Context, client *storage.Client, bucket string) {
	it := client.Bucket(bucket).Objects(ctx, nil)
	for {
		obj, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			e2eframework.Logf("failed to list objects in bucket %q during cleanup: %v", bucket, err)
			return
		}
		if err := client.Bucket(bucket).Object(obj.Name).Delete(ctx); err != nil {
			e2eframework.Logf("failed to delete object %q in bucket %q: %v", obj.Name, bucket, err)
		}
	}
	if err := client.Bucket(bucket).Delete(ctx); err != nil {
		e2eframework.Logf("failed to delete bucket %q: %v", bucket, err)
	}
}

// CreateVolume returns a volume pointing at the calling test's per-namespace bucket.
func (d *ossDriver) CreateVolume(_ context.Context, config *storageframework.PerTestConfig, volType storageframework.TestVolType) storageframework.TestVolume {
	switch volType {
	case storageframework.PreprovisionedPV, storageframework.CSIInlineVolume:
		return &ossTestVolume{
			bucket:       d.buckets[config.Framework.Namespace.Name],
			handleSuffix: uuid.NewString(),
			implicitDirs: strings.Contains(config.Prefix, "implicit-dirs"),
			nonRoot:      strings.Contains(config.Prefix, "non-root"),
		}
	default:
		e2eframework.Failf("unsupported volume type: %v", volType)
		return nil
	}
}

// GetPersistentVolumeSource returns the CSI PV source for a pre-provisioned volume.
func (d *ossDriver) GetPersistentVolumeSource(readOnly bool, _ string, vol storageframework.TestVolume) (*corev1.PersistentVolumeSource, *corev1.VolumeNodeAffinity) {
	v := vol.(*ossTestVolume)
	return &corev1.PersistentVolumeSource{
		CSI: &corev1.CSIPersistentVolumeSource{
			Driver: csiDriver,
			// The CSI driver's ParseVolumeID strips ":<suffix>" via regex /:.*$/,
			// so this still resolves to the correct bucket name.
			VolumeHandle: v.bucket + ":" + v.handleSuffix,
			ReadOnly:     readOnly,
			VolumeAttributes: map[string]string{
				keyBucketName:   v.bucket,
				keyMountOptions: mountOpts(readOnly, v.implicitDirs, v.nonRoot),
			},
		},
	}, nil
}

// GetCSIDriverName satisfies EphemeralTestDriver.
func (d *ossDriver) GetCSIDriverName(_ *storageframework.PerTestConfig) string {
	return csiDriver
}

// GetVolume returns inline CSI ephemeral volume attributes (EphemeralTestDriver).
func (d *ossDriver) GetVolume(config *storageframework.PerTestConfig, _ int) (map[string]string, bool, bool) {
	implicitDirs := strings.Contains(config.Prefix, "implicit-dirs")
	nonRoot := strings.Contains(config.Prefix, "non-root")
	return map[string]string{
		keyBucketName:   d.buckets[config.Framework.Namespace.Name],
		keyMountOptions: mountOpts(false, implicitDirs, nonRoot),
	}, true /* shared */, false /* readOnly */
}

func mountOpts(readOnly, implicitDirs, nonRoot bool) string {
	// file-mode=755 grants the execute bit; gcsfuse's own default file-mode
	// has no execute bit, which fails the upstream "should allow exec of
	// files on the volume" test (files can be written but not chmod'd +
	// executed).
	opts := "logging:severity:info,file-mode=755"
	if readOnly {
		opts += ",ro"
	}
	if implicitDirs {
		opts += ",implicit-dirs"
	}
	if nonRoot {
		// Mirror GKE driver: mount files as uid=1001/gid=2002 so the non-root
		// pod (RunAsUser=1001, RunAsGroup=2002) can write to the volume.
		opts += ",uid=1001,gid=2002"
	}
	return opts
}
