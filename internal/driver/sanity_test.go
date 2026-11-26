package driver_test

import (
	"context"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/origincloudhq/csi-driver-origincloud/internal/cloud"
	"github.com/origincloudhq/csi-driver-origincloud/internal/cloud/fake"
	"github.com/origincloudhq/csi-driver-origincloud/internal/device"
	"github.com/origincloudhq/csi-driver-origincloud/internal/driver"
	"github.com/origincloudhq/csi-driver-origincloud/internal/metadata"
	mountfake "github.com/origincloudhq/csi-driver-origincloud/internal/mount/fake"
	computev1beta1 "go.origincloud.eu/sdk/compute/v1beta1"
	pvsv1beta1 "go.origincloud.eu/sdk/pvs/v1beta1"
)

const (
	sanityZone = "eu-central1-az1"
	sanityNode = "projects/p-sanity/zones/" + sanityZone + "/instances/i-sanity"
)

// TestSanity runs the csi-test sanity suite against both plugins, each
// served on a socket of its own as in a cluster: the controller with a fake
// of the Origin Cloud API behind it, the node plugin with a fake of the
// node's mounts. The node's disks are the volumes the fake API holds
// attached to the node, so what the controller attaches the node finds.
func TestSanity(t *testing.T) {
	dir := t.TempDir()
	controllerSock := filepath.Join(dir, "controller.sock")
	nodeSock := filepath.Join(dir, "node.sock")

	f := fake.New()
	f.AddInstance(sanityNode)
	d, err := driver.New(driver.Config{
		Version:      "sanity",
		Cloud:        f.Cloud(),
		Project:      "p-sanity",
		DefaultZone:  sanityZone,
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	n, err := driver.NewNode(driver.NodeConfig{
		Version:      "sanity",
		Metadata:     sanityMetadata{},
		Mounter:      mountfake.New(),
		Devices:      attachedDisks{cloud: f.Cloud(), node: sanityNode},
		DeviceWait:   10 * time.Millisecond,
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l, err := driver.Listen("unix://" + controllerSock)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 2)
	go func() { served <- d.Serve(ctx, l) }()

	nl, err := driver.Listen("unix://" + nodeSock)
	if err != nil {
		t.Fatal(err)
	}
	go func() { served <- n.Serve(ctx, nl) }()

	cfg := sanity.NewTestConfig()
	cfg.Address = "unix://" + nodeSock
	cfg.ControllerAddress = "unix://" + controllerSock
	cfg.TargetPath = filepath.Join(dir, "target")
	cfg.StagingPath = filepath.Join(dir, "staging")
	cfg.TestVolumeSize = 10 << 30
	cfg.IDGen = idGen{}
	cfg.IdempotentCount = 2

	sc := sanity.GinkgoTest(&cfg)
	gomega.RegisterFailHandler(ginkgo.Fail)
	suite, reporter := ginkgo.GinkgoConfiguration()
	ginkgo.RunSpecs(t, "csi-sanity", suite, reporter)
	sc.Finalize()

	cancel()
	for range 2 {
		if err := <-served; err != nil {
			t.Fatal(err)
		}
	}
}

// idGen issues IDs of the driver's form, for volumes and nodes that don't
// exist.
type idGen struct{}

func (idGen) GenerateUniqueValidVolumeID() string {
	return "projects/p-sanity/zones/" + sanityZone + "/volumes/vol-" + uuid.NewString()[:13]
}
func (idGen) GenerateInvalidVolumeID() string { return "not-a-volume" }
func (idGen) GenerateUniqueValidNodeID() string {
	return "projects/p-sanity/zones/" + sanityZone + "/instances/i-" + uuid.NewString()[:13]
}
func (idGen) GenerateInvalidNodeID() string { return "not-a-node" }

// sanityMetadata is the metadata service of the sanity node.
type sanityMetadata struct{}

func (sanityMetadata) Instance(context.Context) (metadata.Instance, error) {
	return metadata.Instance{Name: "//compute.origincloudapis.eu/" + sanityNode, Zone: sanityZone}, nil
}

// attachedDisks are the node's disks: one for each volume the fake API
// holds attached to the node, named by the volume's id, as large as the
// volume.
type attachedDisks struct {
	cloud cloud.Cloud
	node  string
}

func (a attachedDisks) Find(volumeID string) (string, error) {
	for att, err := range a.cloud.Attachments.AllVolumeAttachments(context.Background(), &computev1beta1.ListVolumeAttachmentsRequest{Parent: a.node}) {
		if err != nil {
			return "", err
		}
		if att.GetState() == computev1beta1.VolumeAttachment_ATTACHED && path.Base(att.GetVolume()) == volumeID {
			return "/dev/disk/by-id/virtio-" + volumeID, nil
		}
	}
	return "", device.ErrNotFound
}

func (a attachedDisks) Size(dev string) (int64, error) {
	id := strings.TrimPrefix(dev, "/dev/disk/by-id/virtio-")
	vol, err := a.cloud.Volumes.GetVolume(context.Background(), &pvsv1beta1.GetVolumeRequest{
		Name: "projects/p-sanity/zones/" + sanityZone + "/volumes/" + id,
	})
	if err != nil {
		return 0, err
	}
	return int64(vol.GetSizeGib()) << 30, nil
}
