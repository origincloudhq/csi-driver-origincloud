package fake

import (
	"context"
	"iter"
	"slices"
	"strings"

	"github.com/origincloudhq/csi-driver-origincloud/internal/cloud"
	pvsv1beta1 "go.origincloud.eu/sdk/pvs/v1beta1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// PutSnapshot stores s as it is, replacing any snapshot of its name.
func (c *Cloud) PutSnapshot(s *pvsv1beta1.Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshots[s.GetName()] = proto.Clone(s).(*pvsv1beta1.Snapshot)
}

// Snapshot returns the snapshot of name, or nil.
func (c *Cloud) Snapshot(name string) *pvsv1beta1.Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.snapshots[name]; ok {
		return proto.Clone(s).(*pvsv1beta1.Snapshot)
	}
	return nil
}

// Snapshots returns every snapshot, deleted ones included.
func (c *Cloud) Snapshots() []*pvsv1beta1.Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*pvsv1beta1.Snapshot
	for _, s := range c.snapshots {
		out = append(out, proto.Clone(s).(*pvsv1beta1.Snapshot))
	}
	return out
}

// checkSource refuses a volume to create in parent from a snapshot that
// isn't a source: one elsewhere, gone, not completed and protected, or
// larger than the volume. The caller holds mu.
func (c *Cloud) checkSource(parent string, vol *pvsv1beta1.Volume) error {
	source := vol.GetSourceSnapshot()
	if source == "" {
		return nil
	}
	relative, ok := strings.CutPrefix(source, pvsFullName(""))
	if !ok || !strings.HasPrefix(relative, parent+"/snapshots/") {
		return Error("BAD_REQUEST", "invalid input on field volume.sourceSnapshot: must be in the volume's project and zone")
	}
	snap, ok := c.snapshots[relative]
	if !ok || snap.State == pvsv1beta1.Snapshot_DELETED {
		return notFound(source)
	}
	if snap.State != pvsv1beta1.Snapshot_COMPLETED || snap.ProtectionState != pvsv1beta1.Snapshot_PROTECTED {
		return Error("CONFLICT", "snapshot %s is %s and %s: a volume is created only from a completed, protected snapshot", relative, snap.State, snap.ProtectionState)
	}
	if vol.GetSizeGib() < snap.SourceSizeGib {
		return Error("BAD_REQUEST", "invalid input on field volume.sizeGib: at least the snapshot's %d GiB", snap.SourceSizeGib)
	}
	return nil
}

// UpdateVolume grows a volume: the only change the driver makes. The size
// it has already changes nothing; a smaller one is refused.
func (v volumes) UpdateVolume(_ context.Context, req *pvsv1beta1.UpdateVolumeRequest) (cloud.Operation[*pvsv1beta1.Volume], error) {
	c := v.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if prior, ok := c.requests[req.GetRequestId()]; ok && req.GetRequestId() != "" {
		return prior.(*op[*pvsv1beta1.Volume]), nil
	}
	if err := c.change("UpdateVolume"); err != nil {
		return nil, err
	}
	if !slices.Equal(req.GetUpdateMask().GetPaths(), []string{"size_gib"}) {
		return nil, Error("BAD_REQUEST", "invalid input on field updateMask: the fake changes only size_gib")
	}
	name := req.GetVolume().GetName()
	vol, ok := c.volumes[name]
	if !ok || vol.State == pvsv1beta1.Volume_DELETED {
		return nil, notFound(name)
	}
	size := req.GetVolume().GetSizeGib()
	switch {
	case vol.State != pvsv1beta1.Volume_AVAILABLE && vol.State != pvsv1beta1.Volume_IN_USE:
		return nil, Error("CONFLICT", "volume %s is %s", name, vol.State)
	case vol.ResizeState == pvsv1beta1.Volume_RESIZING:
		return nil, Error("CONFLICT", "volume %s is being resized", name)
	case size < vol.SizeGib:
		return nil, Error("BAD_REQUEST", "invalid input on field volume.sizeGib: a volume never shrinks, it has %d GiB", vol.SizeGib)
	}
	o := &op[*pvsv1beta1.Volume]{c: c, kind: "UpdateVolume", result: proto.Clone(vol).(*pvsv1beta1.Volume)}
	if size > vol.SizeGib {
		vol.ResizeState, vol.PendingSizeGib = pvsv1beta1.Volume_RESIZING, size
		o.result.SizeGib, o.result.ResizeState = size, pvsv1beta1.Volume_RESIZE_COMPLETED
		o.done = func() {
			if cur, ok := c.volumes[name]; ok && cur.ResizeState == pvsv1beta1.Volume_RESIZING {
				cur.SizeGib, cur.PendingSizeGib, cur.ResizeState = size, 0, pvsv1beta1.Volume_RESIZE_COMPLETED
			}
		}
	}
	if req.GetRequestId() != "" {
		c.requests[req.GetRequestId()] = o
	}
	return o, nil
}

