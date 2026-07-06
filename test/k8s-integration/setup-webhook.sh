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
# setup-webhook.sh
#
# Deploys the two admission webhooks needed for the upstream Kubernetes
# external-storage conformance suite to work against the GCS Fuse CSI driver
# on an OSS (non-GKE) cluster:
#
#   1. gcsfuse-annotator webhook  (aaa-gcsfuse-annotator.csi.storage.gke.io)
#      A small Python HTTPS server that fires on pod CREATE, detects any
#      GCS Fuse CSI volume in the pod spec, and patches the annotation
#        gke-gcsfuse/volumes: "true"
#      onto the pod.  Named so it sorts alphabetically first and fires
#      before the sidecar-injector webhook.
#
#   2. GCS Fuse sidecar-injector webhook  (gcsfuse-sidecar-injector.csi.storage.gke.io)
#      The standard GCS Fuse webhook that reads the annotation added in step 1
#      and injects the gke-gcsfuse-sidecar initContainer into the pod.
#
# Without both webhooks every test pod fails NodePublishVolume with:
#   "failed to find the sidecar container in Pod spec"
#
# NOTE: Requires an OSS (non-GKE) cluster.  On GKE private clusters the API
# server routes all pod admission through the GKE warden sidecar at
# localhost:5443 and user-deployed webhooks are never called.
#
# Usage:
#   ./test/k8s-integration/setup-webhook.sh            # deploy
#   ./test/k8s-integration/setup-webhook.sh --teardown # remove

set -o errexit
set -o nounset
set -o pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

readonly WEBHOOK_NS="gcs-fuse-csi-driver"
readonly INJECTOR_SVC="gcs-fuse-csi-driver-webhook"
readonly INJECTOR_SECRET="gcs-fuse-csi-driver-webhook-secret"
readonly ANNOTATOR_SVC="gcsfuse-annotator-webhook"
readonly ANNOTATOR_SECRET="gcsfuse-annotator-webhook-secret"
readonly ANNOTATOR_SA="gcsfuse-annotator-webhook"
readonly WORK_DIR="${WORK_DIR:-/tmp/gcsfuse-k8s-integration}"
readonly CERT_DIR="${WORK_DIR}/certs"

# ── teardown ──────────────────────────────────────────────────────────────────
if [[ "${1:-}" == "--teardown" ]]; then
  echo "Tearing down GCS Fuse webhooks..."
  kubectl delete mutatingwebhookconfiguration \
    "aaa-gcsfuse-annotator.csi.storage.gke.io" \
    "gcsfuse-sidecar-injector.csi.storage.gke.io" \
    --ignore-not-found
  kubectl delete clusterrolebinding "gcsfuse-annotator-webhook" --ignore-not-found
  kubectl delete clusterrole "gcsfuse-annotator-webhook" --ignore-not-found
  kubectl delete namespace "${WEBHOOK_NS}" --ignore-not-found
  echo "Done."
  exit 0
fi

mkdir -p "${CERT_DIR}"

# ── Step 1: namespace ─────────────────────────────────────────────────────────
echo "Creating namespace ${WEBHOOK_NS}..."
kubectl create namespace "${WEBHOOK_NS}" --dry-run=client -o yaml \
  | kubectl apply -f -

# ── Step 2: shared self-signed CA ─────────────────────────────────────────────
echo "Generating shared CA..."
openssl genrsa -out "${CERT_DIR}/ca.key" 2048 2>/dev/null
openssl req -new -x509 -days 3650 \
  -key "${CERT_DIR}/ca.key" \
  -subj "/CN=gcsfuse-webhook-ca" \
  -out "${CERT_DIR}/ca.crt" 2>/dev/null

CA_BUNDLE="$(base64 -w0 < "${CERT_DIR}/ca.crt")"

# Helper: generate a server cert signed by the shared CA.
# Usage: gen_cert <name> <service> <namespace>
gen_cert() {
  local name="$1" svc="$2" ns="$3"
  local san="DNS:${svc},DNS:${svc}.${ns},DNS:${svc}.${ns}.svc,DNS:${svc}.${ns}.svc.cluster.local"
  openssl genrsa -out "${CERT_DIR}/${name}.key" 2048 2>/dev/null
  openssl req -new \
    -key "${CERT_DIR}/${name}.key" \
    -subj "/CN=${svc}.${ns}.svc" \
    -out "${CERT_DIR}/${name}.csr" \
    -addext "subjectAltName=${san}" 2>/dev/null
  openssl x509 -req -days 3650 \
    -in "${CERT_DIR}/${name}.csr" \
    -CA "${CERT_DIR}/ca.crt" -CAkey "${CERT_DIR}/ca.key" -CAcreateserial \
    -copy_extensions copy \
    -out "${CERT_DIR}/${name}.crt" 2>/dev/null
  echo "  Generated cert for ${svc}.${ns}.svc"
}

