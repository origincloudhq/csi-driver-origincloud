#!/usr/bin/env bash
# Checks a built image: every tool the node plugin runs is on its PATH, the
# image has a CA trust store for the API's TLS, and the driver starts with
# --mode=node and answers GetPluginInfo with the expected name and version.
#
# Usage: hack/smoke-test-image.sh <image> <expected version>
#
# Needs docker, jq and Go (to find the CSI proto in the module cache: run
# go mod download first).
set -euo pipefail

image="${1:?usage: $0 <image> <expected version>}"
want_version="${2:?usage: $0 <image> <expected version>}"
grpcurl_version="v1.9.3"
port=10000

fail() { echo "FAIL: $*" >&2; exit 1; }

echo "--- tools on PATH"
docker run --rm --entrypoint /bin/sh "${image}" -c '
  missing=0
  for tool in blkid mkfs.ext4 mkfs.xfs fsck resize2fs xfs_growfs mount umount; do
    if path="$(command -v "${tool}")"; then echo "${tool}: ${path}"; else echo "${tool}: missing"; missing=1; fi
  done
  exit "${missing}"
' || fail "a tool is missing from the image"

echo "--- CA certificates"
docker run --rm --entrypoint /bin/sh "${image}" -c 'test -s /etc/ssl/certs/ca-certificates.crt' \
  || fail "the image has no CA certificates"

echo "--- --version"
got="$(docker run --rm "${image}" --version)"
[[ "${got}" == "${want_version}" ]] || fail "--version answered ${got}, want ${want_version}"
echo "${got}"

echo "--- --mode=node and GetPluginInfo"
container="$(docker run -d "${image}" --mode=node --endpoint="tcp://127.0.0.1:${port}")"
trap 'docker rm -f "${container}" >/dev/null' EXIT

# grpcurl runs in the driver's network namespace; the CSI proto comes from
# the module cache, since the driver serves no reflection.
spec_dir="$(go list -m -f '{{.Dir}}' github.com/container-storage-interface/spec)"
[[ -f "${spec_dir}/csi.proto" ]] || fail "no csi.proto in the module cache: run go mod download"
grpcurl=(docker run --rm --network "container:${container}" -v "${spec_dir}:/proto:ro"
  "fullstorydev/grpcurl:${grpcurl_version}" -plaintext -import-path /proto -proto csi.proto)

info=""
for _ in $(seq 1 30); do
  if info="$("${grpcurl[@]}" -d '{}' "127.0.0.1:${port}" csi.v1.Identity/GetPluginInfo 2>/dev/null)"; then
    break
  fi
  if [[ "$(docker inspect -f '{{.State.Running}}' "${container}")" != "true" ]]; then
    docker logs "${container}" >&2
    fail "the node plugin exited"
  fi
  info=""
  sleep 1
done
[[ -n "${info}" ]] || { docker logs "${container}" >&2; fail "GetPluginInfo didn't answer"; }
echo "${info}"

name="$(jq -r .name <<<"${info}")"
version="$(jq -r .vendorVersion <<<"${info}")"
[[ "${name}" == "pvs.csi.origincloud.eu" ]] || fail "GetPluginInfo named ${name}"
[[ "${version}" == "${want_version}" ]] || fail "GetPluginInfo answered version ${version}, want ${want_version}"

echo "OK"
