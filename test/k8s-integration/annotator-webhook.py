#!/usr/bin/env python3
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
# annotator-webhook.py
#
# A minimal MutatingAdmissionWebhook that automatically adds:
#   gke-gcsfuse/volumes: "true"
# to any pod that contains a GCS Fuse CSI volume (inline or PVC-backed).
#
# Why this exists:
#   The GCS Fuse sidecar-injector webhook only injects the gke-gcsfuse-sidecar
#   container when this annotation is present on the pod. The upstream Kubernetes
#   external-storage conformance suite (e2e.test) creates generic test pods
#   without this annotation. Without sidecar injection, NodePublishVolume fails
#   with "failed to find the sidecar container in Pod spec".
#
#   This annotator webhook fires first (its MutatingWebhookConfiguration name
#   sorts alphabetically before the GCS Fuse injector), detects the GCS Fuse
#   CSI volume, and patches the annotation onto the pod. The GCS Fuse injector
#   then fires and injects the sidecar as normal.
#
# Volume detection:
#   - Inline CSI volumes: detected directly from pod spec (spec.volumes[].csi.driver)
#   - PVC-backed volumes: detected via the PVC's StorageClass provisioner (works
#     immediately, before binding) with a fallback to resolving PVC → bound PV →
#     spec.csi.driver for statically pre-bound PVs with no StorageClass.
#   - GenericEphemeralVolume (spec.volumes[].ephemeral.volumeClaimTemplate):
#     detected via its embedded storageClassName, no PVC/PV lookup needed.
#   Requires RBAC permission to get persistentvolumeclaims, persistentvolumes,
#   and storageclasses.storage.k8s.io (see setup-webhook.sh).
#
# Deployment:
#   This script is stored in a ConfigMap and run inside a Deployment by
#   setup-webhook.sh. It serves HTTPS on port 8443 using the TLS cert mounted
#   from a Secret.

import base64
import json
import logging
import ssl
import ssl as ssl_module
import sys
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(message)s",
    stream=sys.stdout,
)
log = logging.getLogger(__name__)

GCSFUSE_DRIVER  = "gcsfuse.csi.storage.gke.io"
GCSFUSE_SIDECAR = "gke-gcsfuse-sidecar"
ANNOTATION_KEY  = "gke-gcsfuse/volumes"
CERT_FILE = "/certs/cert.pem"
KEY_FILE  = "/certs/key.pem"
PORT = 8443

# In-cluster k8s API access via the pod's mounted service account token.
SA_TOKEN_FILE = "/var/run/secrets/kubernetes.io/serviceaccount/token"
SA_CA_FILE    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
K8S_API       = "https://kubernetes.default.svc"


def _k8s_get(path: str) -> dict:
    """GET a k8s API resource using the pod's service account token.

    Returns an empty dict on any error so callers can safely use .get().
    """
    try:
        with open(SA_TOKEN_FILE) as f:
            token = f.read().strip()
        ctx = ssl_module.create_default_context(cafile=SA_CA_FILE)
        req = urllib.request.Request(
            f"{K8S_API}{path}",
            headers={"Authorization": f"Bearer {token}"},
        )
        with urllib.request.urlopen(req, context=ctx, timeout=2) as resp:
            return json.loads(resp.read())
    except Exception as exc:
        log.warning("k8s API call failed for %s: %s", path, exc)
        return {}


def _storageclass_uses_gcsfuse(sc_name: str) -> bool:
    """Return True if the named StorageClass provisions via the GCS Fuse driver."""
    if not sc_name:
        return False
    sc = _k8s_get(f"/apis/storage.k8s.io/v1/storageclasses/{sc_name}")
    return sc.get("provisioner") == GCSFUSE_DRIVER


def _pvc_uses_gcsfuse(pvc_name: str, namespace: str) -> bool:
    """Return True if the named PVC is backed by GCS Fuse.

    Checks the PVC's StorageClass first: dynamically-provisioned PVCs using
    VolumeBindingWaitForFirstConsumer aren't bound to a PV yet at
    pod-admission time (binding only happens once the pod is scheduled), so
    resolving through the bound PV would always miss them. The StorageClass
    is known immediately regardless of binding state.
    """
    pvc = _k8s_get(f"/api/v1/namespaces/{namespace}/persistentvolumeclaims/{pvc_name}")
    if _storageclass_uses_gcsfuse(pvc.get("spec", {}).get("storageClassName", "")):
        return True
    pv_name = pvc.get("spec", {}).get("volumeName", "")
    if not pv_name:
        # Not dynamically provisioned by GCS Fuse, and not yet bound to any PV —
        # cannot confirm; skip annotation to avoid misfire.
        log.debug("PVC %s/%s not yet bound, skipping annotation", namespace, pvc_name)
        return False
    pv = _k8s_get(f"/api/v1/persistentvolumes/{pv_name}")
    return pv.get("spec", {}).get("csi", {}).get("driver") == GCSFUSE_DRIVER


