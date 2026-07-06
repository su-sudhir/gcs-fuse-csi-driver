#!/usr/bin/env bash
# Copyright 2018 The Kubernetes Authors.
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
# run.sh — upstream Kubernetes external-storage conformance suite for
# the GCS Fuse CSI driver. Mirrors the approach used by
# gcp-compute-persistent-disk-csi-driver/test/k8s-integration.
#
# How it works:
#   1. Auto-detects the Kubernetes version from the live cluster.
#   2. Downloads the prebuilt e2e.test binary from dl.k8s.io.
#   3. Renders testdriver-template.yaml into a testdriver.yaml with the
#      caller-supplied BUCKET_NAME.
#   4. Runs the upstream "External Storage" conformance suite via ginkgo,
#      pointing it at the rendered testdriver.yaml.
#
# Prerequisites:
#   - Run setup-webhook.sh first. It deploys:
#       a) A pod-annotator webhook that adds gke-gcsfuse/volumes: "true"
#          to any pod containing a GCS Fuse CSI volume.
#       b) The GCS Fuse sidecar-injector webhook that then injects the
#          gke-gcsfuse-sidecar container into those pods.
#     Without both webhooks, every test pod fails NodePublishVolume with
#     "failed to find the sidecar container in Pod spec".
#   - BUCKET_NAME: a pre-existing GCS bucket accessible to the cluster.
#   - gcloud auth application-default login (ADC for GCS bucket ops).
#
# NOTE: This requires an OSS (non-GKE) cluster. On GKE private clusters
# the API server routes all pod admission through the GKE warden sidecar
# at localhost:5443 and user-deployed webhooks are never called.
#
# Required env vars:
#   BUCKET_NAME   — pre-existing GCS bucket
#
# Optional env vars:
#   KUBECONFIG              (default: ~/.kube/config)
#   K8S_TEST_VERSION        (default: auto-detected from cluster)
#   GINKGO_FOCUS            (default: "External Storage")
#   GINKGO_SKIP             (default: Disruptive|Performance)
#   GINKGO_PROCS            (default: 4)
#   GINKGO_TIMEOUT          (default: 2h)
#   SKIP_BUCKET_ACCESS_CHECK (default: true)
#   WORK_DIR                (default: /tmp/gcsfuse-k8s-integration)
#   REPORT_DIR              (default: ${WORK_DIR}/report)
#   PLATFORM                (default: linux)
#   ARCH                    (default: amd64)

set -o errexit
set -o nounset
set -o pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Required ──────────────────────────────────────────────────────────────────
: "${BUCKET_NAME:?BUCKET_NAME must be set to a pre-existing GCS bucket}"

# ── Optional with defaults ────────────────────────────────────────────────────
readonly KUBECONFIG="${KUBECONFIG:-${HOME}/.kube/config}"
readonly GINKGO_FOCUS="${GINKGO_FOCUS:-External Storage}"
readonly GINKGO_SKIP="${GINKGO_SKIP:-Disruptive|Performance}"
readonly GINKGO_PROCS="${GINKGO_PROCS:-4}"
readonly GINKGO_TIMEOUT="${GINKGO_TIMEOUT:-2h}"
readonly SKIP_BUCKET_ACCESS_CHECK="${SKIP_BUCKET_ACCESS_CHECK:-true}"
readonly WORK_DIR="${WORK_DIR:-/tmp/gcsfuse-k8s-integration}"
readonly REPORT_DIR="${REPORT_DIR:-${WORK_DIR}/report}"
readonly PLATFORM="${PLATFORM:-linux}"
readonly ARCH="${ARCH:-amd64}"

mkdir -p "${WORK_DIR}" "${REPORT_DIR}"

