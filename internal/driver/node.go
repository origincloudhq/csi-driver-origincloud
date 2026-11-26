package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/origincloudhq/csi-driver-origincloud/internal/device"
	"github.com/origincloudhq/csi-driver-origincloud/internal/metadata"
	"github.com/origincloudhq/csi-driver-origincloud/internal/mount"
	"go.origincloud.eu/sdk/core"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxVolumesPerNode is how many volumes a node takes: an instance has the
// disk slots vda to vdz, of which the platform reserves vdc and the boot
// disk holds vda.
const maxVolumesPerNode = 24

// defaultFsType is the filesystem a volume gets when its StorageClass
// names none (`csi.storage.k8s.io/fstype`).
const defaultFsType = "ext4"

// fsTypes are the filesystems the node plugin creates and grows.
var fsTypes = map[string]bool{"ext4": true, "xfs": true}

// Metadata tells the node plugin which instance it runs on.
type Metadata interface {
	Instance(ctx context.Context) (metadata.Instance, error)
}

// Devices finds a volume's disk on the node. Find returns an error that
// is device.ErrNotFound when no disk carries the volume's id.
type Devices interface {
	Find(volumeID string) (string, error)
	Size(device string) (int64, error)
}

// NodeConfig configures the node plugin.
type NodeConfig struct {
	// Version is the driver's version, reported by GetPluginInfo.
	Version string
	// Metadata is the instance's metadata service.
	Metadata Metadata
	// Mounter formats and mounts on the node.
	Mounter mount.Mounter
	// Devices finds the volumes' disks.
	Devices Devices
	// DeviceWait is how long a call waits for an attached volume's disk to
	// show up on the node, which takes the guest a moment after the
	// controller attached it. 30 seconds when 0.
	DeviceWait time.Duration
	// PollInterval is how often it looks meanwhile. 100ms when 0.
	PollInterval time.Duration
}

// Node serves the CSI Identity and Node services: the node plugin. It
// needs no API credentials: it learns its instance from the metadata
// service, and the controller attaches the volumes.
type Node struct {
	identity
	csi.UnimplementedNodeServer

	cfg   NodeConfig
	locks volumeLocks
}

// NewNode returns the node plugin for cfg.
func NewNode(cfg NodeConfig) (*Node, error) {
	if cfg.Metadata == nil || cfg.Mounter == nil || cfg.Devices == nil {
		return nil, errors.New("the node plugin needs the metadata service, a mounter and the devices")
	}
	if cfg.DeviceWait == 0 {
		cfg.DeviceWait = 30 * time.Second
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 100 * time.Millisecond
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	return &Node{identity: identity{version: cfg.Version}, cfg: cfg, locks: volumeLocks{held: map[string]bool{}}}, nil
}

// NodeGetCapabilities lists what the node plugin does: it stages a
// volume once per node and publishes it from there, reports usage and
// grows filesystems. SINGLE_NODE_MULTI_WRITER matches the controller's.
func (n *Node) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	var caps []*csi.NodeServiceCapability
	for _, t := range []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
		csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
		csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	} {
		caps = append(caps, &csi.NodeServiceCapability{Type: &csi.NodeServiceCapability_Rpc{
			Rpc: &csi.NodeServiceCapability_RPC{Type: t},
		}})
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: caps}, nil
}

// NodeGetInfo names the node by its instance and places it in its zone.
// The node ID is the instance's relative name, as the controller takes it.
func (n *Node) NodeGetInfo(ctx context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	inst, err := n.cfg.Metadata.Instance(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "reading the instance from the metadata service: %v", err)
	}
	nodeID, err := nodeIDOf(inst)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeGetInfoResponse{
		NodeId:             nodeID,
		MaxVolumesPerNode:  maxVolumesPerNode,
		AccessibleTopology: &csi.Topology{Segments: map[string]string{TopologyZoneKey: inst.Zone}},
	}, nil
}

// nodeIDOf turns the instance's full name into the node ID, its relative
// name.
func nodeIDOf(inst metadata.Instance) (string, error) {
	name, err := core.ParseFullName(inst.Name)
	if err != nil || name.Service() != "compute" {
		return "", fmt.Errorf("the metadata service names the instance %q, which isn't a compute instance's full name", inst.Name)
	}
	z, ok := parseNodeID(name.Relative())
	if !ok {
		return "", fmt.Errorf("the metadata service names the instance %q, which isn't a zonal instance", inst.Name)
	}
	if z.Zone != inst.Zone {
		return "", fmt.Errorf("the metadata service names the instance %q but the zone %q", inst.Name, inst.Zone)
	}
	return name.Relative(), nil
}