def pod_has_gcsfuse_volume(spec: dict, namespace: str) -> bool:
    """Return True if the pod contains any GCS Fuse volume (inline, PVC-backed, or ephemeral)."""
    for vol in spec.get("volumes", []):
        # Inline CSI ephemeral volume — driver name is directly in the pod spec.
        if vol.get("csi", {}).get("driver") == GCSFUSE_DRIVER:
            return True
        # PVC-backed volume — must resolve through the API to find the CSI driver
        # or StorageClass provisioner.
        pvc_name = vol.get("persistentVolumeClaim", {}).get("claimName")
        if pvc_name and _pvc_uses_gcsfuse(pvc_name, namespace):
            return True
        # GenericEphemeralVolume — the StorageClass name is embedded directly in
        # the pod spec's volume template, so no PVC/PV lookup is needed at all.
        eph_sc = (
            vol.get("ephemeral", {})
            .get("volumeClaimTemplate", {})
            .get("spec", {})
            .get("storageClassName", "")
        )
        if _storageclass_uses_gcsfuse(eph_sc):
            return True
    return False


def sidecar_is_first_container(spec: dict) -> bool:
    """Return True when the sidecar injector has prepended gke-gcsfuse-sidecar."""
    containers = spec.get("containers", [])
    return len(containers) > 1 and containers[0].get("name") == GCSFUSE_SIDECAR


def make_response(uid: str, patch: list | None) -> bytes:
    resp: dict = {
        "apiVersion": "admission.k8s.io/v1",
        "kind": "AdmissionReview",
        "response": {"uid": uid, "allowed": True},
    }
    if patch:
        resp["response"]["patchType"] = "JSONPatch"
        resp["response"]["patch"] = base64.b64encode(
            json.dumps(patch).encode()
        ).decode()
    return json.dumps(resp).encode()


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        try:
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            req  = body.get("request", {})
            uid  = req.get("uid", "")
            pod  = req.get("object", {})
            meta = pod.get("metadata", {})
            spec = pod.get("spec", {})
            namespace           = meta.get("namespace", "")
            existing_annotations = meta.get("annotations") or {}
            pod_id = meta.get("generateName", meta.get("name", ""))

            patch = None
            if pod_has_gcsfuse_volume(spec, namespace) and ANNOTATION_KEY not in existing_annotations:
                # First pass: add the annotation so the sidecar-injector fires.
                log.info("Patching annotation onto pod %s/%s", namespace, pod_id)
                if not existing_annotations:
                    patch = [
                        {"op": "add", "path": "/metadata/annotations", "value": {}},
                        {"op": "add", "path": "/metadata/annotations/gke-gcsfuse~1volumes", "value": "true"},
                    ]
                else:
                    patch = [
                        {"op": "add", "path": "/metadata/annotations/gke-gcsfuse~1volumes", "value": "true"},
                    ]
            elif ANNOTATION_KEY in existing_annotations and sidecar_is_first_container(spec):
                # Reinvocation (reinvocationPolicy: IfNeeded): the sidecar-injector
                # has prepended gke-gcsfuse-sidecar as containers[0].  Move it to
                # the end so the upstream e2e.test's Containers[0] exec hits the
                # user's test container, not the distroless sidecar.
                log.info("Reordering sidecar to last position in pod %s/%s", namespace, pod_id)
                patch = [{"op": "move", "from": "/spec/containers/0",
                          "path": "/spec/containers/-"}]

            body_out = make_response(uid, patch)
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body_out)))
            self.end_headers()
            self.wfile.write(body_out)

        except Exception:
            log.exception("Error processing AdmissionReview")
            self.send_response(500)
            self.end_headers()

    def log_message(self, *_):  # silence per-request access log
        pass


def main() -> None:
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(CERT_FILE, KEY_FILE)
    server = HTTPServer(("", PORT), Handler)
    server.socket = ctx.wrap_socket(server.socket, server_side=True)
    log.info("GCS Fuse annotator webhook listening on :%d", PORT)
    server.serve_forever()


if __name__ == "__main__":
    main()
