# Origin CSI driver

The [Container Storage Interface](https://github.com/container-storage-interface/spec)
driver for Origin Cloud's persistent volumes (PVS). It lets a Kubernetes cluster
running on Origin Cloud instances use PVS volumes as persistent volumes.

- Driver name: `pvs.csi.origincloud.eu`
- Topology key: `topology.pvs.csi.origincloud.eu/zone`
- Access modes: one node at a time (`ReadWriteOnce`, `ReadWriteOncePod`). A PVS
  volume attaches to one instance at a time, so there is no `ReadWriteMany`.

The driver calls Origin Cloud's public REST API through
[`go.origincloud.eu/sdk`](https://github.com/origincloudhq/sdk-go).

## Status

| Part | State |
|---|---|
| Identity service | Done |
| Controller plugin: create, delete, attach, detach, topology | Done |
| Node plugin: stage, publish, filesystem expansion, usage | Done |
| Snapshots, restore, volume expansion | Done |
| Container image (`linux/amd64`, `linux/arm64`) | Done |
| Install manifests (`deploy/`) | Done |

## How it works

One binary serves both plugins: `--mode=controller` runs the controller plugin
(one replica per cluster, beside the CSI sidecars), and `--mode=node` the node
plugin on every node.

- **Volume ID**: the PVS volume's resource name,
  `projects/{project}/zones/{zone}/volumes/{volume}`.
- **Node ID**: the node's instance resource name,
  `projects/{project}/zones/{zone}/instances/{instance}`.
- **CreateVolume** creates the volume in the zone the scheduler picked: the
  first preferred topology's zone (with `volumeBindingMode: WaitForFirstConsumer`,
  the zone of the node the pod goes to), else the first requisite's. The size
  is rounded up to whole GiB, at least 1. It is idempotent by the CSI volume
  name. The volume carries the name in its `csi.origincloud.eu/volume-name`
  tag, which a retry finds. The create request's `request_id` is derived from
  the name, so a retry whose first attempt reached the API doesn't create a
  second volume.
- **DeleteVolume** deletes the volume. A volume that doesn't exist is deleted
  already. A volume that is still attached is refused with `FAILED_PRECONDITION`.
- **ControllerPublishVolume** attaches the volume to the node's instance. Its
  publish context's `device` key names the attachment's disk slot, such as
  `vdb`. It is informational only (see the node plugin below). A volume attached to another node is refused with
  `FAILED_PRECONDITION`, and so is a node in another zone.
- **ControllerUnpublishVolume** detaches it again.
- **Snapshot ID**: the PVS snapshot's resource name,
  `projects/{project}/zones/{zone}/snapshots/{snapshot}`, in its volume's zone.
- **CreateSnapshot** takes a PVS snapshot of the volume, waits for the copy,
  and then **protects** it, because PVS creates a volume only from a protected
  snapshot. The snapshot is ready to use once it is both copied and protected.
  CreateSnapshot is idempotent by the CSI snapshot name, the same way
  CreateVolume is, through the `csi.origincloud.eu/snapshot-name` tag and a
  derived `request_id`. The API doesn't report a snapshot's creation time.
  The driver records the time of its first request in the
  `csi.origincloud.eu/created-at` tag and reports that.
- **DeleteSnapshot** unprotects the snapshot and deletes it. PVS doesn't
  flatten a restored volume, so it refuses to unprotect a snapshot while a
  volume restored from it exists. Until those volumes are deleted,
  DeleteSnapshot fails with `FAILED_PRECONDITION`, and Kubernetes retries it.
- **ListSnapshots** lists the snapshot the snapshot ID names, or the source
  volume's snapshots. Without either filter it lists the snapshots in
  `--default-zone`, because the controller doesn't know every zone of the
  cluster.
- **CreateVolume from a snapshot** (a `dataSource` of kind `VolumeSnapshot`)
  creates the volume in the snapshot's zone, the only zone PVS restores into.
  It is refused with `RESOURCE_EXHAUSTED` when the requisite topology leaves
  that zone out. The volume is at least the snapshot's size. A snapshot that
  isn't protected is refused with `FAILED_PRECONDITION`. Cloning a volume
  (a `dataSource` of kind `PersistentVolumeClaim`) isn't supported, because
  PVS has no clone: snapshot the volume and restore the snapshot instead.
- **ControllerExpandVolume** grows the volume, attached or not (online
  expansion), to the requested size rounded up to whole GiB. Growing it to the
  size it already has changes nothing. A smaller size is refused with
  `OUT_OF_RANGE`, because a PVS volume never shrinks. A filesystem volume then
  needs the node to grow its filesystem (`node_expansion_required`); a block
  volume doesn't.
- A call that has to wait for an operation waits until the CO's deadline. If
  the deadline passes first, the call fails with `DEADLINE_EXCEEDED`, and the
  CO's retry picks up the operation already under way.

### Node plugin

The node plugin needs no API credentials. It reads its instance from the
metadata service (`GET /origin/v1/instance`), and the controller does the
attaching.

- **NodeGetInfo**: the node ID is the instance's relative name, which is
  what the controller takes. The topology is the instance's zone. A node
  takes at most 24 volumes: an instance's disk slots run from `vda` to `vdz`,
  `vdc` is reserved by the platform, and the boot disk is on `vda`.
- **Finding the disk**: the guest names its disks in the order it finds them,
  so a disk attached to a running instance often doesn't get the name the
  attachment's `device` gives. The node plugin never uses that name. Every
  volume's disk carries the volume's id (`vol-...`) as its serial, and the
  plugin finds it as `/dev/disk/by-id/virtio-<volume id>`. Without udev's
  links, it reads each disk's serial in `/sys/block/*/serial`. It waits up to
  30 seconds for a freshly attached disk to show up. A disk that doesn't
  show up is `NOT_FOUND`.
- **NodeStageVolume** mounts a filesystem volume's disk at the staging path.
  It creates the filesystem first only when the disk holds nothing at all,
  neither a filesystem nor a partition table (as `blkid` reads it). A disk
  that holds a filesystem of another type, or a partition table, is refused
  with `FAILED_PRECONDITION` and never formatted. The filesystem is `ext4`
  unless the StorageClass sets `csi.storage.k8s.io/fstype: xfs`. A block
  volume has nothing to stage.
- **NodePublishVolume** bind-mounts the staged filesystem at the target
  path, or binds a block volume's disk onto a file there. A read-only publish
  is a read-only bind. A `ReadWriteOncePod` (single-writer) volume is refused
  at a second target path.
- **NodeUnpublishVolume** and **NodeUnstageVolume** unmount, and unpublish
  removes the target path. Each of the four calls is idempotent: a repeat
  finds what the first one did and succeeds.
- **NodeExpandVolume** grows the filesystem online (`resize2fs`,
  `xfs_growfs`) to fill the disk once the controller expanded the volume,
  and answers the disk's size as the guest sees it.
- **NodeGetVolumeStats** reports a filesystem's bytes and inodes, or a block
  volume's size.

The node plugin runs privileged, with the node's `/dev` and the kubelet's
directory mounted with bidirectional propagation, and needs `blkid`,
`mkfs.ext4`, `mkfs.xfs`, `fsck`, `resize2fs`, `xfs_growfs` and `mount` in its
image.

### StorageClass parameters

| Parameter | Value |
|---|---|
| `storageClass` | The PVS storage class by its full name, such as `//pvs.origincloudapis.eu/storageClasses/gp1`. The platform's default class when absent. |

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: origin-gp1
provisioner: pvs.csi.origincloud.eu
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: true
parameters:
  storageClass: //pvs.origincloudapis.eu/storageClasses/gp1
```

A `VolumeSnapshotClass` takes no parameters:

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: origin-snapshots
driver: pvs.csi.origincloud.eu
deletionPolicy: Delete
```

### Errors

The API's error reasons map to these gRPC codes:

| API reason | gRPC code |
|---|---|
| `NOT_FOUND` | `NOT_FOUND` |
| `CONFLICT` | `FAILED_PRECONDITION` |
| `INSUFFICIENT_CAPACITY`, an exhausted quota, `RESOURCE_EXHAUSTED` | `RESOURCE_EXHAUSTED` |
| `BAD_REQUEST` | `INVALID_ARGUMENT` |
| `UNAUTHORIZED` | `UNAUTHENTICATED` |
| `FORBIDDEN` | `PERMISSION_DENIED` |
| `SERVICE_UNAVAILABLE` | `UNAVAILABLE` |
| `PRODUCT_UNAVAILABLE`, `PAYMENT_METHOD_REQUIRED`, `LEGAL_ACCEPTANCE_REQUIRED` | `FAILED_PRECONDITION` |
| anything else | `INTERNAL` |

## Install

`deploy/` installs the driver on a Kubernetes cluster whose nodes are Origin
Cloud instances, as plain YAML with a `kustomization.yaml`:

| Object | What it is |
|---|---|
| `CSIDriver` `pvs.csi.origincloud.eu` | `attachRequired: true`, `podInfoOnMount: false`, `fsGroupPolicy: File` |
| `Deployment` `origincloud-csi-controller` | The controller plugin with the external-provisioner, -attacher, -resizer and -snapshotter sidecars and the liveness probe; one replica, the sidecars elect a leader |
| `DaemonSet` `origincloud-csi-node` | The node plugin with node-driver-registrar and the liveness probe on every node, whatever its taints |
| RBAC | A ClusterRole per sidecar, from upstream's, and a Role for their leader-election leases |
| `StorageClass` `origin-gp1` | PVS class `gp1`, the cluster's default: `WaitForFirstConsumer`, `allowVolumeExpansion: true`, `reclaimPolicy: Delete`, `ext4` |
| `VolumeSnapshotClass` `origin-snapshots` | The default snapshot class, `deletionPolicy: Delete` |

Everything goes to `kube-system`. Every image is pinned: the driver to a
release of this repository, the sidecars to kubernetes-csi releases.

There is a `StorageClass` for each PVS storage class that can be selected
today (`origin pvs list-storage-classes`, `available: true`). Only `gp1` can
be: `st1` and `io1` are published but refused, and get theirs once they're
available.

### Prerequisites

- **The nodes' service account.** The controller authenticates as the
  service account of the instance it runs on (see [Credentials](#credentials)).
  Bind `roles/pvs.csi-driver` on the cluster's project to the service
  account the nodes run as. The manifests hold no credential and no
  `Secret`, and CI fails if one appears.
- **The VolumeSnapshot CRDs and the snapshot-controller**, for snapshots.
  They are cluster-wide and often installed already, so they aren't part of
  `deploy/`. Without them, the `VolumeSnapshotClass` is refused and the
  snapshotter sidecar keeps restarting. From kubernetes-csi:

  ```sh
  kubectl apply -k 'https://github.com/kubernetes-csi/external-snapshotter/client/config/crd?ref=v8.7.1'
  kubectl apply -k 'https://github.com/kubernetes-csi/external-snapshotter/deploy/kubernetes/snapshot-controller?ref=v8.7.1'
  ```

### Install with kustomize

The controller needs the project the cluster's instances run in, where it
creates the volumes. Set it in an overlay, a directory with this
`kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - https://github.com/origincloudhq/csi-driver-origincloud/deploy?ref=v0.1.0
configMapGenerator:
  - name: origincloud-csi-controller
    namespace: kube-system
    behavior: merge
    literals:
      - ORIGINCLOUD_PROJECT=my-project
      # Optional: the region whose API to call. Empty, the regionless one.
      # - ORIGINCLOUD_REGION=eu-central1
```

```sh
kubectl apply -k ./my-overlay
kubectl -n kube-system rollout status deployment/origincloud-csi-controller
kubectl -n kube-system rollout status daemonset/origincloud-csi-node
```

Without the project, the controller exits with `the project must be a
project id`. An overlay is also where to drop the default-class
annotations, add tolerations or change resources.

Both the controller and the node plugin use the node's network
(`hostNetwork`): the metadata service, which the controller gets its
credentials from and the node plugin its instance and zone, answers only an
instance's own address. Their liveness probes take ports `29652`
(controller) and `29653` (node) on the node.

### Check the manifests

```sh
hack/check-manifests.sh
```

renders `deploy/` and checks it as the `Manifests` workflow does: every
object against the Kubernetes and snapshot CRD schemas with
[kubeconform](https://github.com/yannh/kubeconform), no `Secret` and no
credential in any object (`hack/check_no_credentials.py`), and every image
pinned. It needs `kubectl`, `kubeconform` and Python with PyYAML.

## Image

The driver ships as one multi-arch image (`linux/amd64`, `linux/arm64`) for
both plugins:

```
ghcr.io/origincloudhq/csi-driver-origincloud:<version>
```

such as `ghcr.io/origincloudhq/csi-driver-origincloud:v0.1.0`. There is no
`latest` tag: pin a version. The image holds the static binary as its
entrypoint, on a Debian slim base with the tools the node plugin runs
(`blkid`, `mkfs.ext4`, `mkfs.xfs`, `fsck`, `resize2fs`, `xfs_growfs`,
`mount`, `umount`). `GetPluginInfo` and `--version` answer the image's
version.

## Credentials

The driver authenticates as the service account of the instance it runs on,
through the instance's metadata service. It reads no other credentials:
no key file, no token in the environment, no Kubernetes secret. Give the
instances that run the controller a service account bound to the built-in
role `roles/pvs.csi-driver` on the cluster's project. That role allows
exactly what the driver calls: volumes, snapshots (protecting and
unprotecting them too), and attaching to and detaching from instances.
The node plugin calls no API and needs no service account.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--mode` | | `controller` or `node` |
| `--endpoint` | `unix:///csi/csi.sock` | Where to serve CSI: `unix:///path` or `tcp://host:port` |
| `--project` | `$ORIGINCLOUD_PROJECT` | Controller: the project to create volumes in |
| `--region` | `$ORIGINCLOUD_REGION` | Controller: the region whose API to call |
| `--default-zone` | | Controller: the zone for a volume whose request names none |
| `--version` | | Print the version and exit |

## Development

```sh
go test -race ./...
```

The tests include the [csi-test](https://github.com/kubernetes-csi/csi-test)
sanity suite's Identity, Controller and Node tests. They run against both
plugins, each served on its own socket. The controller has an in-memory fake
of the API behind it (`internal/cloud/fake`). The node plugin has a fake of
the node's mounts (`internal/mount/fake`), whose disks are the volumes the
fake API holds attached to the node.

To build and check the image locally:

```sh
docker build --build-arg VERSION=v0.0.0-dev -t csi-driver-origincloud:dev .
go mod download
hack/smoke-test-image.sh csi-driver-origincloud:dev v0.0.0-dev
```

`hack/smoke-test-image.sh` checks that every tool the node plugin runs is on
the image's `PATH`, and that the image starts with `--mode=node` and answers
`GetPluginInfo` with the given version. The `Image` workflow runs it on every
pull request, for each architecture on a runner of that architecture.

## Releases

A release is a version tag on `main`, `vMAJOR.MINOR.PATCH` (a pre-release
such as `v0.2.0-rc.1` works too). First set the driver's image in
`deploy/controller.yaml` and `deploy/node.yaml`, and the `?ref=` in the
install section, to the new version, and merge that: the manifests at a tag
install that tag's image. Then tag:

```sh
git switch main && git pull
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

The tag's `Image` workflow builds and checks both architectures, then pushes
the multi-arch image to `ghcr.io/origincloudhq/csi-driver-origincloud:v0.1.0`
and checks that its manifest list holds both. The version in the image,
`GetPluginInfo` and `--version`, is the tag. To look at a published release:

```sh
docker buildx imagetools inspect ghcr.io/origincloudhq/csi-driver-origincloud:v0.1.0
```

## License

Apache License 2.0; see [LICENSE](LICENSE).
