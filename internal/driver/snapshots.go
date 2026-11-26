package driver

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/google/uuid"
	pvsv1beta1 "go.origincloud.eu/sdk/pvs/v1beta1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// A snapshot the driver takes is protected as soon as it is copied: PVS
// creates a volume only from a protected snapshot. It is ready to use
// once it is both. PVS doesn't flatten a restored volume, so a snapshot
// stays protected while a volume restored from it exists, and deleting it
// fails with FailedPrecondition until those volumes are deleted.

// CreateSnapshot takes a PVS snapshot of a volume and protects it, or
// returns the one an earlier attempt of the same name took. It is
// idempotent by the CSI snapshot name as CreateVolume is by the volume
// name: through a tag and a request id derived from the name.
func (d *Driver) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	name := req.GetName()
	if err := checkName("snapshot", name); err != nil {
		return nil, err
	}
	sourceID := req.GetSourceVolumeId()
	if sourceID == "" {
		return nil, status.Error(codes.InvalidArgument, "the source volume ID is missing")
	}
	if err := checkSnapshotParameters(req.GetParameters()); err != nil {
		return nil, err
	}
	vz, ok := parseVolumeID(sourceID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "volume %q not found", sourceID)
	}
	parent := vz.Parent()

	snap, err := d.findSnapshot(ctx, parent, name)
	if err != nil {
		return nil, err
	}
	if snap != nil {
		if snap.GetSourceVolume() != pvsFullName(sourceID) {
			return nil, status.Errorf(codes.AlreadyExists, "snapshot %q exists as %s of %s, not of %s", name, snap.GetName(), relativeOf(snap.GetSourceVolume()), sourceID)
		}
	} else {
		if _, err := d.cfg.Cloud.Volumes.GetVolume(ctx, &pvsv1beta1.GetVolumeRequest{Name: sourceID}); err != nil {
			return nil, apiStatus(err, "reading volume "+sourceID)
		}
		tags := map[string]string{
			tagSnapshotName: name,
			tagCreatedBy:    Name,
			tagCreatedAt:    time.Now().UTC().Format(time.RFC3339),
		}
		if len(name) <= 128 {
			tags["origin:name"] = name
		}
		if snap, err = d.createSnapshot(ctx, parent, name, &pvsv1beta1.Snapshot{
			SourceVolume: pvsFullName(sourceID),
			Tags:         tags,
		}); err != nil {
			return nil, err
		}
	}
	if snap, err = d.settleSnapshot(ctx, snap); err != nil {
		return nil, err
	}
	return &csi.CreateSnapshotResponse{Snapshot: csiSnapshot(snap)}, nil
}

// createSnapshot takes the snapshot spec for the CSI name in parent and
// waits for the operation. As in createVolume, a request id whose
// operation returns a snapshot deleted since is replaced by one derived
// from the gone snapshot's name too.
func (d *Driver) createSnapshot(ctx context.Context, parent, name string, spec *pvsv1beta1.Snapshot) (*pvsv1beta1.Snapshot, error) {
	requestID := snapshotRequestID(parent, name)
	for range 8 {
		op, err := d.cfg.Cloud.Snapshots.CreateSnapshot(ctx, &pvsv1beta1.CreateSnapshotRequest{
			Parent:    parent,
			Snapshot:  spec,
			RequestId: requestID,
		})
		if err != nil {
			return nil, apiStatus(err, "taking snapshot "+name)
		}
		snap, err := op.Wait(ctx)
		if err != nil {
			return nil, apiStatus(err, "waiting for snapshot "+name+" to be taken")
		}
		current, err := d.cfg.Cloud.Snapshots.GetSnapshot(ctx, &pvsv1beta1.GetSnapshotRequest{Name: snap.GetName()})
		switch {
		case isNotFound(err):
		case err != nil:
			return nil, apiStatus(err, "reading snapshot "+snap.GetName())
		case current.GetState() != pvsv1beta1.Snapshot_DELETING && current.GetState() != pvsv1beta1.Snapshot_DELETED:
			return current, nil
		}
		requestID = snapshotRequestID(parent, name+"\x00"+snap.GetName())
	}
	return nil, status.Errorf(codes.Internal, "taking snapshot %s kept returning snapshots that were deleted", name)
}

// findSnapshot returns the snapshot in parent that carries the CSI name,
// or nil. A snapshot on its way out doesn't count.
func (d *Driver) findSnapshot(ctx context.Context, parent, name string) (*pvsv1beta1.Snapshot, error) {
	for snap, err := range d.cfg.Cloud.Snapshots.AllSnapshots(ctx, &pvsv1beta1.ListSnapshotsRequest{Parent: parent}) {
		if err != nil {
			return nil, apiStatus(err, "listing the snapshots of "+parent)
		}
		if snap.GetTags()[tagSnapshotName] != name {
			continue
		}
		switch snap.GetState() {
		case pvsv1beta1.Snapshot_DELETING, pvsv1beta1.Snapshot_DELETED:
			continue
		}
		return snap, nil
	}
	return nil, nil
}