// NodeStageVolume mounts a filesystem volume's disk at the staging path,
// creating the filesystem first if the disk holds nothing at all. A disk
// that holds anything, a filesystem of another type included, is never
// formatted. A block volume has nothing to stage: it is published from its
// disk.
func (n *Node) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeID, staging := req.GetVolumeId(), req.GetStagingTargetPath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume ID is missing")
	}
	if staging == "" {
		return nil, status.Error(codes.InvalidArgument, "the staging target path is missing")
	}
	vc := req.GetVolumeCapability()
	if vc == nil {
		return nil, status.Error(codes.InvalidArgument, "the volume capability is missing")
	}
	if err := checkCapabilities([]*csi.VolumeCapability{vc}); err != nil {
		return nil, err
	}
	vz, ok := parseVolumeID(volumeID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "volume %q not found", volumeID)
	}
	unlock, err := n.locks.lock(volumeID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if vc.GetBlock() != nil {
		return &csi.NodeStageVolumeResponse{}, nil
	}
	fsType, err := fsTypeOf(vc.GetMount())
	if err != nil {
		return nil, err
	}
	dev, err := n.device(ctx, volumeID, vz, true)
	if err != nil {
		return nil, err
	}
	mounts, err := n.mounts()
	if err != nil {
		return nil, err
	}
	if m, ok := latest(mounts, staging); ok {
		if sameDevice(m.Device, dev) {
			return &csi.NodeStageVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.FailedPrecondition, "the staging path %s holds %s, not volume %s's disk %s", staging, orNothing(m.Device), volumeID, dev)
	}
	holds, err := n.cfg.Mounter.Filesystem(dev)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "volume %s: %v", volumeID, err)
	}
	if holds != "" && holds != fsType {
		return nil, status.Errorf(codes.FailedPrecondition, "the disk %s of volume %s holds %s, not %s; the driver never formats a disk that holds data", dev, volumeID, holds, fsType)
	}
	if err := os.MkdirAll(staging, 0o750); err != nil {
		return nil, status.Errorf(codes.Internal, "creating the staging path: %v", err)
	}
	if err := n.cfg.Mounter.FormatAndMount(dev, staging, fsType, vc.GetMount().GetMountFlags()); err != nil {
		return nil, status.Errorf(codes.Internal, "mounting volume %s's disk %s at %s: %v", volumeID, dev, staging, err)
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

// NodeUnstageVolume unmounts the staging path. One that holds nothing is
// unstaged already.
func (n *Node) NodeUnstageVolume(_ context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID, staging := req.GetVolumeId(), req.GetStagingTargetPath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume ID is missing")
	}
	if staging == "" {
		return nil, status.Error(codes.InvalidArgument, "the staging target path is missing")
	}
	unlock, err := n.locks.lock(volumeID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := n.unmountAll(staging); err != nil {
		return nil, err
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodePublishVolume makes the volume available at the target path: a
// bind mount of the staged filesystem, or of the disk itself onto a file
// for a block volume. A read-only publish is a read-only bind. A
// single-writer volume is refused at a second target path.
func (n *Node) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID, target, staging := req.GetVolumeId(), req.GetTargetPath(), req.GetStagingTargetPath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume ID is missing")
	}
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "the target path is missing")
	}
	vc := req.GetVolumeCapability()
	if vc == nil {
		return nil, status.Error(codes.InvalidArgument, "the volume capability is missing")
	}
	if err := checkCapabilities([]*csi.VolumeCapability{vc}); err != nil {
		return nil, err
	}
	block := vc.GetBlock() != nil
	if !block {
		if staging == "" {
			return nil, status.Error(codes.InvalidArgument, "the staging target path is missing")
		}
		if _, err := fsTypeOf(vc.GetMount()); err != nil {
			return nil, err
		}
	}
	vz, ok := parseVolumeID(volumeID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "volume %q not found", volumeID)
	}
	unlock, err := n.locks.lock(volumeID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// A staged volume's disk is there already; a block volume's may still
	// be on its way.
	dev, err := n.device(ctx, volumeID, vz, block)
	if err != nil {
		return nil, err
	}
	mounts, err := n.mounts()
	if err != nil {
		return nil, err
	}
	if m, ok := latest(mounts, target); ok {
		if sameDevice(m.Device, dev) {
			return &csi.NodePublishVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.FailedPrecondition, "the target path %s holds %s, not volume %s's disk %s", target, orNothing(m.Device), volumeID, dev)
	}
	if vc.GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER {
		for _, m := range mounts {
			p := filepath.Clean(m.Path)
			if sameDevice(m.Device, dev) && p != filepath.Clean(staging) && p != filepath.Clean(target) {
				return nil, status.Errorf(codes.FailedPrecondition, "volume %s is single-writer and published at %s already", volumeID, m.Path)
			}
		}
	}
	options := []string{"bind"}
	if req.GetReadonly() {
		options = append(options, "ro")
	}

	if block {
		if err := publishBlock(n.cfg.Mounter, dev, target, options); err != nil {
			return nil, status.Errorf(codes.Internal, "publishing volume %s at %s: %v", volumeID, target, err)
		}
		return &csi.NodePublishVolumeResponse{}, nil
	}
	staged := false
	for _, m := range at(mounts, staging) {
		staged = staged || sameDevice(m.Device, dev)
	}
	if !staged {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s isn't staged at %s", volumeID, staging)
	}
	if err := os.MkdirAll(target, 0o750); err != nil {
		return nil, status.Errorf(codes.Internal, "creating the target path: %v", err)
	}
	options = append(options, vc.GetMount().GetMountFlags()...)
	if err := n.cfg.Mounter.Mount(staging, target, "", options); err != nil {
		return nil, status.Errorf(codes.Internal, "publishing volume %s at %s: %v", volumeID, target, err)
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

// publishBlock binds the disk onto a file at target, which it creates,
// and removes the file again if the bind fails.
func publishBlock(m mount.Mounter, dev, target string, options []string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	created := err == nil
	switch {
	case created:
		_ = f.Close()
	case !errors.Is(err, os.ErrExist):
		return err
	}
	if err := m.Mount(dev, target, "", options); err != nil {
		if created {
			_ = os.Remove(target)
		}
		return err
	}
	return nil
}

// NodeUnpublishVolume unmounts the target path and removes it. A target
// path that is gone is unpublished already.
func (n *Node) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID, target := req.GetVolumeId(), req.GetTargetPath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume ID is missing")
	}
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "the target path is missing")
	}
	unlock, err := n.locks.lock(volumeID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := n.unmountAll(target); err != nil {
		return nil, err
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, status.Errorf(codes.Internal, "removing the target path: %v", err)
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeGetVolumeStats reports the usage of the volume at the path: its
// filesystem's bytes and inodes, or a block volume's size.
func (n *Node) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	dev, isBlock, err := n.mountedVolume(ctx, req.GetVolumeId(), req.GetVolumePath(), nil)
	if err != nil {
		return nil, err
	}
	if isBlock {
		size, err := n.cfg.Devices.Size(dev)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "volume %s: %v", req.GetVolumeId(), err)
		}
		return &csi.NodeGetVolumeStatsResponse{Usage: []*csi.VolumeUsage{
			{Unit: csi.VolumeUsage_BYTES, Total: size},
		}}, nil
	}
	s, err := n.cfg.Mounter.Stats(req.GetVolumePath())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "volume %s: %v", req.GetVolumeId(), err)
	}
	return &csi.NodeGetVolumeStatsResponse{Usage: []*csi.VolumeUsage{
		{Unit: csi.VolumeUsage_BYTES, Total: s.TotalBytes, Available: s.AvailableBytes, Used: s.UsedBytes},
		{Unit: csi.VolumeUsage_INODES, Total: s.TotalInodes, Available: s.AvailableInodes, Used: s.UsedInodes},
	}}, nil
}

