#!/usr/bin/env bash
# Copyright 2022 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# run-conformance.sh
#
# Builds and runs the GCS Fuse CSI driver conformance tests on an OSS kubeadm
# cluster.  Covers PreprovisionedPV, CSI ephemeral, and multi-volume patterns.
#
# Requires:
#   - Go 1.21+
#   - KUBECONFIG pointing at the OSS cluster
#   - GCP credentials (ADC) with GCS bucket create/delete on the target project
#   - setup-webhook.sh already run on the cluster (annotator + sidecar-injector)
#   - One-time IAM setup: the cluster node service account needs
#     roles/storage.admin at the project level so the test binary (which runs
#     on the cluster master and inherits the node's ADC) can create and
#     delete per-test buckets:
#       gcloud projects add-iam-policy-binding "${GCP_PROJECT}" \
#         --member="serviceAccount:<node-sa>@developer.gserviceaccount.com" \
#         --role=roles/storage.admin
#     Per-test IAM bindings from each namespace's gcsfuse-csi-sa KSA onto its
#     own bucket (via Workload Identity Federation) are handled automatically
#     by the test driver — no per-test setup required.
#
# Required env vars:
#   GCP_PROJECT   — GCP project ID (bucket creation)
#   GCP_ZONE      — GCE zone of the cluster (e.g. us-central1-a)
#
# Optional env vars:
#   KUBECONFIG            (default: ~/.kube/config)
#   BUCKET_LOCATION       (default: us-central1)
#   WIF_POOL_ID           (default: wi-pool-k8s-cluster)
#   GINKGO_FOCUS          (default: "" — runs all conformance suites)
#   GINKGO_SKIP           (default: "Performance|Disruptive")
#   GINKGO_PROCS          (default: 4) — number of parallel Ginkgo processes.
#                         Each process gets its own namespace, bucket, and WIF
#                         binding, so specs run genuinely in parallel rather
#                         than just interleaved within one process. Requires
#                         the `ginkgo` CLI (matching the version pinned in
#                         test/go.mod) on PATH; install with:
#                           go install github.com/onsi/ginkgo/v2/ginkgo@<version>
#   GINKGO_TIMEOUT        (default: 2h)
#   REPORT_DIR            (default: /tmp/gcsfuse-conformance/report)
#   TEST_WITH_NATIVE_SIDECAR  (default: false)

set -o errexit
set -o nounset
set -o pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
readonly TEST_DIR="${REPO_ROOT}/test"

# ── Required ──────────────────────────────────────────────────────────────────
: "${GCP_PROJECT:?GCP_PROJECT must be set to your GCP project ID}"
: "${GCP_ZONE:?GCP_ZONE must be set to the cluster zone, e.g. us-central1-a}"

