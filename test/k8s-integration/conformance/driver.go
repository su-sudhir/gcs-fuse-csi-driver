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
// Unlike the full GCSFuseCSITestDriver in test/e2e/specs/, this driver:
//   - Does NOT set up Workload Identity or GCP Service Accounts (not available on OSS).
//   - Uses a single pre-existing GCS bucket with per-test subdirectory isolation
//     via gcsfuse's "only-dir=<uuid>" mount option.
//   - Sets skipCSIBucketAccessCheck=true so the CSI driver skips the IAM check
//     and relies on the GCE node's ADC (Application Default Credentials) for auth.
//
// Implements:
//   - storageframework.TestDriver                 (mandatory)
//   - storageframework.PreprovisionedPVTestDriver (enables PV/PVC test patterns)
//   - storageframework.EphemeralTestDriver        (enables CSI ephemeral volume test patterns)

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	e2eframework "k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
)

const (
	csiDriver          = "gcsfuse.csi.storage.gke.io"
	bucketNameEnv      = "BUCKET_NAME"
	keySkipBucketCheck = "skipCSIBucketAccessCheck"
	keyBucketName      = "bucketName"
	keyMountOptions    = "mountOptions"
)

// ossTestVolume holds the per-test state.
type ossTestVolume struct {
	bucket       string
	subDir       string
	implicitDirs bool
	nonRoot      bool
}

func (v *ossTestVolume) DeleteVolume(_ context.Context) {}

// ossDriver implements the required storageframework interfaces.
type ossDriver struct {
	driverInfo storageframework.DriverInfo
	bucket     string
}

// Compile-time interface checks.
var _ storageframework.TestDriver = &ossDriver{}
var _ storageframework.PreprovisionedPVTestDriver = &ossDriver{}
var _ storageframework.EphemeralTestDriver = &ossDriver{}

func initOSSDriver(bucket string) storageframework.TestDriver {
	return &ossDriver{
		bucket: bucket,
		driverInfo: storageframework.DriverInfo{
			Name:            csiDriver,
			SupportedFsType: sets.NewString(""),
			Capabilities: map[storageframework.Capability]bool{
				storageframework.CapPersistence: true,
				storageframework.CapExec:        true,
				storageframework.CapMultiPODs:   true,
				storageframework.CapRWX:         true,
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

func (d *ossDriver) SkipUnsupportedTest(pattern storageframework.TestPattern) {
	if pattern.VolType == storageframework.DynamicPV {
		e2eskipper.Skipf("GCS Fuse has no CSI provisioner — dynamic PV tests not supported")
	}
}

// serviceAccountName mirrors specs.K8sServiceAccountName.  On GKE this SA is
// created by Workload Identity setup; on OSS clusters we create it here so the
// pod admission controller doesn't reject the pod before the volume is mounted.
const serviceAccountName = "gcsfuse-csi-sa"

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
	return &storageframework.PerTestConfig{
		Driver:    d,
		Framework: f,
	}
}

// CreateVolume allocates a unique subdirectory in the shared bucket for isolation.
func (d *ossDriver) CreateVolume(_ context.Context, config *storageframework.PerTestConfig, volType storageframework.TestVolType) storageframework.TestVolume {
	switch volType {
	case storageframework.PreprovisionedPV, storageframework.CSIInlineVolume:
		return &ossTestVolume{
			bucket:       d.bucket,
			subDir:       uuid.NewString(),
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
			// Use "bucket:subDir" so each PV gets a unique VolumeHandle.  Without this,
			// two PVs from the same bucket share a handle and the kubelet skips the
			// second NodePublishVolume call, leaving one volume permanently unmounted.
			// The CSI driver's ParseVolumeID strips ":<suffix>" via regex /:.*$/,
			// so it still resolves to the correct bucket name.
			VolumeHandle: v.bucket + ":" + v.subDir,
			ReadOnly:     readOnly,
			VolumeAttributes: map[string]string{
				keyBucketName:      v.bucket,
				keySkipBucketCheck: "true",
				keyMountOptions:    mountOpts(v.subDir, readOnly, v.implicitDirs, v.nonRoot),
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
	subDir := uuid.NewString()
	implicitDirs := strings.Contains(config.Prefix, "implicit-dirs")
	nonRoot := strings.Contains(config.Prefix, "non-root")
	return map[string]string{
		keyBucketName:      d.bucket,
		keySkipBucketCheck: "true",
		keyMountOptions:    mountOpts(subDir, false, implicitDirs, nonRoot),
	}, true /* shared */, false /* readOnly */
}

func mountOpts(subDir string, readOnly bool, implicitDirs bool, nonRoot bool) string {
	opts := fmt.Sprintf("only-dir=%s,logging:severity:info", subDir)
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

// bucketFromEnv reads BUCKET_NAME or fatals — called once at suite start.
func bucketFromEnv() string {
	b := os.Getenv(bucketNameEnv)
	if b == "" {
		ginkgo.Fail(fmt.Sprintf("env var %s must be set to a pre-existing GCS bucket", bucketNameEnv))
	}
	return b
}