type snapshots struct{ c *Cloud }

func (s snapshots) GetSnapshot(_ context.Context, req *pvsv1beta1.GetSnapshotRequest) (*pvsv1beta1.Snapshot, error) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	snap, ok := s.c.snapshots[req.GetName()]
	if !ok {
		return nil, notFound(req.GetName())
	}
	return proto.Clone(snap).(*pvsv1beta1.Snapshot), nil
}

// AllSnapshots lists a zone's snapshots oldest first, as the API does: the
// fake's ids grow with every resource it creates. Deleted ones aren't
// listed.
func (s snapshots) AllSnapshots(_ context.Context, req *pvsv1beta1.ListSnapshotsRequest) iter.Seq2[*pvsv1beta1.Snapshot, error] {
	s.c.mu.Lock()
	var page []*pvsv1beta1.Snapshot
	for name, snap := range s.c.snapshots {
		if strings.HasPrefix(name, req.GetParent()+"/snapshots/") && snap.GetState() != pvsv1beta1.Snapshot_DELETED {
			page = append(page, proto.Clone(snap).(*pvsv1beta1.Snapshot))
		}
	}
	s.c.mu.Unlock()
	slices.SortFunc(page, func(a, b *pvsv1beta1.Snapshot) int { return strings.Compare(a.GetName(), b.GetName()) })
	return func(yield func(*pvsv1beta1.Snapshot, error) bool) {
		for _, snap := range page {
			if !yield(snap, nil) {
				return
			}
		}
	}
}

func (s snapshots) CreateSnapshot(_ context.Context, req *pvsv1beta1.CreateSnapshotRequest) (cloud.Operation[*pvsv1beta1.Snapshot], error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if prior, ok := c.requests[req.GetRequestId()]; ok && req.GetRequestId() != "" {
		return prior.(*op[*pvsv1beta1.Snapshot]), nil
	}
	if err := c.change("CreateSnapshot"); err != nil {
		return nil, err
	}
	source := req.GetSnapshot().GetSourceVolume()
	relative, ok := strings.CutPrefix(source, pvsFullName(""))
	if !ok || !strings.HasPrefix(relative, req.GetParent()+"/volumes/") {
		return nil, Error("BAD_REQUEST", "invalid input on field snapshot.sourceVolume: must be a volume in %s", req.GetParent())
	}
	vol, ok := c.volumes[relative]
	if !ok || vol.State == pvsv1beta1.Volume_DELETED {
		return nil, notFound(source)
	}
	snap := &pvsv1beta1.Snapshot{
		Name:            req.GetParent() + "/snapshots/" + c.id("snap"),
		SourceVolume:    source,
		SourceSizeGib:   vol.SizeGib,
		State:           pvsv1beta1.Snapshot_PENDING,
		ProtectionState: pvsv1beta1.Snapshot_UNPROTECTED,
		Tags:            req.GetSnapshot().GetTags(),
	}
	c.snapshots[snap.Name] = snap
	ready := proto.Clone(snap).(*pvsv1beta1.Snapshot)
	ready.State = pvsv1beta1.Snapshot_COMPLETED
	o := &op[*pvsv1beta1.Snapshot]{c: c, kind: "CreateSnapshot", result: ready, done: func() {
		if cur, ok := c.snapshots[ready.Name]; ok && cur.State == pvsv1beta1.Snapshot_PENDING {
			cur.State = pvsv1beta1.Snapshot_COMPLETED
		}
	}}
	if req.GetRequestId() != "" {
		c.requests[req.GetRequestId()] = o
	}
	return o, nil
}

