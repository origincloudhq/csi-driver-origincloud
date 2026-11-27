#!/usr/bin/env bash
# Renders deploy/ and checks it:
#
# - kubeconform validates every object, strictly, against the Kubernetes
#   schemas of each version in KUBERNETES_VERSIONS, and the VolumeSnapshotClass
#   against the snapshot CRD's schema;
# - hack/check_no_credentials.py fails on a Secret or a credential;
# - every image is pinned to a version tag.
#
# Needs kubectl (or kustomize), kubeconform and python3 with PyYAML.
set -euo pipefail

cd "$(dirname "$0")/.."

KUBERNETES_VERSIONS="${KUBERNETES_VERSIONS:-1.34.0 1.37.0}"
# The snapshot CRDs' schemas, from a fixed commit of the CRDs catalog.
CRD_SCHEMAS="https://raw.githubusercontent.com/datreeio/CRDs-catalog/b50f3fbc45169a14b6d2d3f767b32fd96ef4a3fa/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json"

rendered="$(mktemp)"
trap 'rm -f "${rendered}"' EXIT

if command -v kustomize >/dev/null; then
  kustomize build deploy/ >"${rendered}"
else
  kubectl kustomize deploy/ >"${rendered}"
fi
echo "rendered $(grep -c '^kind:' "${rendered}") objects"

for version in ${KUBERNETES_VERSIONS}; do
  echo "kubeconform, Kubernetes ${version}"
  kubeconform -strict -summary \
    -kubernetes-version "${version}" \
    -schema-location default \
    -schema-location "${CRD_SCHEMAS}" \
    "${rendered}"
done

python3 hack/check_no_credentials.py "${rendered}"

unpinned="$(grep -E '^\s+image:' "${rendered}" | grep -vE ':v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || true)"
if [[ -n "${unpinned}" ]]; then
  echo "images not pinned to a version:"; echo "${unpinned}"; exit 1
fi
echo "every image is pinned"