# ── Step 3: image refs from cluster ConfigMap ─────────────────────────────────
# On OSS clusters the configmap lives in WEBHOOK_NS (gcs-fuse-csi-driver), not
# kube-system, and there is no oss-webhook-image key — fall back to the running
# deployment's image in that case.
echo "Reading image refs from ${WEBHOOK_NS}/gcsfusecsi-image-config..."
SIDECAR_IMAGE="$(kubectl get configmap gcsfusecsi-image-config \
  -n "${WEBHOOK_NS}" -o jsonpath='{.data.sidecar-image}')"
METADATA_SIDECAR_IMAGE="$(kubectl get configmap gcsfusecsi-image-config \
  -n "${WEBHOOK_NS}" -o jsonpath='{.data.metadata-sidecar-image}')"
WEBHOOK_IMAGE="$(kubectl get configmap gcsfusecsi-image-config \
  -n "${WEBHOOK_NS}" -o jsonpath='{.data.oss-webhook-image}' 2>/dev/null || true)"
if [[ -z "${WEBHOOK_IMAGE}" ]]; then
  WEBHOOK_IMAGE="$(kubectl get deployment "${INJECTOR_SVC}" \
    -n "${WEBHOOK_NS}" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)"
fi
echo "  sidecar          : ${SIDECAR_IMAGE}"
echo "  metadata-sidecar : ${METADATA_SIDECAR_IMAGE}"
echo "  webhook          : ${WEBHOOK_IMAGE}"

kubectl create configmap gcsfusecsi-image-config \
  --from-literal="sidecar-image=${SIDECAR_IMAGE}" \
  --from-literal="metadata-sidecar-image=${METADATA_SIDECAR_IMAGE:-${SIDECAR_IMAGE}}" \
  -n "${WEBHOOK_NS}" --dry-run=client -o yaml | kubectl apply -f -

# ── Step 4: GCS Fuse sidecar-injector webhook ─────────────────────────────────
echo ""
echo "=== Deploying GCS Fuse sidecar-injector webhook ==="

gen_cert "injector" "${INJECTOR_SVC}" "${WEBHOOK_NS}"
kubectl create secret generic "${INJECTOR_SECRET}" \
  --from-file="cert.pem=${CERT_DIR}/injector.crt" \
  --from-file="key.pem=${CERT_DIR}/injector.key" \
  -n "${WEBHOOK_NS}" --dry-run=client -o yaml | kubectl apply -f -

# RBAC — patch namespace into ClusterRoleBinding subjects
echo "Applying RBAC..."
python3 - "${REPO_ROOT}/deploy/base/webhook/webhook_setup.yaml" "${WEBHOOK_NS}" <<'PYEOF' \
  | kubectl apply -n "${WEBHOOK_NS}" --validate=false -f -
import sys, re
path, ns = sys.argv[1], sys.argv[2]
text = open(path).read()
patched = re.sub(
    r'(subjects:\n(?:.*\n)*?  - kind: ServiceAccount\n    name: \S+)(\n)',
    lambda m: m.group(1) + f'\n    namespace: {ns}\n',
    text
)
print(patched)
PYEOF

# Deployment — strip flags unsupported by older GKE-managed binaries
echo "Deploying injector..."
sed \
  -e "s|gke.gcr.io/gcs-fuse-csi-driver-webhook|${WEBHOOK_IMAGE}|g" \
  -e '/--enable-auto-gomemlimit/d' \
  -e '/--auto-gomemlimit-ratio/d' \
  "${REPO_ROOT}/deploy/base/webhook/deployment.yaml" \
  | kubectl apply -n "${WEBHOOK_NS}" --validate=false -f -

echo "Restarting injector to pick up new TLS cert..."
kubectl rollout restart deployment/"${INJECTOR_SVC}" -n "${WEBHOOK_NS}"
kubectl rollout status deployment/"${INJECTOR_SVC}" \
  -n "${WEBHOOK_NS}" --timeout=120s

# MutatingWebhookConfiguration for the injector (ClusterIP service reference)
kubectl apply -f - <<EOF
$(sed "s|caBundle: \"\"|caBundle: \"${CA_BUNDLE}\"|g" \
    "${REPO_ROOT}/deploy/base/webhook/mutatingwebhook.yaml")
EOF

# ── Step 5: GCS Fuse annotator webhook ───────────────────────────────────────
echo ""
echo "=== Deploying GCS Fuse annotator webhook ==="

