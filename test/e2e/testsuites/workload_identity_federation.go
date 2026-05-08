/*
Copyright 2018 The Kubernetes Authors.
Copyright 2025 Google LLC

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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"local/test/e2e/specs"
	"local/test/e2e/utils"

	"github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/webhook"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	iam "google.golang.org/api/iam/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

const (
	wifWorkloadIdentityPoolID     = "gcs-fuse-oidc-pool-1"
	wifWorkloadIdentityProviderID = "gcs-fuse-oidc-provider-1"
)

type gcsFuseCSIWorkloadIdentityFederationTestSuite struct {
	tsInfo storageframework.TestSuiteInfo
}

// InitGcsFuseCSIWorkloadIdentityFederationTestSuite returns a suite with WIF-focused tests.
func InitGcsFuseCSIWorkloadIdentityFederationTestSuite() storageframework.TestSuite {
	return &gcsFuseCSIWorkloadIdentityFederationTestSuite{
		tsInfo: storageframework.TestSuiteInfo{
			Name: "workload-identity-federation",
			TestPatterns: []storageframework.TestPattern{
				storageframework.DefaultFsCSIEphemeralVolume,
			},
		},
	}
}

func (t *gcsFuseCSIWorkloadIdentityFederationTestSuite) GetTestSuiteInfo() storageframework.TestSuiteInfo {
	return t.tsInfo
}

func (t *gcsFuseCSIWorkloadIdentityFederationTestSuite) SkipUnsupportedTests(_ storageframework.TestDriver, _ storageframework.TestPattern) {
}

func (t *gcsFuseCSIWorkloadIdentityFederationTestSuite) DefineTests(driver storageframework.TestDriver, pattern storageframework.TestPattern) {
	type local struct {
		config         *storageframework.PerTestConfig
		volumeResource *storageframework.VolumeResource
	}
	var l local
	ctx := context.Background()
	f := framework.NewFrameworkWithCustomTimeouts("workload-identity-federation", storageframework.GetDriverTimeouts(driver))
	f.NamespacePodSecurityEnforceLevel = admissionapi.LevelPrivileged

	init := func(configPrefix ...string) {
		l = local{}
		l.config = driver.PrepareTest(ctx, f)
		if len(configPrefix) > 0 {
			l.config.Prefix = configPrefix[0]
		}
		l.volumeResource = storageframework.CreateVolumeResource(ctx, driver, l.config, pattern, e2evolume.SizeRange{})
	}

	cleanup := func() {
		var cleanUpErrs []error
		cleanUpErrs = append(cleanUpErrs, l.volumeResource.CleanupResource(ctx))
		err := utilerrors.NewAggregate(cleanUpErrs)
		framework.ExpectNoError(err, "while cleaning up")
	}

	// setupOSSWIFPrincipal creates all OSS Workload Identity Federation infrastructure
	// (WIF pool, provider, KSA, credential ConfigMap) for ksaName and returns the
	// WIF principal string. Cleanup is registered via ginkgo.DeferCleanup.
	setupOSSWIFPrincipal := func(ksaName, poolID, providerID, configMapName string) string {
		projectID := os.Getenv(utils.ProjectEnvVar)
		gomega.Expect(projectID).NotTo(gomega.BeEmpty(), fmt.Sprintf("%s environment variable must be set", utils.ProjectEnvVar))

		ginkgo.By("Getting GCP project number")
		projectNumber := getProjectNumber(projectID)
		gomega.Expect(projectNumber).NotTo(gomega.BeEmpty(), "failed to get project number")

		ginkgo.By(fmt.Sprintf("Creating workload identity pool: %s", poolID))
		createWorkloadIdentityPool(projectID, poolID)

		ginkgo.By("Discovering cluster OIDC issuer from cluster service account token")
		clusterIssuer := getOSSClusterOIDCIssuer(ctx, f)
		gomega.Expect(clusterIssuer).NotTo(gomega.BeEmpty(), "failed to discover cluster OIDC issuer")

		ginkgo.By(fmt.Sprintf("Creating workload identity provider: %s", providerID))
		createWorkloadIdentityProvider(projectID, poolID, providerID, clusterIssuer)

		ginkgo.By("Generating credential configuration")
		credentialConfig := generateCredentialConfig(projectNumber, poolID, providerID)

		ginkgo.By(fmt.Sprintf("Creating Kubernetes service account: %s", ksaName))
		createServiceAccount(ctx, f, ksaName)
		ginkgo.DeferCleanup(func() { deleteServiceAccount(ctx, f, ksaName) })

		ginkgo.By(fmt.Sprintf("Creating credential ConfigMap: %s", configMapName))
		createCredentialConfigMap(ctx, f, configMapName, credentialConfig)
		ginkgo.DeferCleanup(func() { deleteConfigMap(ctx, f, configMapName) })

		return fmt.Sprintf(
			"principal://iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s/subject/system:serviceaccount:%s:%s",
			projectNumber, poolID, f.Namespace.Name, ksaName,
		)
	}

	testCaseOIDCAuthFailure := func() {
		isOSS := os.Getenv(utils.IsOSSEnvVar) == "true"

		// Local constants to avoid depending on consts from gcsfuse_oidc_auth.go
		const (
			ksaName = "gcs-fuse-oidc-ksa"
			volName = "gcs-volume"
			mntPath = "/mnt/gcs"
			cmName  = "oidc-auth-failure-credentials"
		)

		if isOSS {
			// ── OSS path ──────────────────────────────────────────────────────
			// Set up WIF infrastructure (pool+provider already exist on OSS cluster).
			// Create KSA + ConfigMap but intentionally DO NOT grant bucket access.
			// NOTE: This test assumes node SA does NOT have access to the bucket.
			// If node SA has bucket access, mount will succeed and this test will fail.
			init(specs.SkipCSIBucketAccessCheckPrefix)
			defer cleanup()

			bucketName := l.volumeResource.VolSource.CSI.VolumeAttributes["bucketName"]
			gomega.Expect(bucketName).NotTo(gomega.BeEmpty(), "bucketName must be set in volume attributes")

			// setupOSSWIFPrincipal creates KSA + ConfigMap and returns the WIF principal.
			// We call it only for infrastructure setup — we do NOT call grantBucketAccess.
			setupOSSWIFPrincipal(ksaName, wifWorkloadIdentityPoolID, wifWorkloadIdentityProviderID, cmName)
			// intentionally DO NOT grant bucket access.
			// // The pod gets a usable WIF identity, but that identity
			// // is not authorized to access the bucket.

			ginkgo.By("Configuring test pod with WIF credentials but WITHOUT IAM bucket binding")
			tPod := specs.NewTestPodModifiedSpec(f.ClientSet, f.Namespace, true)
			tPod.SetServiceAccount(ksaName)
			tPod.SetupVolume(l.volumeResource, volName, mntPath, false)
			tPod.SetAnnotations(map[string]string{
				webhook.GCPWorkloadIdentityCredentialConfigMapAnnotation: cmName,
			})

			ginkgo.By("Deploying test pod")
			tPod.Create(ctx)
			defer tPod.Cleanup(ctx)

			podName := tPod.GetPodName()

			// On OSS: gcsfuse sidecar starts, token exchange with STS succeeds,
			// but GCS rejects the request → mount fails → volume-tester gets
			// CreateContainerError (context deadline exceeded).
			ginkgo.By("Waiting for auth failure signal (CreateContainerError or PermissionDenied)")

			foundAuthFailure := false
			var lastEvents []corev1.Event

			for i := 0; i < 60; i++ { // ~5 min timeout
				pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(
					ctx, podName, metav1.GetOptions{},
				)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				// OSS primary signal: CreateContainerError on volume-tester
				for _, cs := range pod.Status.ContainerStatuses {
					if cs.State.Waiting != nil &&
						cs.State.Waiting.Reason == "CreateContainerError" {
						ginkgo.By(fmt.Sprintf("Found CreateContainerError on %s: %s",
							cs.Name, cs.State.Waiting.Message))
						foundAuthFailure = true
						break
					}
				}

				if !foundAuthFailure {
					events, err := f.ClientSet.CoreV1().Events(f.Namespace.Name).List(
						ctx,
						metav1.ListOptions{
							FieldSelector: fmt.Sprintf("involvedObject.name=%s", podName),
						},
					)
					gomega.Expect(err).ToNot(gomega.HaveOccurred())
					lastEvents = events.Items

					for _, e := range events.Items {
						ginkgo.By(fmt.Sprintf("Event [%s]: %s", e.Reason, e.Message))
						if strings.Contains(e.Message, "PermissionDenied") {
							foundAuthFailure = true
							break
						}
					}
				}

				if foundAuthFailure {
					break
				}

				time.Sleep(5 * time.Second)
			}

			if !foundAuthFailure {
				ginkgo.By("Auth failure not found — printing debug info")
				pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, podName, metav1.GetOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				ginkgo.By(fmt.Sprintf("Final Pod Phase: %s", pod.Status.Phase))
				for _, cs := range pod.Status.ContainerStatuses {
					if cs.State.Waiting != nil {
						ginkgo.By(fmt.Sprintf("Container %s Waiting: reason=%s message=%s",
							cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message))
					}
					if cs.State.Terminated != nil {
						ginkgo.By(fmt.Sprintf("Container %s Terminated: reason=%s message=%s",
							cs.Name, cs.State.Terminated.Reason, cs.State.Terminated.Message))
					}
				}
				for _, e := range lastEvents {
					ginkgo.By(fmt.Sprintf("Last Event [%s]: %s", e.Reason, e.Message))
				}
			}

			gomega.Expect(foundAuthFailure).To(
				gomega.BeTrue(),
				"Expected auth failure (PermissionDenied or CreateContainerError) but none found — "+
					"check if node SA has bucket access or WIF provider setup failed",
			)

			// Confirm pod never reached Running
			ginkgo.By("Confirming pod did NOT reach Running state")
			podReachedRunning := false
			deadline := time.Now().Add(60 * time.Second)

			for time.Now().Before(deadline) {
				pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, podName, metav1.GetOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				if pod.Status.Phase == corev1.PodRunning {
					podReachedRunning = true
					ginkgo.By("ERROR: Pod unexpectedly reached Running phase")
					break
				}
				if pod.Status.Phase == corev1.PodFailed {
					ginkgo.By("Pod reached Failed phase as expected")
					break
				}

				time.Sleep(3 * time.Second)
			}

			gomega.Expect(podReachedRunning).To(
				gomega.BeFalse(),
				"Pod must NOT reach Running state when KSA has no IAM bucket binding",
			)

			ginkgo.By("Verifying final pod phase is not Running")
			pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, podName, metav1.GetOptions{})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
			gomega.Expect(pod.Status.Phase).NotTo(
				gomega.Equal(corev1.PodRunning),
				fmt.Sprintf("Pod phase must not be Running, got: %s", pod.Status.Phase),
			)

		} else {
			// ── GKE path ──────────────────────────────────────────────────────
			// Create a KSA with NO IAM annotation → GKE has no GCP SA to map
			// credentials to → gcsfuse gets no valid credentials → PermissionDenied.
			// NOTE: This test assumes node SA does NOT have access to the bucket.
			if pattern.VolType == storageframework.DynamicPV {
				e2eskipper.Skipf("skip for volume type %v", storageframework.DynamicPV)
			}

			init()
			defer cleanup()

			ginkgo.By("Creating an unbound KSA (no iam.gke.io/gcp-service-account annotation)")
			unboundKSAName := "unbound-ksa-" + f.Namespace.Name
			unboundKSA := &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:      unboundKSAName,
					Namespace: f.Namespace.Name,
					// Intentionally no iam.gke.io/gcp-service-account annotation
					// → GKE cannot map this KSA to any GCP SA → no GCS credentials
				},
			}
			_, err := f.ClientSet.CoreV1().ServiceAccounts(f.Namespace.Name).Create(
				ctx, unboundKSA, metav1.CreateOptions{},
			)
			gomega.Expect(err).ToNot(gomega.HaveOccurred(), "failed to create unbound KSA")
			defer func() {
				_ = f.ClientSet.CoreV1().ServiceAccounts(f.Namespace.Name).Delete(
					ctx, unboundKSAName, metav1.DeleteOptions{},
				)
			}()

			ginkgo.By("Configuring test pod to use the unbound KSA")
			tPod := specs.NewTestPod(f.ClientSet, f.Namespace)
			tPod.SetServiceAccount(unboundKSAName)
			tPod.SetupVolume(l.volumeResource, volName, mntPath, false)

			ginkgo.By("Deploying the pod")
			tPod.Create(ctx)
			defer tPod.Cleanup(ctx)

			podName := tPod.GetPodName()

			ginkgo.By("Waiting for PermissionDenied error in pod events (mount failure)")

			foundPermissionDenied := false
			var lastEvents []corev1.Event

			for i := 0; i < 60; i++ { // ~5 min timeout
				events, err := f.ClientSet.CoreV1().Events(f.Namespace.Name).List(
					ctx,
					metav1.ListOptions{
						FieldSelector: fmt.Sprintf("involvedObject.name=%s", podName),
					},
				)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				lastEvents = events.Items

				for _, e := range events.Items {
					ginkgo.By(fmt.Sprintf("Event: %s", e.Message))
					if strings.Contains(e.Message, "PermissionDenied") {
						foundPermissionDenied = true
						break
					}
				}

				if foundPermissionDenied {
					break
				}

				time.Sleep(5 * time.Second)
			}

			if !foundPermissionDenied {
				ginkgo.By("PermissionDenied not found — printing debug info")
				pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, podName, metav1.GetOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				ginkgo.By(fmt.Sprintf("Final Pod Phase: %s", pod.Status.Phase))
				for _, cs := range pod.Status.ContainerStatuses {
					if cs.State.Waiting != nil {
						ginkgo.By(fmt.Sprintf("Waiting Reason: %s, Message: %s",
							cs.State.Waiting.Reason, cs.State.Waiting.Message))
					}
					if cs.State.Terminated != nil {
						ginkgo.By(fmt.Sprintf("Terminated Reason: %s, Message: %s",
							cs.State.Terminated.Reason, cs.State.Terminated.Message))
					}
				}
				for _, e := range lastEvents {
					ginkgo.By(fmt.Sprintf("Last Event: %s", e.Message))
				}
			}

			gomega.Expect(foundPermissionDenied).To(
				gomega.BeTrue(),
				"Expected PermissionDenied in pod events but none found — "+
					"check if node SA has bucket access",
			)

			ginkgo.By("Confirming pod does NOT reach Running state")
			podReachedRunning := false
			deadline := time.Now().Add(60 * time.Second)

			for time.Now().Before(deadline) {
				pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, podName, metav1.GetOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				if pod.Status.Phase == corev1.PodRunning {
					podReachedRunning = true
					ginkgo.By("ERROR: Pod unexpectedly reached Running phase")
					break
				}
				if pod.Status.Phase == corev1.PodFailed {
					ginkgo.By("Pod reached Failed phase as expected")
					break
				}

				time.Sleep(3 * time.Second)
			}

			gomega.Expect(podReachedRunning).To(
				gomega.BeFalse(),
				"Pod must NOT reach Running state when KSA has no IAM binding",
			)

			ginkgo.By("Verifying final pod phase is not Running")
			pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, podName, metav1.GetOptions{})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
			gomega.Expect(pod.Status.Phase).NotTo(
				gomega.Equal(corev1.PodRunning),
				fmt.Sprintf("Pod phase must not be Running, got: %s", pod.Status.Phase),
			)
		}
	}

	ginkgo.It("should fail authentication when KSA is not bound to IAM service account", func() {
		testCaseOIDCAuthFailure()
	})
}

// ── Package-level helper functions ───────────────────────────────────────────
// These are shared with gcsfuse_oidc_auth.go via the same package.
// Do NOT redeclare getProjectNumber, createWorkloadIdentityPool,
// createWorkloadIdentityProvider, generateCredentialConfig,
// createServiceAccount, deleteServiceAccount, createCredentialConfigMap,
// deleteConfigMap, grantBucketAccess, revokeBucketAccess here —
// they are already declared in gcsfuse_oidc_auth.go.

// addWorkloadIdentityBinding grants roles/iam.workloadIdentityUser on the given GCP
// service account to the Workload Identity principal for ksaName, enabling GKE
// token exchange. Retries with backoff to handle IAM eventual consistency.
func addWorkloadIdentityBinding(ctx context.Context, gcpSAEmail, projectID, namespace, ksaName string) {
	iamService, err := iam.NewService(ctx)
	framework.ExpectNoError(err, "creating IAM service")
	saResourceName := fmt.Sprintf("projects/%s/serviceAccounts/%s", projectID, gcpSAEmail)
	member := fmt.Sprintf("serviceAccount:%s.svc.id.goog[%s/%s]", projectID, namespace, ksaName)

	err = wait.PollUntilContextTimeout(ctx, 10*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		policy, e := iamService.Projects.ServiceAccounts.GetIamPolicy(saResourceName).Do()
		if e != nil {
			klog.Warningf("GetIamPolicy for %s not ready yet: %v — retrying", gcpSAEmail, e)
			return false, nil
		}
		alreadyBound := false
		for _, b := range policy.Bindings {
			if b.Role == "roles/iam.workloadIdentityUser" {
				for _, m := range b.Members {
					if m == member {
						alreadyBound = true
						break
					}
				}
			}
			if alreadyBound {
				break
			}
		}
		if !alreadyBound {
			policy.Bindings = append(policy.Bindings, &iam.Binding{
				Role:    "roles/iam.workloadIdentityUser",
				Members: []string{member},
			})
		}
		if _, e = iamService.Projects.ServiceAccounts.SetIamPolicy(saResourceName, &iam.SetIamPolicyRequest{Policy: policy}).Do(); e != nil {
			klog.Warningf("SetIamPolicy for %s failed: %v — retrying", gcpSAEmail, e)
			return false, nil
		}
		return true, nil
	})
	framework.ExpectNoError(err, "setting workload identity binding for %s", gcpSAEmail)
}

// getOSSClusterOIDCIssuer discovers the cluster OIDC issuer URL by decoding a live
// ServiceAccount token issued by the cluster. Works on any Kubernetes cluster
// (GKE or self-managed) without requiring cluster-name or location env vars.
func getOSSClusterOIDCIssuer(ctx context.Context, f *framework.Framework) string {
	expirationSecs := int64(600)
	tok, err := f.ClientSet.CoreV1().ServiceAccounts(f.Namespace.Name).CreateToken(
		ctx,
		"default",
		&authv1.TokenRequest{
			Spec: authv1.TokenRequestSpec{ExpirationSeconds: &expirationSecs},
		},
		metav1.CreateOptions{},
	)
	framework.ExpectNoError(err, "creating service account token to discover cluster OIDC issuer")

	parts := strings.Split(tok.Status.Token, ".")
	if len(parts) != 3 {
		framework.Failf("unexpected JWT format: want 3 parts, got %d", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	framework.ExpectNoError(err, "base64-decoding JWT payload")

	var claims struct {
		Issuer string `json:"iss"`
	}
	framework.ExpectNoError(json.Unmarshal(payload, &claims), "unmarshalling JWT claims")
	gomega.Expect(claims.Issuer).NotTo(gomega.BeEmpty(), "cluster OIDC issuer must not be empty")
	return claims.Issuer
}