// settleSnapshot waits for a snapshot to be copied, protects it, and
// waits for that. It fails for a snapshot that ended in ERROR, and with
// Aborted for one deleted meanwhile, which a retry takes again.
func (d *Driver) settleSnapshot(ctx context.Context, snap *pvsv1beta1.Snapshot) (*pvsv1beta1.Snapshot, error) {
	err := d.poll(ctx, func() (bool, error) {
		switch snap.GetState() {
		case pvsv1beta1.Snapshot_ERROR:
			return false, status.Errorf(codes.Internal, "snapshot %s failed: %s", snap.GetName(), snap.GetStateReason())
		case pvsv1beta1.Snapshot_DELETING, pvsv1beta1.Snapshot_DELETED:
			return false, status.Errorf(codes.Aborted, "snapshot %s is being deleted", snap.GetName())
		case pvsv1beta1.Snapshot_COMPLETED:
			switch snap.GetProtectionState() {
			case pvsv1beta1.Snapshot_PROTECTED:
				return true, nil
			case pvsv1beta1.Snapshot_PROTECTING, pvsv1beta1.Snapshot_UNPROTECTING:
			default:
				op, err := d.cfg.Cloud.Snapshots.ProtectSnapshot(ctx, &pvsv1beta1.ProtectSnapshotRequest{
					Name: snap.GetName(),
					// A fresh id each time: a protect that failed is
					// tried again. A retry finds the first attempt's
					// protect under way and waits for it.
					RequestId: uuid.NewString(),
				})
				if err != nil {
					return false, apiStatus(err, "protecting snapshot "+snap.GetName())
				}
				if _, err := op.Wait(ctx); err != nil {
					return false, apiStatus(err, "waiting for snapshot "+snap.GetName()+" to be protected")
				}
			}
		}
		next, err := d.cfg.Cloud.Snapshots.GetSnapshot(ctx, &pvsv1beta1.GetSnapshotRequest{Name: snap.GetName()})
		if err != nil {
			return false, apiStatus(err, "reading snapshot "+snap.GetName())
		}
		snap = next
		return false, nil
	})
	return snap, err
}

// DeleteSnapshot unprotects and deletes a snapshot. A snapshot that
// doesn't exist, or whose ID this driver never issued, is deleted
// already. PVS refuses to unprotect a snapshot a volume was restored from,
// which fails with FailedPrecondition until that volume is deleted.
func (d *Driver) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	id := req.GetSnapshotId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "the snapshot ID is missing")
	}
	if _, ok := parseSnapshotID(id); !ok {
		return &csi.DeleteSnapshotResponse{}, nil
	}
	gone := false
	err := d.poll(ctx, func() (bool, error) {
		snap, err := d.cfg.Cloud.Snapshots.GetSnapshot(ctx, &pvsv1beta1.GetSnapshotRequest{Name: id})
		if isNotFound(err) {
			gone = true
			return true, nil
		}
		if err != nil {
			return false, apiStatus(err, "reading snapshot "+id)
		}
		switch snap.GetState() {
		case pvsv1beta1.Snapshot_DELETED:
			gone = true
			return true, nil
		case pvsv1beta1.Snapshot_PENDING, pvsv1beta1.Snapshot_DELETING, pvsv1beta1.Snapshot_STATE_UNSPECIFIED:
			// Being copied or deleted: wait for it to settle.
			return false, nil
		}
		switch snap.GetProtectionState() {
		case pvsv1beta1.Snapshot_PROTECTING, pvsv1beta1.Snapshot_UNPROTECTING:
			return false, nil
		case pvsv1beta1.Snapshot_PROTECTED, pvsv1beta1.Snapshot_PROTECTION_FAILED:
			op, err := d.cfg.Cloud.Snapshots.UnprotectSnapshot(ctx, &pvsv1beta1.UnprotectSnapshotRequest{Name: id, RequestId: uuid.NewString()})
			if isNotFound(err) {
				gone = true
				return true, nil
			}
			if err != nil {
				return false, apiStatus(err, "unprotecting snapshot "+id)
			}
			if _, err := op.Wait(ctx); err != nil {
				return false, apiStatus(err, "waiting for snapshot "+id+" to be unprotected")
			}
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if gone {
		return &csi.DeleteSnapshotResponse{}, nil
	}
	op, err := d.cfg.Cloud.Snapshots.DeleteSnapshot(ctx, &pvsv1beta1.DeleteSnapshotRequest{Name: id, RequestId: uuid.NewString()})
	if isNotFound(err) {
		return &csi.DeleteSnapshotResponse{}, nil
	}
	if err != nil {
		return nil, apiStatus(err, "deleting snapshot "+id)
	}
	if _, err := op.Wait(ctx); err != nil && !isNotFound(err) {
		return nil, apiStatus(err, "waiting for snapshot "+id+" to be deleted")
	}
	return &csi.DeleteSnapshotResponse{}, nil
}

// ListSnapshots lists snapshots, oldest first: the one the snapshot ID
// names, those of the source volume, or, with neither, those in the
// controller's default zone. Listing every zone would need the cluster's
// set of zones, which the controller doesn't know. The starting token is
// the position of the first entry to return.
func (d *Driver) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	if req.GetMaxEntries() < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "max entries is negative: %d", req.GetMaxEntries())
	}
	snaps, err := d.listSnapshots(ctx, req.GetSnapshotId(), req.GetSourceVolumeId())
	if err != nil {
		return nil, err
	}
	start := 0
	if token := req.GetStartingToken(); token != "" {
		start, err = strconv.Atoi(token)
		if err != nil || start < 0 || start > len(snaps) {
			return nil, status.Errorf(codes.Aborted, "starting token %q names no position in the list", token)
		}
	}
	end := len(snaps)
	if n := int(req.GetMaxEntries()); n > 0 && start+n < end {
		end = start + n
	}
	resp := &csi.ListSnapshotsResponse{}
	for _, snap := range snaps[start:end] {
		resp.Entries = append(resp.Entries, &csi.ListSnapshotsResponse_Entry{Snapshot: csiSnapshot(snap)})
	}
	if end < len(snaps) {
		resp.NextToken = strconv.Itoa(end)
	}
	return resp, nil
}