// NodeExpandVolume grows the volume's filesystem online to fill its disk,
// once the controller expanded the volume and the guest sees the disk's
// new size. A block volume has no filesystem to grow. It answers the
// disk's size as the guest sees it.
func (n *Node) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	dev, isBlock, err := n.mountedVolume(ctx, req.GetVolumeId(), req.GetVolumePath(), req.GetVolumeCapability())
	if err != nil {
		return nil, err
	}
	unlock, err := n.locks.lock(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	defer unlock()
	if !isBlock {
		if err := n.cfg.Mounter.Resize(dev, req.GetVolumePath()); err != nil {
			return nil, status.Errorf(codes.Internal, "volume %s: %v", req.GetVolumeId(), err)
		}
	}
	size, err := n.cfg.Devices.Size(dev)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "volume %s: %v", req.GetVolumeId(), err)
	}
	return &csi.NodeExpandVolumeResponse{CapacityBytes: size}, nil
}

// mountedVolume checks that the volume is mounted at path and returns its
// disk, and whether it is published as a block device there (by the
// capability, else by path being a file).
func (n *Node) mountedVolume(ctx context.Context, volumeID, path string, vc *csi.VolumeCapability) (string, bool, error) {
	if volumeID == "" {
		return "", false, status.Error(codes.InvalidArgument, "the volume ID is missing")
	}
	if path == "" {
		return "", false, status.Error(codes.InvalidArgument, "the volume path is missing")
	}
	vz, ok := parseVolumeID(volumeID)
	if !ok {
		return "", false, status.Errorf(codes.NotFound, "volume %q not found", volumeID)
	}
	dev, err := n.device(ctx, volumeID, vz, false)
	if err != nil {
		return "", false, err
	}
	mounts, err := n.mounts()
	if err != nil {
		return "", false, err
	}
	mounted := false
	for _, m := range at(mounts, path) {
		mounted = mounted || sameDevice(m.Device, dev)
	}
	if !mounted {
		return "", false, status.Errorf(codes.NotFound, "volume %s isn't mounted at %s", volumeID, path)
	}
	if vc != nil {
		return dev, vc.GetBlock() != nil, nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", false, status.Errorf(codes.NotFound, "volume %s at %s: %v", volumeID, path, err)
	}
	return dev, !fi.IsDir(), nil
}

