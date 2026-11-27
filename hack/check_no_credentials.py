#!/usr/bin/env python3
"""Fails if rendered Kubernetes manifests carry a credential.

The driver authenticates as its instance's service account through the
metadata service, so its manifests need no credential, and must not grow one.
Reads a multi-document YAML stream (kubectl kustomize deploy/) from the file
given, or stdin, and reports every:

- Secret;
- container env var or ConfigMap key whose name is a credential's (a token,
  secret, password, key, credentials file, impersonation);
- env var or envFrom that reads a Secret;
- volume that mounts a Secret, directly or projected.

Exits 1 when it found any, 0 otherwise.
"""

import re
import sys

import yaml

CREDENTIAL_NAME = re.compile(
    r"TOKEN|SECRET|PASSW(OR)?D|CREDENTIAL|IMPERSONATE|(^|_)(API|ACCESS|PRIVATE|CLIENT)_?KEY",
    re.IGNORECASE,
)

POD_SPEC_PATHS = {
    "Pod": ("spec",),
    "Deployment": ("spec", "template", "spec"),
    "DaemonSet": ("spec", "template", "spec"),
    "StatefulSet": ("spec", "template", "spec"),
    "ReplicaSet": ("spec", "template", "spec"),
    "Job": ("spec", "template", "spec"),
    "CronJob": ("spec", "jobTemplate", "spec", "template", "spec"),
}


def dig(obj, path):
    for key in path:
        if not isinstance(obj, dict):
            return None
        obj = obj.get(key)
    return obj


def check_pod_spec(where, spec):
    problems = []
    containers = (spec.get("initContainers") or []) + (spec.get("containers") or [])
    for c in containers:
        at = f"{where} container {c.get('name')}"
        for env in c.get("env") or []:
            name = env.get("name", "")
            if CREDENTIAL_NAME.search(name):
                problems.append(f"{at}: env {name} is named like a credential")
            if dig(env, ("valueFrom", "secretKeyRef")) is not None:
                problems.append(f"{at}: env {name} reads a Secret")
        for src in c.get("envFrom") or []:
            if "secretRef" in src:
                problems.append(f"{at}: envFrom reads a Secret")
    for v in spec.get("volumes") or []:
        if "secret" in v:
            problems.append(f"{where} volume {v.get('name')}: mounts a Secret")
        for s in dig(v, ("projected", "sources")) or []:
            if "secret" in s:
                problems.append(f"{where} volume {v.get('name')}: projects a Secret")
    return problems


def check(docs):
    problems = []
    for doc in docs:
        if not isinstance(doc, dict):
            continue
        kind = doc.get("kind", "")
        where = f"{kind} {dig(doc, ('metadata', 'name'))}"
        if kind == "Secret":
            problems.append(f"{where}: is a Secret")
        if kind == "ConfigMap":
            for section in ("data", "binaryData"):
                for key in doc.get(section) or {}:
                    if CREDENTIAL_NAME.search(key):
                        problems.append(f"{where}: key {key} is named like a credential")
        path = POD_SPEC_PATHS.get(kind)
        if path:
            spec = dig(doc, path)
            if isinstance(spec, dict):
                problems.extend(check_pod_spec(where, spec))
    return problems


def main():
    stream = open(sys.argv[1]) if len(sys.argv) > 1 else sys.stdin
    with stream:
        docs = list(yaml.safe_load_all(stream))
    if not any(isinstance(d, dict) for d in docs):
        print("no manifests to check", file=sys.stderr)
        return 1
    problems = check(docs)
    for p in problems:
        print(p, file=sys.stderr)
    if problems:
        print(f"{len(problems)} credential(s) found in the manifests", file=sys.stderr)
        return 1
    print(f"no credential in {sum(isinstance(d, dict) for d in docs)} objects")
    return 0


if __name__ == "__main__":
    sys.exit(main())