func (s snapshots) DeleteSnapshot(_ context.Context, req *pvsv1beta1.DeleteSnapshotRequest) (cloud.Operation[*emptypb.Empty], error) {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.change("DeleteSnapshot"); err != nil {
		return nil, err
	}
	snap, ok := c.snapshots[req.GetName()]
	if !ok || snap.State == pvsv1beta1.Snapshot_DELETED {
		return nil, notFound(req.GetName())
	}
	settled := snap.State == pvsv1beta1.Snapshot_COMPLETED || snap.State == pvsv1beta1.Snapshot_ERROR
	if !settled || snap.ProtectionState != pvsv1beta1.Snapshot_UNPROTECTED {
		return nil, Error("CONFLICT", "snapshot %s is %s and %s: only an unprotected, completed or failed snapshot is deleted", req.GetName(), snap.State, snap.ProtectionState)
	}
	snap.State = pvsv1beta1.Snapshot_DELETING
	name := snap.Name
	return &op[*emptypb.Empty]{c: c, kind: "DeleteSnapshot", result: &emptypb.Empty{}, done: func() {
		if cur, ok := c.snapshots[name]; ok {
			cur.State = pvsv1beta1.Snapshot_DELETED
		}
	}}, nil
}

func (s snapshots) ProtectSnapshot(_ context.Context, req *pvsv1beta1.ProtectSnapshotRequest) (cloud.Operation[*pvsv1beta1.Snapshot], error) {
	return s.c.setProtection("ProtectSnapshot", req.GetName(), req.GetRequestId(), pvsv1beta1.Snapshot_PROTECTING, pvsv1beta1.Snapshot_PROTECTED)
}

func (s snapshots) UnprotectSnapshot(_ context.Context, req *pvsv1beta1.UnprotectSnapshotRequest) (cloud.Operation[*pvsv1beta1.Snapshot], error) {
	return s.c.setProtection("UnprotectSnapshot", req.GetName(), req.GetRequestId(), pvsv1beta1.Snapshot_UNPROTECTING, pvsv1beta1.Snapshot_UNPROTECTED)
}

// setProtection protects or unprotects a completed snapshot: it is in the
// state changing until the operation is done, then in the state to. An
// unprotect is refused while a volume not deleted comes from the snapshot.
func (c *Cloud) setProtection(kind, name, requestID string, changing, to pvsv1beta1.Snapshot_ProtectionState) (cloud.Operation[*pvsv1beta1.Snapshot], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if prior, ok := c.requests[requestID]; ok && requestID != "" {
		return prior.(*op[*pvsv1beta1.Snapshot]), nil
	}
	if err := c.change(kind); err != nil {
		return nil, err
	}
	snap, ok := c.snapshots[name]
	if !ok || snap.State == pvsv1beta1.Snapshot_DELETED {
		return nil, notFound(name)
	}
	if snap.State != pvsv1beta1.Snapshot_COMPLETED {
		return nil, Error("CONFLICT", "snapshot %s is %s", name, snap.State)
	}
	if to == pvsv1beta1.Snapshot_UNPROTECTED {
		for _, vol := range c.volumes {
			if vol.SourceSnapshot == pvsFullName(name) && vol.State != pvsv1beta1.Volume_DELETED {
				return nil, Error("CONFLICT", "snapshot %s is the source of volume %s; delete it first", name, vol.Name)
			}
		}
	}
	snap.ProtectionState = changing
	ready := proto.Clone(snap).(*pvsv1beta1.Snapshot)
	ready.ProtectionState = to
	o := &op[*pvsv1beta1.Snapshot]{c: c, kind: kind, result: ready, done: func() {
		if cur, ok := c.snapshots[name]; ok && cur.ProtectionState == changing {
			cur.ProtectionState = to
		}
	}}
	if requestID != "" {
		c.requests[requestID] = o
	}
	return o, nil
}