# ── Optional with defaults ────────────────────────────────────────────────────
readonly KUBECONFIG="${KUBECONFIG:-${HOME}/.kube/config}"
readonly GINKGO_FOCUS="${GINKGO_FOCUS:-}"
# Dynamic PV tests are skipped: GCS Fuse has no CSI provisioner so PVCs never bind.
# "custom sidecar container image" tests check Containers[0].Name == "gke-gcsfuse-sidecar",
# but on OSS the annotator webhook (reinvocationPolicy: IfNeeded) intentionally moves the
# sidecar to Containers[last] so that upstream exec tests hit the user container.
# "in init container" tests require native sidecar (k8s 1.29+); on 1.28 the sidecar is a
# regular container that can only start after all init containers finish — deadlock.
# "existing paths" / "files as paths" require a type assertion to *GCSFuseCSITestDriver.
# "multiple GCS buckets via the same volume" requires a driver that provides multi-bucket
# volumes; our ossDriver provisions a single per-test bucket.
# "subPath" tests: the kubelet accesses the FUSE mount directory to create the subPath
# bind mount BEFORE the sidecar starts (sidecar is a regular container in non-native mode).
# The FUSE daemon is not yet running → kubelet goroutine blocks forever.  All subPath
# patterns are incompatible with non-native sidecar mode.
# "custom buffer volume" tests: create a PVC with StorageClass "standard-rwo" (GCE PD) for
# the gcsfuse buffer volume.  The standard-rwo StorageClass does not exist on OSS clusters.
#
# Unlocked now that each test gets its own WIF-bound bucket:
# "should store data in implicit directory" now works because PrepareTest seeds the bucket
# with an implicit-dir placeholder object before the test runs.
# "same bucket from different Pods" now works because all pods in a test share the same
# per-test bucket rather than being isolated into separate only-dir subdirectories.
readonly GINKGO_SKIP="${GINKGO_SKIP:-Performance|Disruptive|Dynamic PV|custom sidecar container image|in init container|should support existing paths|should support files as paths|multiple GCS buckets via the same volume|subPath|custom buffer volume}"
readonly GINKGO_PROCS="${GINKGO_PROCS:-4}"
readonly GINKGO_TIMEOUT="${GINKGO_TIMEOUT:-4h}"
readonly REPORT_DIR="${REPORT_DIR:-/tmp/gcsfuse-conformance/report}"
export TEST_WITH_NATIVE_SIDECAR="${TEST_WITH_NATIVE_SIDECAR:-false}"
export WIF_POOL_ID="${WIF_POOL_ID:-wi-pool-k8s-cluster}"

mkdir -p "${REPORT_DIR}"

# ── Step 1: locate a matching ginkgo CLI ───────────────────────────────────────
# Real multi-process parallelism (GINKGO_PROCS > 1) requires the `ginkgo` CLI:
# a plain `go test -c` binary only supports a single process. A CLI version
# that doesn't match test/go.mod's github.com/onsi/ginkgo/v2 causes flag
# parsing errors, so check the version even if `ginkgo` is already on PATH.
readonly GINKGO_VERSION="$(cd "${TEST_DIR}" && go list -m -f '{{.Version}}' github.com/onsi/ginkgo/v2)"
readonly GOBIN="${GOBIN:-$(go env GOPATH)/bin}"
current_ginkgo_version=""
if command -v ginkgo >/dev/null 2>&1; then
  current_ginkgo_version="v$(ginkgo version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
fi
if [[ "${current_ginkgo_version}" != "${GINKGO_VERSION}" ]]; then
  echo "ginkgo CLI (${current_ginkgo_version:-none}) does not match test/go.mod (${GINKGO_VERSION}); installing..."
  go install "github.com/onsi/ginkgo/v2/ginkgo@${GINKGO_VERSION}"
  export PATH="${GOBIN}:${PATH}"
fi

# ── Step 2: run ───────────────────────────────────────────────────────────────
echo ""
echo "Running GCS Fuse CSI driver conformance tests"
echo "  Project        : ${GCP_PROJECT}"
echo "  Zone           : ${GCP_ZONE}"
echo "  WIF pool       : ${WIF_POOL_ID}"
echo "  Focus          : ${GINKGO_FOCUS:-<all>}"
echo "  Skip           : ${GINKGO_SKIP}"
echo "  Procs          : ${GINKGO_PROCS}"
echo "  Native sidecar : ${TEST_WITH_NATIVE_SIDECAR}"
echo ""

focus_flag=""
if [[ -n "${GINKGO_FOCUS}" ]]; then
  focus_flag="--focus=${GINKGO_FOCUS}"
fi

(
  cd "${TEST_DIR}"
  ginkgo \
    -v \
    --timeout="${GINKGO_TIMEOUT}" \
    --skip="${GINKGO_SKIP}" \
    --procs="${GINKGO_PROCS}" \
    --junit-report=junit_conformance.xml \
    --json-report=report_conformance.json \
    --output-dir="${REPORT_DIR}" \
    ${focus_flag} \
    ./k8s-integration/conformance/ \
    -- \
    --project="${GCP_PROJECT}" \
    --zone="${GCP_ZONE}" \
    --kubeconfig="${KUBECONFIG}"
)
