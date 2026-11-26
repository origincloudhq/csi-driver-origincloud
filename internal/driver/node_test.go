package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/origincloudhq/csi-driver-origincloud/internal/device"
	"github.com/origincloudhq/csi-driver-origincloud/internal/metadata"
	mountfake "github.com/origincloudhq/csi-driver-origincloud/internal/mount/fake"
	"google.golang.org/grpc/codes"
)

const (
	volumeID = parent + "/volumes/vol-a"
	disk     = "/dev/vdb"
)

// disks are the node's disks, by the volume id they carry.
type disks struct {
	mu    sync.Mutex
	byID  map[string]string
	sizes map[string]int64
}

func (d *disks) Find(volumeID string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if dev, ok := d.byID[volumeID]; ok {
		return dev, nil
	}
	return "", device.ErrNotFound
}

func (d *disks) Size(dev string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sizes[dev], nil
}

func (d *disks) attach(volumeID, dev string, size int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.byID[volumeID] = dev
	d.sizes[dev] = size
}

type instance metadata.Instance

func (i instance) Instance(context.Context) (metadata.Instance, error) {
	if i.Name == "" {
		return metadata.Instance{}, errors.New("no metadata service")
	}
	return metadata.Instance(i), nil
}

type nodeEnv struct {
	n     *Node
	m     *mountfake.Mounter
	disks *disks
	dir   string
}

// newNode returns a node plugin on the instance `node`, to which the
// volume `volumeID` is attached as `disk`.
func newNode(t *testing.T) nodeEnv {
	t.Helper()
	m := mountfake.New()
	d := &disks{byID: map[string]string{}, sizes: map[string]int64{}}
	d.attach("vol-a", disk, 5*gib)
	n, err := NewNode(NodeConfig{
		Metadata:     instance{Name: "//compute.origincloudapis.eu/" + node, Zone: zone},
		Mounter:      m,
		Devices:      d,
		DeviceWait:   20 * time.Millisecond,
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return nodeEnv{n: n, m: m, disks: d, dir: t.TempDir()}
}

func (e nodeEnv) staging() string { return filepath.Join(e.dir, "staging") }
func (e nodeEnv) target() string  { return filepath.Join(e.dir, "pods", "target") }

func fsCap(fsType string, mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: fsType}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}

func blockCap() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

func (e nodeEnv) stage(vc *csi.VolumeCapability) error {
	_, err := e.n.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: e.staging(), VolumeCapability: vc,
	})
	return err
}

func (e nodeEnv) publish(target string, vc *csi.VolumeCapability, readOnly bool) error {
	_, err := e.n.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: e.staging(), TargetPath: target, VolumeCapability: vc, Readonly: readOnly,
	})
	return err
}

func (e nodeEnv) mountsAt(path string) int {
	ms, _ := e.m.List()
	return len(at(ms, path))
}

func TestNodeGetInfo(t *testing.T) {
	e := newNode(t)
	info, err := e.n.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if info.GetNodeId() != node {
		t.Errorf("node ID %q, want the instance's relative name %q", info.GetNodeId(), node)
	}
	if got := info.GetAccessibleTopology().GetSegments()[TopologyZoneKey]; got != zone {
		t.Errorf("zone %q, want %q", got, zone)
	}
	if _, ok := parseNodeID(info.GetNodeId()); !ok {
		t.Errorf("the controller doesn't take the node ID %q", info.GetNodeId())
	}
}

func TestNodeGetInfoRefusesAnInstanceNameItCantUse(t *testing.T) {
	for name, inst := range map[string]instance{
		"no metadata service": {},
		"not compute":         {Name: "//pvs.origincloudapis.eu/" + volumeID, Zone: zone},
		"relative":            {Name: node, Zone: zone},
		"another zone":        {Name: "//compute.origincloudapis.eu/" + node, Zone: "eu-central1-az2"},
	} {
		e := newNode(t)
		e.n.cfg.Metadata = inst
		if _, err := e.n.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{}); err == nil {
			t.Errorf("%s: NodeGetInfo succeeded", name)
		}
	}
}

