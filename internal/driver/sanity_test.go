package driver_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/google/uuid"
	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/origincloudhq/csi-driver-origincloud/internal/cloud/fake"
	"github.com/origincloudhq/csi-driver-origincloud/internal/driver"
	"google.golang.org/grpc"
)

const (
	sanityZone = "eu-central1-az1"
	sanityNode = "projects/p-sanity/zones/" + sanityZone + "/instances/i-sanity"
)

// TestSanity runs the csi-test sanity suite's Identity and Controller tests
// against the controller plugin, served on a socket as in a cluster, with a
// fake of the Origin Cloud API behind it.
//
// The suite's controller tests ask the node service for the node's ID and
// topology; a stub answers that on a socket of its own. The Node Service
// tests are skipped: the node plugin isn't part of this binary yet.
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l, err := driver.Listen("unix://" + controllerSock)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx, l) }()

	nl, err := driver.Listen("unix://" + nodeSock)
	if err != nil {
		t.Fatal(err)
	}
	nodeSrv := grpc.NewServer()
	csi.RegisterIdentityServer(nodeSrv, d)
	csi.RegisterNodeServer(nodeSrv, nodeStub{})
	go func() { _ = nodeSrv.Serve(nl) }()
	defer nodeSrv.Stop()

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
	suite.SkipStrings = append(suite.SkipStrings, "Node Service")
	ginkgo.RunSpecs(t, "csi-sanity", suite, reporter)
	sc.Finalize()

	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
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

// nodeStub answers what the controller tests ask a node: who it is and
// where.
type nodeStub struct {
	csi.UnimplementedNodeServer
}

func (nodeStub) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId:             sanityNode,
		AccessibleTopology: &csi.Topology{Segments: map[string]string{driver.TopologyZoneKey: sanityZone}},
	}, nil
}

func (nodeStub) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{}, nil
}

// The suite's cleanup unpublishes and unstages on the node whatever it
// published; with no node plugin there is nothing to undo.
func (nodeStub) NodeUnpublishVolume(context.Context, *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (nodeStub) NodeUnstageVolume(context.Context, *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return &csi.NodeUnstageVolumeResponse{}, nil
}
