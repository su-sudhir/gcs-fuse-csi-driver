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

// Package conformance runs the real upstream Kubernetes external-storage
// testsuites (k8s.io/kubernetes/test/e2e/storage/testsuites) against the GCS
// Fuse CSI driver on an OSS kubeadm cluster, using ossDriver — a Go
// storageframework.TestDriver implementing PreprovisionedPVTestDriver and
// EphemeralTestDriver — instead of the driver-definition YAML mechanism
// (test/k8s-integration/run.sh), which only supports CSI ephemeral volumes.
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
//	  --zone=us-central1-a
//
// Or build a binary first:
//
//	go test -c -o gcsfuse-conformance.test ./test/k8s-integration/conformance/
//	./gcsfuse-conformance.test --project=... --zone=...
package conformance

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/test/e2e/framework"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	storagetestsuites "k8s.io/kubernetes/test/e2e/storage/testsuites"
)

var (
	// GCP flags — required on OSS clusters since we cannot parse them from
	// the GKE kubeconfig context name.
	project = flag.String("project", "", "GCP project ID (used for per-test bucket creation)")
	zone    = flag.String("zone", "", "GCE zone of the cluster nodes (e.g. us-central1-a)")
)

// init runs once before any tests to configure the e2e framework.
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

	// The real upstream Kubernetes external-storage testsuites, run against
	// ossDriver directly (no driver-definition YAML, so PreprovisionedPV
	// patterns work here unlike test/k8s-integration/run.sh). ossDriver only
	// declares CapPersistence/CapExec/CapMultiPODs/CapRWX and implements
	// TestDriver/PreprovisionedPVTestDriver/EphemeralTestDriver — suites or
	// patterns needing capabilities it doesn't declare (snapshots, resize,
	// topology, dynamic provisioning, volume limits) self-skip via each
	// suite's own SkipUnsupportedTests/pattern checks, the same mechanism
	// that already skips DynamicPV in ossDriver.SkipUnsupportedTest.
	conformanceSuites := []func() storageframework.TestSuite{
		storagetestsuites.InitVolumesTestSuite,
		storagetestsuites.InitVolumeIOTestSuite,
		storagetestsuites.InitVolumeModeTestSuite,
		storagetestsuites.InitMultiVolumeTestSuite,
		storagetestsuites.InitSubPathTestSuite,
		storagetestsuites.InitEphemeralTestSuite,
		storagetestsuites.InitReadWriteOncePodTestSuite,
		storagetestsuites.InitProvisioningTestSuite,
		storagetestsuites.InitCapacityTestSuite,
		storagetestsuites.InitVolumeLimitsTestSuite,
		storagetestsuites.InitTopologyTestSuite,
		storagetestsuites.InitFsGroupChangePolicyTestSuite,
		storagetestsuites.InitDisruptiveTestSuite,
		storagetestsuites.InitVolumeExpandTestSuite,
		storagetestsuites.InitSnapshottableTestSuite,
		storagetestsuites.InitSnapshottableStressTestSuite,
		storagetestsuites.InitVolumeGroupSnapshottableTestSuite,
		storagetestsuites.InitVolumeStressTestSuite,
		storagetestsuites.InitVolumeModifyTestSuite,
		storagetestsuites.InitVolumePerformanceTestSuite,
		storagetestsuites.InitPvcDeletionPerformanceTestSuite,
	}

	ginkgo.Context(fmt.Sprintf("[Driver: %s]", testDriver.GetDriverInfo().Name), func() {
		storageframework.DefineTestSuites(testDriver, conformanceSuites)
	})
})