func TestStageFormatsAnEmptyDiskOnceAndIsIdempotent(t *testing.T) {
	e := newNode(t)
	for range 3 {
		if err := e.stage(fsCap("", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.m.Formats[disk]; got != 1 {
		t.Errorf("formatted %d times, want once", got)
	}
	if fs, _ := e.m.Filesystem(disk); fs != "ext4" {
		t.Errorf("the disk holds %q, want the default ext4", fs)
	}
	if got := e.mountsAt(e.staging()); got != 1 {
		t.Errorf("%d mounts at the staging path, want 1", got)
	}
}

func TestStageAsXFS(t *testing.T) {
	e := newNode(t)
	if err := e.stage(fsCap("xfs", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)); err != nil {
		t.Fatal(err)
	}
	if fs, _ := e.m.Filesystem(disk); fs != "xfs" {
		t.Errorf("the disk holds %q, want xfs", fs)
	}
}

func TestStageNeverFormatsADiskThatHoldsData(t *testing.T) {
	// Staged again on another node, the volume keeps its data.
	e := newNode(t)
	e.m.SetFilesystem(disk, "ext4")
	if err := e.stage(fsCap("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)); err != nil {
		t.Fatal(err)
	}
	if got := e.m.Formats[disk]; got != 0 {
		t.Errorf("formatted a disk that holds ext4 %d times", got)
	}

	for _, holds := range []string{"xfs", "partition table"} {
		e := newNode(t)
		e.m.SetFilesystem(disk, holds)
		err := e.stage(fsCap("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER))
		wantCode(t, err, codes.FailedPrecondition)
		if got := e.m.Formats[disk]; got != 0 {
			t.Errorf("formatted a disk that holds %s", holds)
		}
		if fs, _ := e.m.Filesystem(disk); fs != holds {
			t.Errorf("the disk holds %q now, want %q still", fs, holds)
		}
	}
}

func TestStageRefusesAnUnsupportedFilesystem(t *testing.T) {
	e := newNode(t)
	wantCode(t, e.stage(fsCap("ntfs", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)), codes.InvalidArgument)
}

func TestStageRefusesAStagingPathThatHoldsAnotherDisk(t *testing.T) {
	e := newNode(t)
	if err := e.m.Mount("/dev/vdz", e.staging(), "ext4", nil); err != nil {
		t.Fatal(err)
	}
	wantCode(t, e.stage(fsCap("", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)), codes.FailedPrecondition)
}

func TestStageWaitsForTheDiskToShowUp(t *testing.T) {
	e := newNode(t)
	e.disks = &disks{byID: map[string]string{}, sizes: map[string]int64{}}
	e.n.cfg.Devices = e.disks
	e.n.cfg.DeviceWait = 5 * time.Second
	go func() {
		time.Sleep(20 * time.Millisecond)
		e.disks.attach("vol-a", disk, gib)
	}()
	if err := e.stage(fsCap("", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)); err != nil {
		t.Fatal(err)
	}
}

func TestStageAVolumeNotAttachedHere(t *testing.T) {
	e := newNode(t)
	_, err := e.n.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId: parent + "/volumes/vol-elsewhere", StagingTargetPath: e.staging(),
		VolumeCapability: fsCap("", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	})
	wantCode(t, err, codes.NotFound)
}

func TestStageBlockDoesNothing(t *testing.T) {
	e := newNode(t)
	if err := e.stage(blockCap()); err != nil {
		t.Fatal(err)
	}
	if ms, _ := e.m.List(); len(ms) != 0 || e.m.Formats[disk] != 0 {
		t.Errorf("staging a block volume mounted %v or formatted", ms)
	}
}

func TestPublishFilesystemIsIdempotentAndHonoursReadOnly(t *testing.T) {
	e := newNode(t)
	vc := fsCap("", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
	if err := e.stage(vc); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := e.publish(e.target(), vc, true); err != nil {
			t.Fatal(err)
		}
	}
	ms, _ := e.m.List()
	pub := at(ms, e.target())
	if len(pub) != 1 {
		t.Fatalf("%d mounts at the target, want 1", len(pub))
	}
	if pub[0].Device != disk || !slices.Contains(pub[0].Options, "bind") || !slices.Contains(pub[0].Options, "ro") {
		t.Errorf("published %+v, want a read-only bind of %s", pub[0], disk)
	}
	if fi, err := os.Stat(e.target()); err != nil || !fi.IsDir() {
		t.Errorf("the target path isn't a directory: %v", err)
	}
}

func TestPublishFilesystemNeedsTheVolumeStaged(t *testing.T) {
	e := newNode(t)
	wantCode(t, e.publish(e.target(), fsCap("", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER), false), codes.FailedPrecondition)
}

func TestPublishBlockBindsTheDiskOntoAFile(t *testing.T) {
	e := newNode(t)
	for range 2 {
		if err := e.publish(e.target(), blockCap(), false); err != nil {
			t.Fatal(err)
		}
	}
	ms, _ := e.m.List()
	pub := at(ms, e.target())
	if len(pub) != 1 || pub[0].Device != disk || slices.Contains(pub[0].Options, "ro") {
		t.Fatalf("mounts at the target %+v, want one read-write bind of %s", pub, disk)
	}
	if fi, err := os.Stat(e.target()); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("the target path isn't a file: %v", err)
	}
}

func TestPublishSingleWriterOnlyOnce(t *testing.T) {
	e := newNode(t)
	single := fsCap("", csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER)
	if err := e.stage(single); err != nil {
		t.Fatal(err)
	}
	if err := e.publish(e.target(), single, false); err != nil {
		t.Fatal(err)
	}
	wantCode(t, e.publish(filepath.Join(e.dir, "other"), single, false), codes.FailedPrecondition)

	// Multi-writer on one node may be published twice.
	multi := fsCap("", csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER)
	if err := e.publish(filepath.Join(e.dir, "other"), multi, false); err != nil {
		t.Fatal(err)
	}
}

func TestUnpublishAndUnstageAreIdempotent(t *testing.T) {
	e := newNode(t)
	vc := fsCap("", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
	if err := e.stage(vc); err != nil {
		t.Fatal(err)
	}
	if err := e.publish(e.target(), vc, false); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := e.n.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{VolumeId: volumeID, TargetPath: e.target()}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(e.target()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the target path is still there: %v", err)
	}
	for range 2 {
		if _, err := e.n.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{VolumeId: volumeID, StagingTargetPath: e.staging()}); err != nil {
			t.Fatal(err)
		}
	}
	if ms, _ := e.m.List(); len(ms) != 0 {
		t.Errorf("mounts left: %+v", ms)
	}
}

func TestExpandGrowsTheFilesystem(t *testing.T) {
	e := newNode(t)
	vc := fsCap("", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
	if err := e.stage(vc); err != nil {
		t.Fatal(err)
	}
	if err := e.publish(e.target(), vc, false); err != nil {
		t.Fatal(err)
	}
	e.disks.attach("vol-a", disk, 8*gib) // the controller expanded it
	resp, err := e.n.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId: volumeID, VolumePath: e.target(), CapacityRange: &csi.CapacityRange{RequiredBytes: 8 * gib},
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.m.Resizes[disk] != 1 {
		t.Errorf("resized %d times, want once", e.m.Resizes[disk])
	}
	if resp.GetCapacityBytes() != 8*gib {
		t.Errorf("capacity %d, want %d", resp.GetCapacityBytes(), 8*gib)
	}
}

func TestExpandBlockHasNoFilesystemToGrow(t *testing.T) {
	e := newNode(t)
	if err := e.publish(e.target(), blockCap(), false); err != nil {
		t.Fatal(err)
	}
	resp, err := e.n.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId: volumeID, VolumePath: e.target(), VolumeCapability: blockCap(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.m.Resizes[disk] != 0 || resp.GetCapacityBytes() != 5*gib {
		t.Errorf("resized %d times, capacity %d; want no resize, %d", e.m.Resizes[disk], resp.GetCapacityBytes(), 5*gib)
	}
}

func TestExpandAVolumeNotMountedAtThePath(t *testing.T) {
	e := newNode(t)
	_, err := e.n.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{VolumeId: volumeID, VolumePath: e.target()})
	wantCode(t, err, codes.NotFound)
}

func TestVolumeStats(t *testing.T) {
	e := newNode(t)
	vc := fsCap("", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
	if err := e.stage(vc); err != nil {
		t.Fatal(err)
	}
	if err := e.publish(e.target(), vc, false); err != nil {
		t.Fatal(err)
	}
	resp, err := e.n.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: volumeID, VolumePath: e.target()})
	if err != nil {
		t.Fatal(err)
	}
	var bytes, inodes *csi.VolumeUsage
	for _, u := range resp.GetUsage() {
		switch u.GetUnit() {
		case csi.VolumeUsage_BYTES:
			bytes = u
		case csi.VolumeUsage_INODES:
			inodes = u
		}
	}
	if bytes.GetTotal() != e.m.Usage.TotalBytes || bytes.GetAvailable() != e.m.Usage.AvailableBytes || inodes.GetUsed() != e.m.Usage.UsedInodes {
		t.Errorf("usage %v, want the filesystem's %+v", resp.GetUsage(), e.m.Usage)
	}

	block := newNode(t)
	if err := block.publish(block.target(), blockCap(), false); err != nil {
		t.Fatal(err)
	}
	resp, err = block.n.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: volumeID, VolumePath: block.target()})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetUsage()) != 1 || resp.GetUsage()[0].GetTotal() != 5*gib {
		t.Errorf("block usage %v, want the disk's size", resp.GetUsage())
	}
}

func TestConcurrentCallsOnOneVolumeAreAborted(t *testing.T) {
	e := newNode(t)
	unlock, err := e.n.locks.lock(volumeID)
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, e.stage(fsCap("", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)), codes.Aborted)
	unlock()
	if err := e.stage(fsCap("", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)); err != nil {
		t.Fatal(err)
	}
}