gen_cert "annotator" "${ANNOTATOR_SVC}" "${WEBHOOK_NS}"
kubectl create secret generic "${ANNOTATOR_SECRET}" \
  --from-file="cert.pem=${CERT_DIR}/annotator.crt" \
  --from-file="key.pem=${CERT_DIR}/annotator.key" \
  -n "${WEBHOOK_NS}" --dry-run=client -o yaml | kubectl apply -f -

# ServiceAccount + RBAC — the annotator needs to read PVCs and PVs to detect
# GCS Fuse volumes that are mounted via PersistentVolumeClaim (not inline CSI).
kubectl apply -n "${WEBHOOK_NS}" -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${ANNOTATOR_SA}
  namespace: ${WEBHOOK_NS}
EOF

kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ${ANNOTATOR_SA}
rules:
- apiGroups: [""]
  resources: ["persistentvolumeclaims", "persistentvolumes"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ${ANNOTATOR_SA}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ${ANNOTATOR_SA}
subjects:
- kind: ServiceAccount
  name: ${ANNOTATOR_SA}
  namespace: ${WEBHOOK_NS}
EOF

# Store the Python webhook server in a ConfigMap
kubectl create configmap gcsfuse-annotator-script \
  --from-file="webhook.py=${SCRIPT_DIR}/annotator-webhook.py" \
  -n "${WEBHOOK_NS}" --dry-run=client -o yaml | kubectl apply -f -

# Deployment + Service
kubectl apply -n "${WEBHOOK_NS}" -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${ANNOTATOR_SVC}
  labels:
    app: ${ANNOTATOR_SVC}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${ANNOTATOR_SVC}
  template:
    metadata:
      labels:
        app: ${ANNOTATOR_SVC}
    spec:
      serviceAccountName: ${ANNOTATOR_SA}
      containers:
      - name: annotator
        image: python:3.11-slim
        command: ["python3", "/scripts/webhook.py"]
        ports:
        - containerPort: 8443
          name: https
        volumeMounts:
        - name: certs
          mountPath: /certs
          readOnly: true
        - name: script
          mountPath: /scripts
          readOnly: true
        resources:
          requests:
            cpu: 10m
            memory: 32Mi
          limits:
            cpu: 100m
            memory: 64Mi
      volumes:
      - name: certs
        secret:
          secretName: ${ANNOTATOR_SECRET}
      - name: script
        configMap:
          name: gcsfuse-annotator-script
---
apiVersion: v1
kind: Service
metadata:
  name: ${ANNOTATOR_SVC}
  labels:
    app: ${ANNOTATOR_SVC}
spec:
  selector:
    app: ${ANNOTATOR_SVC}
  ports:
  - name: https
    port: 443
    targetPort: 8443
EOF

echo "Waiting for annotator rollout..."
kubectl rollout status deployment/"${ANNOTATOR_SVC}" \
  -n "${WEBHOOK_NS}" --timeout=120s

# MutatingWebhookConfiguration for annotator.
# Named "aaa-gcsfuse-annotator..." so it sorts alphabetically before
# "gcsfuse-sidecar-injector..." and fires first on every pod CREATE.
kubectl apply -f - <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: aaa-gcsfuse-annotator.csi.storage.gke.io
webhooks:
- name: aaa-gcsfuse-annotator.csi.storage.gke.io
  admissionReviewVersions: ["v1"]
  clientConfig:
    caBundle: "${CA_BUNDLE}"
    service:
      name: ${ANNOTATOR_SVC}
      namespace: ${WEBHOOK_NS}
      port: 443
      path: /
  rules:
  - apiGroups:   [""]
    apiVersions: ["v1"]
    operations:  ["CREATE"]
    resources:   ["pods"]
    scope:       "*"
  failurePolicy: Ignore
  sideEffects:   None
  timeoutSeconds: 5
  # IfNeeded causes Kubernetes to reinvoke this webhook after the sidecar
  # injector prepends gke-gcsfuse-sidecar.  The second invocation moves it to
  # the end so that upstream e2e.test's Containers[0] exec hits the user
  # container, not the distroless sidecar.
  reinvocationPolicy: IfNeeded
EOF

echo ""
echo "Setup complete. Both webhooks are ready:"
echo "  aaa-gcsfuse-annotator.csi.storage.gke.io  → adds gke-gcsfuse/volumes: \"true\" to pods with GCS Fuse volumes"
echo "  gcsfuse-sidecar-injector.csi.storage.gke.io → injects gke-gcsfuse-sidecar into annotated pods"
echo ""
echo "Run the integration tests with:"
echo "  BUCKET_NAME=<your-bucket> ./test/k8s-integration/run.sh"
