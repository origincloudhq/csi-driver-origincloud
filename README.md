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
| Node plugin: stage, publish, filesystem expansion | Not yet: `--mode=node` exits with an error |
| Snapshots, restore, volume expansion | Not yet |
| Container image, manifests | Not yet |

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
  publish context's `device` key names the device the guest sees it under,
  such as `vdb`. A volume attached to another node is refused with
  `FAILED_PRECONDITION`, and so is a node in another zone.
- **ControllerUnpublishVolume** detaches it again.
- A call that has to wait for an operation waits until the CO's deadline. If
  the deadline passes first, the call fails with `DEADLINE_EXCEEDED`, and the
  CO's retry picks up the operation already under way.

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
parameters:
  storageClass: //pvs.origincloudapis.eu/storageClasses/gp1
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

## Credentials

The driver authenticates as the service account of the instance it runs on,
through the instance's metadata service. It reads no other credentials:
no key file, no token in the environment, no Kubernetes secret. Give the
instances that run the controller a service account bound to the built-in
role `roles/pvs.csi-driver` on the cluster's project. That role allows
exactly what the driver calls: volumes, snapshots, and attaching to and
detaching from instances.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--mode` | | `controller` or `node` |
| `--endpoint` | `unix:///csi/csi.sock` | Where to serve CSI: `unix:///path` or `tcp://host:port` |
| `--project` | `$ORIGINCLOUD_PROJECT` | Controller: the project to create volumes in |
| `--region` | `$ORIGINCLOUD_REGION` | The region whose API to call |
| `--default-zone` | | Controller: the zone for a volume whose request names none |
| `--version` | | Print the version and exit |

## Development

```sh
go test -race ./...
```

The tests include the [csi-test](https://github.com/kubernetes-csi/csi-test)
sanity suite's Identity and Controller tests. They run against the controller
served on a socket, with an in-memory fake of the API behind it
(`internal/cloud/fake`).

## License

Apache License 2.0; see [LICENSE](LICENSE).
