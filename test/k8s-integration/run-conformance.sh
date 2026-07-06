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
#   - GCP credentials (ADC) with GCS bucket create/delete and object admin on
#     the target project
#   - setup-webhook.sh already run on the cluster (annotator + sidecar-injector)
#
# Required env vars:
#   GCP_PROJECT   — GCP project ID (bucket creation)
#   GCP_ZONE      — GCE zone of the cluster (e.g. us-central1-a)
#
# Optional env vars:
#   KUBECONFIG            (default: ~/.kube/config)
#   BUCKET_LOCATION       (default: us-central1)
#   GINKGO_FOCUS          (default: "" — runs all conformance suites)
#   GINKGO_SKIP           (default: "Performance|Disruptive")
#   GINKGO_PROCS          (default: 4)
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
: "${BUCKET_NAME:?BUCKET_NAME must be set to a pre-existing GCS bucket for test volumes}"
export BUCKET_NAME

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
# volumes; our ossDriver uses a single bucket with per-test subdir isolation.
# "subPath" tests: the kubelet accesses the FUSE mount directory to create the subPath
# bind mount BEFORE the sidecar starts (sidecar is a regular container in non-native mode).
# The FUSE daemon is not yet running → kubelet goroutine blocks forever.  All subPath
# patterns are incompatible with non-native sidecar mode.
# "same bucket from different Pods" tests: pod1 writes data, pod2 reads it.  Our ossDriver
# uses per-volume subdir isolation (only-dir=<uuid>), so different volume resources point
# to different GCS paths and data written by pod1 is invisible to pod2.
# "custom buffer volume" tests: create a PVC with StorageClass "standard-rwo" (GCE PD) for
# the gcsfuse buffer volume.  The standard-rwo StorageClass does not exist on OSS clusters.
# "should store data in implicit directory" tests: write to /mnt/test/implicit-dir/data where
# implicit-dir has no GCS objects yet.  gcsfuse --implicit-dirs only shows directories that
# exist implicitly (via existing object prefixes); a fresh empty subdir has no such prefix so
# the kernel LOOKUP for implicit-dir returns ENOENT.  GKE's driver calls createGCSTestFiles
# to seed the bucket first; our ossDriver does not.
readonly GINKGO_SKIP="${GINKGO_SKIP:-Performance|Disruptive|Dynamic PV|custom sidecar container image|in init container|should support existing paths|should support files as paths|multiple GCS buckets via the same volume|subPath|same bucket from different Pods|custom buffer volume|should store data in implicit directory}"
readonly GINKGO_TIMEOUT="${GINKGO_TIMEOUT:-4h}"
readonly REPORT_DIR="${REPORT_DIR:-/tmp/gcsfuse-conformance/report}"
export TEST_WITH_NATIVE_SIDECAR="${TEST_WITH_NATIVE_SIDECAR:-false}"

mkdir -p "${REPORT_DIR}"

# ── Step 1: build the conformance test binary ─────────────────────────────────
readonly BINARY="${REPORT_DIR}/gcsfuse-conformance.test"

echo "Building conformance test binary..."
(
  cd "${TEST_DIR}"
  go test -c \
    -o "${BINARY}" \
    ./k8s-integration/conformance/
)
echo "Built: ${BINARY}"

# ── Step 2: run ───────────────────────────────────────────────────────────────
echo ""
echo "Running GCS Fuse CSI driver conformance tests"
echo "  Project        : ${GCP_PROJECT}"
echo "  Zone           : ${GCP_ZONE}"
echo "  Bucket         : ${BUCKET_NAME}"
echo "  Focus          : ${GINKGO_FOCUS:-<all>}"
echo "  Skip           : ${GINKGO_SKIP}"
echo "  Native sidecar : ${TEST_WITH_NATIVE_SIDECAR}"
echo ""

focus_flag=""
if [[ -n "${GINKGO_FOCUS}" ]]; then
  focus_flag="--ginkgo.focus=${GINKGO_FOCUS}"
fi

# --ginkgo.timeout controls Ginkgo's own suite timeout (default: 1h) and IS accepted
# by compiled test binaries.  -test.timeout is the outer Go test process timeout and
# should be set slightly higher so Ginkgo gets a chance to print its summary on expiry.
"${BINARY}" \
  -test.v \
  -test.timeout="$((${GINKGO_TIMEOUT%h} + 1))h" \
  ${focus_flag} \
  --ginkgo.skip="${GINKGO_SKIP}" \
  --ginkgo.timeout="${GINKGO_TIMEOUT}" \
  --ginkgo.v \
  --ginkgo.junit-report="${REPORT_DIR}/junit_conformance.xml" \
  --ginkgo.json-report="${REPORT_DIR}/report_conformance.json" \
  --project="${GCP_PROJECT}" \
  --zone="${GCP_ZONE}" \
  --kubeconfig="${KUBECONFIG}"
