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

// Package conformance runs the GCS Fuse CSI driver test suites (including
// PreprovisionedPV patterns) on an OSS kubeadm cluster — without any GKE
// cluster-name or Workload Identity requirements.
//
// Unlike test/e2e/e2e_test.go which validates that the kubeconfig context
// starts with "gke_" and parses project/zone from the context name, this
// entry point accepts --project and --zone as explicit flags so it works on
// any cluster (OSS kubeadm, kind, etc.) whose nodes have GCS access via ADC.
//
// Usage (on OSS cluster master or any machine with KUBECONFIG set):
//
//	go test -v ./test/k8s-integration/conformance/ \
//	  --project=prod-y-in2508995 \
//	  --zone=us-central1-a \
//	  --bucket-location=us-central1 \
//	  BUCKET_NAME=my-bucket ./gcsfuse-conformance.test ...
//
// Or build a binary first:
//
//	go test -c -o gcsfuse-conformance.test ./test/k8s-integration/conformance/
//	BUCKET_NAME=my-bucket ./gcsfuse-conformance.test --project=... --zone=...
package conformance

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"local/test/e2e/testsuites"
	"local/test/e2e/utils"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/test/e2e/framework"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
)

var (
	// GCP flags — required on OSS clusters since we cannot parse them from
	// the GKE kubeconfig context name.
	project     = flag.String("project", "", "GCP project ID (used to set env vars for testsuites)")
	zone        = flag.String("zone", "", "GCE zone of the cluster nodes (e.g. us-central1-a)")
	clusterName = flag.String("cluster-name", "oss-cluster", "Cluster name used for metadata (informational only)")
)

// init runs once before any tests to configure the e2e framework and
// set env vars that testsuites read at DefineTests time.
var _ = func() bool {
	testing.Init()

	if os.Getenv(clientcmd.RecommendedConfigPathEnvVar) == "" {
		kubeconfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")
		os.Setenv(clientcmd.RecommendedConfigPathEnvVar, kubeconfig)
	}

	framework.RegisterCommonFlags(flag.CommandLine)
	framework.RegisterClusterFlags(flag.CommandLine)
	flag.Parse()
	framework.AfterReadingAllFlags(&framework.TestContext)

	if *project == "" {
		klog.Fatalf("--project must be set (GCP project ID)")
	}
	if *zone == "" {
		klog.Fatalf("--zone must be set (GCE zone, e.g. us-central1-a)")
	}

	// Several testsuites read boolean env vars at DefineTests time and fatal if
	// they are unset or non-boolean. Set safe OSS defaults so callers only need
	// to override the ones they care about.
	setDefaultEnv := func(key, val string) {
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
	// Native sidecar (k8s >= 1.29 feature gate). Default off for k8s 1.28.
	setDefaultEnv(utils.TestWithNativeSidecarEnvVar, "false")
	// SA volume injection requires GKE Workload Identity — not available on OSS.
	setDefaultEnv(utils.TestWithSAVolumeInjectionEnvVar, "false")
	// Sidecar bucket access check — enabled now that each test namespace's
	// gcsfuse-csi-sa KSA is bound to its bucket via WIF.
	setDefaultEnv(utils.TestWithSidecarBucketAccessCheckEnvVar, "true")
	// Error file clean-up — safe to enable on OSS.
	setDefaultEnv(utils.TestWithErrorFileCleanUpEnvVar, "true")
	// Cluster metadata env vars used by some testsuites for labels/logging.
	setDefaultEnv(utils.IsOSSEnvVar, "true")
	setDefaultEnv(utils.IsZBEnabledEnvVar, "false")
	setDefaultEnv(utils.ProjectEnvVar, *project)
	setDefaultEnv(utils.ClusterLocationEnvVar, *zone)
	setDefaultEnv(utils.ClusterNameEnvVar, *clusterName)
	// GCS Fuse version string used for feature-flag detection in testsuites.
	// On OSS we cannot query the sidecar binary version; use a recent GKE
	// release version so all feature gates are enabled.
	setDefaultEnv(utils.GcsfuseVersionVarName, "v3.8.0-gke.0")
	testsuites.GCSFuseVersionStr = os.Getenv(utils.GcsfuseVersionVarName)

	return true
}()

func TestConformance(t *testing.T) {
	t.Parallel()

	gomega.RegisterFailHandler(framework.Fail)
	if framework.TestContext.ReportDir != "" {
		if err := os.MkdirAll(framework.TestContext.ReportDir, 0o755); err != nil {
			klog.Errorf("Failed creating report directory: %v", err)
		}
	}

	suiteConfig, reporterConfig := framework.CreateGinkgoConfig()
	klog.Infof("Starting conformance run %q on Ginkgo node %d", framework.RunID, suiteConfig.ParallelProcess)
	ginkgo.RunSpecs(t, "GCS Fuse CSI Driver Conformance", suiteConfig, reporterConfig)
}

var _ = ginkgo.Describe("Conformance Test Suite", func() {
	// ossDriver creates a fresh GCS bucket per test namespace using the
	// GCE node's ADC (Application Default Credentials) for bucket
	// management.
	testDriver := initOSSDriver(*project)

	// Testsuites that exercise the core volume lifecycle including
	// PreprovisionedPV, CSI ephemeral, and multi-volume patterns.
	//
	// Excluded (require GKE-specific infrastructure):
	//   - InitGcsFuseMountTestSuite: needs TEST_WITH_SA_VOL_INJECTION (Workload Identity)
	//   - InitGcsFuseCSIFailedMountTestSuite: needs SA injection + error-file cleanup paths
	//   - InitGcsFuseCSIWorkloadsTestSuite, OIDC, Profiles, Metrics, Istio, WIF: GKE-only
	conformanceSuites := []func() storageframework.TestSuite{
		testsuites.InitGcsFuseCSIVolumesTestSuite,
		testsuites.InitGcsFuseCSIMultiVolumeTestSuite,
		testsuites.InitGcsFuseCSISubPathTestSuite,
	}

	ginkgo.Context(fmt.Sprintf("[Driver: %s]", testDriver.GetDriverInfo().Name), func() {
		storageframework.DefineTestSuites(testDriver, conformanceSuites)
	})
})