// device finds the volume's disk on the node, waiting up to DeviceWait for
// it to show up when wait is set. A disk that doesn't show up is NotFound:
// the volume isn't attached to this node.
func (n *Node) device(ctx context.Context, volumeID string, vz zonal, wait bool) (string, error) {
	deadline := time.Now().Add(n.cfg.DeviceWait)
	for {
		dev, err := n.cfg.Devices.Find(vz.ID)
		switch {
		case err == nil:
			return dev, nil
		case !errors.Is(err, device.ErrNotFound):
			return "", status.Errorf(codes.Internal, "finding the disk of volume %s: %v", volumeID, err)
		case !wait || time.Now().After(deadline):
			return "", status.Errorf(codes.NotFound, "volume %s isn't attached to this node: %v", volumeID, err)
		}
		t := time.NewTimer(n.cfg.PollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return "", apiStatus(ctx.Err(), "waiting for the disk of volume "+volumeID)
		case <-t.C:
		}
	}
}

func (n *Node) mounts() ([]mount.Mount, error) {
	mounts, err := n.cfg.Mounter.List()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return mounts, nil
}

// unmountAll unmounts every mount stacked at path.
func (n *Node) unmountAll(path string) error {
	mounts, err := n.mounts()
	if err != nil {
		return err
	}
	for range at(mounts, path) {
		if err := n.cfg.Mounter.Unmount(path); err != nil {
			return status.Errorf(codes.Internal, "unmounting %s: %v", path, err)
		}
	}
	return nil
}

// at returns the mounts at path, latest last.
func at(mounts []mount.Mount, path string) []mount.Mount {
	path = filepath.Clean(path)
	var out []mount.Mount
	for _, m := range mounts {
		if filepath.Clean(m.Path) == path {
			out = append(out, m)
		}
	}
	return out
}

// latest returns the latest mount at path, the one a reader of path sees.
func latest(mounts []mount.Mount, path string) (mount.Mount, bool) {
	ms := at(mounts, path)
	if len(ms) == 0 {
		return mount.Mount{}, false
	}
	return ms[len(ms)-1], true
}

// sameDevice reports whether a and b name the same device, following
// symlinks such as /dev/disk/by-id/... where they resolve.
func sameDevice(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return resolve(a) == resolve(b)
}

func resolve(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	return filepath.Clean(path)
}

func orNothing(dev string) string {
	if dev == "" {
		return "a mount of no device"
	}
	return dev
}

// fsTypeOf is the filesystem a mount capability asks for: ext4 when it
// names none.
func fsTypeOf(m *csi.VolumeCapability_MountVolume) (string, error) {
	fs := m.GetFsType()
	if fs == "" {
		return defaultFsType, nil
	}
	if !fsTypes[fs] {
		return "", status.Errorf(codes.InvalidArgument, "filesystem %q isn't supported: ext4 or xfs", fs)
	}
	return fs, nil
}

// volumeLocks keeps two calls from changing one volume at once. The CO
// shouldn't send them; one that does gets Aborted and retries.
type volumeLocks struct {
	mu   sync.Mutex
	held map[string]bool
}

func (l *volumeLocks) lock(volumeID string) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[volumeID] {
		return nil, status.Errorf(codes.Aborted, "another call is changing volume %s", volumeID)
	}
	l.held[volumeID] = true
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.held, volumeID)
	}, nil
}