// listSnapshots returns the snapshots ListSnapshots filters for. An ID of
// a form the driver never issues matches nothing.
func (d *Driver) listSnapshots(ctx context.Context, snapshotID, sourceID string) ([]*pvsv1beta1.Snapshot, error) {
	ofSource := func(s *pvsv1beta1.Snapshot) bool {
		return sourceID == "" || s.GetSourceVolume() == pvsFullName(sourceID)
	}
	var parent string
	switch {
	case snapshotID != "":
		if _, ok := parseSnapshotID(snapshotID); !ok {
			return nil, nil
		}
		snap, err := d.cfg.Cloud.Snapshots.GetSnapshot(ctx, &pvsv1beta1.GetSnapshotRequest{Name: snapshotID})
		if isNotFound(err) {
			return nil, nil
		}
		if err != nil {
			return nil, apiStatus(err, "reading snapshot "+snapshotID)
		}
		if snap.GetState() == pvsv1beta1.Snapshot_DELETED || !ofSource(snap) {
			return nil, nil
		}
		return []*pvsv1beta1.Snapshot{snap}, nil
	case sourceID != "":
		vz, ok := parseVolumeID(sourceID)
		if !ok {
			return nil, nil
		}
		parent = vz.Parent()
	case d.cfg.DefaultZone != "":
		parent = zonal{Project: d.cfg.Project, Zone: d.cfg.DefaultZone}.Parent()
	default:
		return nil, status.Error(codes.FailedPrecondition, "listing snapshots without a snapshot or source volume lists the controller's default zone, and it has none")
	}
	var out []*pvsv1beta1.Snapshot
	for snap, err := range d.cfg.Cloud.Snapshots.AllSnapshots(ctx, &pvsv1beta1.ListSnapshotsRequest{Parent: parent}) {
		if err != nil {
			return nil, apiStatus(err, "listing the snapshots of "+parent)
		}
		if ofSource(snap) {
			out = append(out, snap)
		}
	}
	return out, nil
}

// csiSnapshot is the CSI view of a PVS snapshot. Its creation time is the
// one the driver recorded when it asked for the snapshot; a snapshot it
// didn't take has none.
func csiSnapshot(s *pvsv1beta1.Snapshot) *csi.Snapshot {
	out := &csi.Snapshot{
		SizeBytes:      int64(s.GetSourceSizeGib()) * gib,
		SnapshotId:     s.GetName(),
		SourceVolumeId: relativeOf(s.GetSourceVolume()),
		ReadyToUse:     s.GetState() == pvsv1beta1.Snapshot_COMPLETED && s.GetProtectionState() == pvsv1beta1.Snapshot_PROTECTED,
	}
	if t, err := time.Parse(time.RFC3339, s.GetTags()[tagCreatedAt]); err == nil {
		out.CreationTime = timestamppb.New(t)
	}
	return out
}

// checkSnapshotParameters refuses every VolumeSnapshotClass parameter but
// Kubernetes' own `csi.storage.k8s.io/` ones, which the sidecar passes
// along: a snapshot takes no options.
func checkSnapshotParameters(params map[string]string) error {
	for k := range params {
		if !strings.HasPrefix(k, "csi.storage.k8s.io/") {
			return status.Errorf(codes.InvalidArgument, "unknown snapshot parameter %q; the driver takes none", k)
		}
	}
	return nil
}
