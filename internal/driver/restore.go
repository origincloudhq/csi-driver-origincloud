package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	pvsv1beta1 "go.origincloud.eu/sdk/pvs/v1beta1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// restoreSource returns the snapshot a volume is restored from, once it is
// a source (completed and protected), and the zone to create the volume
// in: the snapshot's, since PVS creates a volume from a snapshot only in
// the snapshot's zone. It fails with ResourceExhausted when the CO's
// requisite topology leaves that zone out.
func (d *Driver) restoreSource(ctx context.Context, id string, req *csi.TopologyRequirement) (*pvsv1beta1.Snapshot, string, error) {
	if id == "" {
		return nil, "", status.Error(codes.InvalidArgument, "the content source's snapshot ID is missing")
	}
	sz, ok := parseSnapshotID(id)
	if !ok {
		return nil, "", status.Errorf(codes.NotFound, "snapshot %q not found", id)
	}
	if sz.Project != d.cfg.Project {
		return nil, "", status.Errorf(codes.InvalidArgument, "snapshot %s is in project %s; the driver creates volumes in %s, and a volume is restored only in its snapshot's project", id, sz.Project, d.cfg.Project)
	}
	if !allows(req.GetRequisite(), sz.Zone) {
		return nil, "", status.Errorf(codes.ResourceExhausted, "snapshot %s is in zone %s, which the requisite topology leaves out: a volume is restored only in its snapshot's zone", id, sz.Zone)
	}
	var snap *pvsv1beta1.Snapshot
	err := d.poll(ctx, func() (bool, error) {
		var err error
		snap, err = d.cfg.Cloud.Snapshots.GetSnapshot(ctx, &pvsv1beta1.GetSnapshotRequest{Name: id})
		if err != nil {
			return false, apiStatus(err, "reading snapshot "+id)
		}
		switch snap.GetState() {
		case pvsv1beta1.Snapshot_DELETING, pvsv1beta1.Snapshot_DELETED:
			return false, status.Errorf(codes.NotFound, "snapshot %s is %s", id, snap.GetState())
		case pvsv1beta1.Snapshot_ERROR:
			return false, status.Errorf(codes.FailedPrecondition, "snapshot %s failed, there is no copy to restore: %s", id, snap.GetStateReason())
		case pvsv1beta1.Snapshot_COMPLETED:
		default:
			return false, nil
		}
		switch snap.GetProtectionState() {
		case pvsv1beta1.Snapshot_PROTECTED:
			return true, nil
		case pvsv1beta1.Snapshot_PROTECTING:
			return false, nil
		}
		return false, status.Errorf(codes.FailedPrecondition, "snapshot %s is %s: a volume is restored only from a protected snapshot; the driver protects the snapshots it takes, protect this one first", id, snap.GetProtectionState())
	})
	if err != nil {
		return nil, "", err
	}
	return snap, sz.Zone, nil
}

// allows reports whether a requisite topology includes the zone. Without
// any topology naming a zone, every zone is allowed.
func allows(requisite []*csi.Topology, zone string) bool {
	named := false
	for _, t := range requisite {
		z, ok := t.GetSegments()[TopologyZoneKey]
		if !ok {
			continue
		}
		if z == zone {
			return true
		}
		named = true
	}
	return !named
}

// restoreSize is the size of a volume restored from snap: the size asked
// for, or the snapshot's when that is larger. OutOfRange when the limit
// leaves no room for the snapshot.
func restoreSize(asked int32, snap *pvsv1beta1.Snapshot, r *csi.CapacityRange) (int32, error) {
	least := snap.GetSourceSizeGib()
	if asked >= least {
		return asked, nil
	}
	if limit := r.GetLimitBytes(); limit > 0 && int64(least)*gib > limit {
		return 0, status.Errorf(codes.OutOfRange, "snapshot %s holds %d GiB, more than the limit of %d bytes", snap.GetName(), least, limit)
	}
	return least, nil
}