# ── Step 1: resolve upstream Kubernetes version ───────────────────────────────
if [[ -z "${K8S_TEST_VERSION:-}" ]]; then
  echo "Auto-detecting Kubernetes version from cluster..."
  raw_version="$(kubectl version --output=json --kubeconfig="${KUBECONFIG}" \
    | python3 -c "import json,sys; print(json.load(sys.stdin)['serverVersion']['gitVersion'])")"
  K8S_TEST_VERSION="$(echo "${raw_version}" | sed 's/^v//; s/[-+].*//')"
  echo "Detected upstream k8s version: ${K8S_TEST_VERSION}"
fi
readonly K8S_TEST_VERSION

# ── Step 2: download the prebuilt e2e.test binary ────────────────────────────
readonly E2E_TEST_BIN="${WORK_DIR}/e2e.test"
readonly GINKGO_BIN="${WORK_DIR}/ginkgo"

if [[ ! -x "${E2E_TEST_BIN}" ]]; then
  tarball="kubernetes-test-${PLATFORM}-${ARCH}.tar.gz"
  download_url="https://dl.k8s.io/release/v${K8S_TEST_VERSION}/${tarball}"
  tarball_path="${WORK_DIR}/${tarball}"

  echo "Downloading ${download_url}..."
  curl -sSfL --output "${tarball_path}" "${download_url}"

  echo "Extracting e2e.test..."
  tar -xzf "${tarball_path}" -C "${WORK_DIR}" \
    --strip-components=3 \
    "kubernetes/test/bin/e2e.test" \
    "kubernetes/test/bin/ginkgo" 2>/dev/null || \
  tar -xzf "${tarball_path}" -C "${WORK_DIR}" \
    --strip-components=3 \
    "kubernetes/test/bin/e2e.test"

  rm -f "${tarball_path}"
  chmod +x "${E2E_TEST_BIN}"
  [[ -f "${GINKGO_BIN}" ]] && chmod +x "${GINKGO_BIN}"
  echo "e2e.test ready at ${E2E_TEST_BIN}"
else
  echo "Using cached e2e.test at ${E2E_TEST_BIN}"
fi

# Fall back to installed ginkgo if the tarball did not include it.
if [[ ! -x "${GINKGO_BIN}" ]]; then
  export PATH="${PATH}:$(go env GOPATH)/bin"
  if ! command -v ginkgo &>/dev/null; then
    echo "Installing ginkgo..."
    go install github.com/onsi/ginkgo/v2/ginkgo@v2.27.0
  fi
  readonly GINKGO_BIN="$(command -v ginkgo)"
fi

# ── Step 3: render the driver-definition YAML ────────────────────────────────
readonly TESTDRIVER_FILE="${WORK_DIR}/testdriver.yaml"
export BUCKET_NAME SKIP_BUCKET_ACCESS_CHECK
envsubst < "${SCRIPT_DIR}/testdriver-template.yaml" > "${TESTDRIVER_FILE}"
echo "Driver-definition written to ${TESTDRIVER_FILE}"

# ── Step 4: run the upstream external-storage suite ──────────────────────────
echo ""
echo "Running External Storage conformance suite against GCS Fuse CSI driver"
echo "  Kubernetes version : v${K8S_TEST_VERSION}"
echo "  Bucket             : ${BUCKET_NAME}"
echo "  Focus              : ${GINKGO_FOCUS}"
echo "  Skip               : ${GINKGO_SKIP}"
echo "  Procs              : ${GINKGO_PROCS}"
echo "  Timeout            : ${GINKGO_TIMEOUT}"
echo ""

"${GINKGO_BIN}" \
  --focus="${GINKGO_FOCUS}" \
  --skip="${GINKGO_SKIP}" \
  --procs="${GINKGO_PROCS}" \
  --timeout="${GINKGO_TIMEOUT}" \
  --v \
  --output-dir="${REPORT_DIR}" \
  --junit-report="junit_gcsfuse_k8s_integration.xml" \
  --json-report="report_gcsfuse_k8s_integration.json" \
  "${E2E_TEST_BIN}" \
  -- \
  --storage.testdriver="${TESTDRIVER_FILE}" \
  --kubeconfig="${KUBECONFIG}"
