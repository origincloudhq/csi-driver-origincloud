package driver

import (
	"context"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/origincloudhq/csi-driver-origincloud/internal/cloud/fake"
	pvsv1beta1 "go.origincloud.eu/sdk/pvs/v1beta1"
	"google.golang.org/grpc/codes"
)

const zone2 = "eu-central1-az2"

var errUnavailable = fake.Error("SERVICE_UNAVAILABLE", "try again later")

func mustSnapshot(t *testing.T, d *Driver, name, volume string) *csi.Snapshot {
	t.Helper()
	resp, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: name, SourceVolumeId: volume})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Snapshot
}

func restoreReq(name, snapshot string, topology ...string) *csi.CreateVolumeRequest {
	req := createReq(name)
	req.CapacityRange = nil
	req.AccessibilityRequirements = nil
	if len(topology) > 0 {
		req.AccessibilityRequirements = &csi.TopologyRequirement{}
		for _, z := range topology {
			t := &csi.Topology{Segments: map[string]string{TopologyZoneKey: z}}
			req.AccessibilityRequirements.Requisite = append(req.AccessibilityRequirements.Requisite, t)
			req.AccessibilityRequirements.Preferred = append(req.AccessibilityRequirements.Preferred, t)
		}
	}
	req.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
		Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: snapshot},
	}}
	return req
}

func TestCreateSnapshotProtectsItAndIsIdempotentByName(t *testing.T) {
	d, f := newDriver(t)
	vol := mustCreate(t, d, "pvc-1")
	first := mustSnapshot(t, d, "snap-1", vol)
	again := mustSnapshot(t, d, "snap-1", vol)
	if first.SnapshotId != again.SnapshotId {
		t.Fatalf("a retry took a second snapshot: %s, then %s", first.SnapshotId, again.SnapshotId)
	}
	if n := f.Calls["CreateSnapshot"]; n != 1 {
		t.Fatalf("CreateSnapshot called %d times, want 1", n)
	}
	if n := f.Calls["ProtectSnapshot"]; n != 1 {
		t.Fatalf("ProtectSnapshot called %d times, want 1", n)
	}
	snap := f.Snapshot(first.SnapshotId)
	if snap.GetProtectionState() != pvsv1beta1.Snapshot_PROTECTED || snap.GetTags()[tagSnapshotName] != "snap-1" {
		t.Fatalf("snapshot %v", snap)
	}
	if !first.ReadyToUse || first.SourceVolumeId != vol || first.SizeBytes != 5*gib || first.CreationTime == nil {
		t.Fatalf("CSI snapshot %v", first)
	}
	if !first.CreationTime.AsTime().Equal(again.CreationTime.AsTime()) {
		t.Fatalf("the creation time moved: %v, then %v", first.CreationTime, again.CreationTime)
	}
}

func TestCreateSnapshotRefusesAnotherSource(t *testing.T) {
	d, _ := newDriver(t)
	mustSnapshot(t, d, "snap-1", mustCreate(t, d, "pvc-1"))
	_, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: mustCreate(t, d, "pvc-2")})
	wantCode(t, err, codes.AlreadyExists)
}

func TestCreateSnapshotOfAMissingVolume(t *testing.T) {
	d, _ := newDriver(t)
	for _, id := range []string{parent + "/volumes/vol-gone", "not-a-volume"} {
		_, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: id})
		wantCode(t, err, codes.NotFound)
	}
	_, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name: "snap-1", SourceVolumeId: parent + "/volumes/vol-gone", Parameters: map[string]string{"speed": "fast"},
	})
	wantCode(t, err, codes.InvalidArgument)
}

func TestCreateSnapshotRetriesAFailedProtect(t *testing.T) {
	d, f := newDriver(t)
	vol := mustCreate(t, d, "pvc-1")
	f.Fail = func(kind string) error {
		if kind == "ProtectSnapshot" {
			return errUnavailable
		}
		return nil
	}
	_, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: vol})
	wantCode(t, err, codes.Unavailable)
	f.Fail = nil
	snap := mustSnapshot(t, d, "snap-1", vol)
	if !snap.ReadyToUse || f.Calls["CreateSnapshot"] != 1 {
		t.Fatalf("snapshot %v after %d creates", snap, f.Calls["CreateSnapshot"])
	}
}

func TestRestoreCreatesTheVolumeInTheSnapshotsZone(t *testing.T) {
	d, f := newDriver(t)
	// The snapshot is in zone2, the controller's default zone is unset,
	// and the CO prefers zone2 and allows both.
	f.PutVolume(&pvsv1beta1.Volume{Name: "projects/p1/zones/" + zone2 + "/volumes/vol-src", SizeGib: 8, State: pvsv1beta1.Volume_AVAILABLE})
	snap := mustSnapshot(t, d, "snap-1", "projects/p1/zones/"+zone2+"/volumes/vol-src")

	resp, err := d.CreateVolume(context.Background(), restoreReq("pvc-restored", snap.SnapshotId, zone, zone2))
	if err != nil {
		t.Fatal(err)
	}
	z, _ := parseVolumeID(resp.Volume.VolumeId)
	if z.Zone != zone2 || resp.Volume.AccessibleTopology[0].Segments[TopologyZoneKey] != zone2 {
		t.Fatalf("restored into %s (topology %v), want %s", z.Zone, resp.Volume.AccessibleTopology, zone2)
	}
	vol := f.Volume(resp.Volume.VolumeId)
	if vol.GetSourceSnapshot() != pvsFullName(snap.SnapshotId) || vol.GetSizeGib() != 8 {
		t.Fatalf("volume %v", vol)
	}
	if resp.Volume.ContentSource.GetSnapshot().GetSnapshotId() != snap.SnapshotId || resp.Volume.CapacityBytes != 8*gib {
		t.Fatalf("response %v", resp.Volume)
	}

	// A retry returns the same volume; the same name without the source
	// doesn't fit it.
	again, err := d.CreateVolume(context.Background(), restoreReq("pvc-restored", snap.SnapshotId, zone2))
	if err != nil || again.Volume.VolumeId != resp.Volume.VolumeId {
		t.Fatalf("retry: %v, %v", again, err)
	}
	plain := createReq("pvc-restored")
	plain.AccessibilityRequirements.Preferred[0].Segments[TopologyZoneKey] = zone2
	_, err = d.CreateVolume(context.Background(), plain)
	wantCode(t, err, codes.AlreadyExists)
}

func TestRestoreRefusesATopologyWithoutTheSnapshotsZone(t *testing.T) {
	d, _ := newDriver(t)
	snap := mustSnapshot(t, d, "snap-1", mustCreate(t, d, "pvc-1"))
	_, err := d.CreateVolume(context.Background(), restoreReq("pvc-2", snap.SnapshotId, zone2))
	wantCode(t, err, codes.ResourceExhausted)
}

func TestRestoreSize(t *testing.T) {
	d, _ := newDriver(t)
	snap := mustSnapshot(t, d, "snap-1", mustCreate(t, d, "pvc-1")) // 5 GiB

	larger := restoreReq("pvc-larger", snap.SnapshotId)
	larger.CapacityRange = &csi.CapacityRange{RequiredBytes: 9 * gib}
	resp, err := d.CreateVolume(context.Background(), larger)
	if err != nil || resp.Volume.CapacityBytes != 9*gib {
		t.Fatalf("larger: %v, %v", resp, err)
	}
	smaller := restoreReq("pvc-smaller", snap.SnapshotId)
	smaller.CapacityRange = &csi.CapacityRange{RequiredBytes: 1 * gib}
	if resp, err = d.CreateVolume(context.Background(), smaller); err != nil || resp.Volume.CapacityBytes != 5*gib {
		t.Fatalf("smaller: %v, %v", resp, err)
	}
	capped := restoreReq("pvc-capped", snap.SnapshotId)
	capped.CapacityRange = &csi.CapacityRange{RequiredBytes: 1 * gib, LimitBytes: 2 * gib}
	_, err = d.CreateVolume(context.Background(), capped)
	wantCode(t, err, codes.OutOfRange)
}

func TestRestoreRefusals(t *testing.T) {
	d, f := newDriver(t)
	vol := mustCreate(t, d, "pvc-1")
	ctx := context.Background()

	_, err := d.CreateVolume(ctx, restoreReq("pvc-2", parent+"/snapshots/snap-gone"))
	wantCode(t, err, codes.NotFound)
	_, err = d.CreateVolume(ctx, restoreReq("pvc-2", "projects/p2/zones/"+zone+"/snapshots/snap-1"))
	wantCode(t, err, codes.InvalidArgument)

	f.PutSnapshot(&pvsv1beta1.Snapshot{
		Name: parent + "/snapshots/snap-plain", SourceVolume: pvsFullName(vol), SourceSizeGib: 5,
		State: pvsv1beta1.Snapshot_COMPLETED, ProtectionState: pvsv1beta1.Snapshot_UNPROTECTED,
	})
	_, err = d.CreateVolume(ctx, restoreReq("pvc-2", parent+"/snapshots/snap-plain"))
	wantCode(t, err, codes.FailedPrecondition)

	clone := createReq("pvc-clone")
	clone.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
		Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: vol},
	}}
	_, err = d.CreateVolume(ctx, clone)
	wantCode(t, err, codes.InvalidArgument)
	if n := f.Calls["CreateVolume"]; n != 1 {
		t.Fatalf("CreateVolume called %d times, want only the source's", n)
	}
}

func TestDeleteSnapshotUnprotectsItAndIsIdempotent(t *testing.T) {
	d, f := newDriver(t)
	ctx := context.Background()
	snap := mustSnapshot(t, d, "snap-1", mustCreate(t, d, "pvc-1"))
	for range 2 {
		if _, err := d.DeleteSnapshot(ctx, &csi.DeleteSnapshotRequest{SnapshotId: snap.SnapshotId}); err != nil {
			t.Fatal(err)
		}
	}
	if s := f.Snapshot(snap.SnapshotId); s.GetState() != pvsv1beta1.Snapshot_DELETED {
		t.Fatalf("snapshot %v", s)
	}
	if f.Calls["UnprotectSnapshot"] != 1 || f.Calls["DeleteSnapshot"] != 1 {
		t.Fatalf("calls %v", f.Calls)
	}
	for _, id := range []string{"not-a-snapshot", parent + "/snapshots/snap-gone"} {
		if _, err := d.DeleteSnapshot(ctx, &csi.DeleteSnapshotRequest{SnapshotId: id}); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

func TestDeleteSnapshotWaitsForTheVolumesRestoredFromIt(t *testing.T) {
	d, f := newDriver(t)
	ctx := context.Background()
	snap := mustSnapshot(t, d, "snap-1", mustCreate(t, d, "pvc-1"))
	restored, err := d.CreateVolume(ctx, restoreReq("pvc-2", snap.SnapshotId))
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.DeleteSnapshot(ctx, &csi.DeleteSnapshotRequest{SnapshotId: snap.SnapshotId})
	wantCode(t, err, codes.FailedPrecondition)
	if s := f.Snapshot(snap.SnapshotId); s.GetProtectionState() != pvsv1beta1.Snapshot_PROTECTED {
		t.Fatalf("a refused delete changed the snapshot: %v", s)
	}
	if _, err := d.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: restored.Volume.VolumeId}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DeleteSnapshot(ctx, &csi.DeleteSnapshotRequest{SnapshotId: snap.SnapshotId}); err != nil {
		t.Fatal(err)
	}
}

func TestListSnapshots(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	v1, v2 := mustCreate(t, d, "pvc-1"), mustCreate(t, d, "pvc-2")
	s1, s2, s3 := mustSnapshot(t, d, "s1", v1), mustSnapshot(t, d, "s2", v2), mustSnapshot(t, d, "s3", v1)

	ids := func(resp *csi.ListSnapshotsResponse) []string {
		var out []string
		for _, e := range resp.GetEntries() {
			out = append(out, e.GetSnapshot().GetSnapshotId())
		}
		return out
	}
	list := func(req *csi.ListSnapshotsRequest) *csi.ListSnapshotsResponse {
		t.Helper()
		resp, err := d.ListSnapshots(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if got := ids(list(&csi.ListSnapshotsRequest{SourceVolumeId: v1})); len(got) != 2 || got[0] != s1.SnapshotId || got[1] != s3.SnapshotId {
		t.Fatalf("by source: %v", got)
	}
	if got := ids(list(&csi.ListSnapshotsRequest{SnapshotId: s2.SnapshotId})); len(got) != 1 || got[0] != s2.SnapshotId {
		t.Fatalf("by id: %v", got)
	}
	if got := ids(list(&csi.ListSnapshotsRequest{SnapshotId: s2.SnapshotId, SourceVolumeId: v1})); len(got) != 0 {
		t.Fatalf("by id of another source: %v", got)
	}
	if got := ids(list(&csi.ListSnapshotsRequest{SnapshotId: "nope"})); len(got) != 0 {
		t.Fatalf("by an id never issued: %v", got)
	}

	// Paging through the source's snapshots one at a time.
	page := list(&csi.ListSnapshotsRequest{SourceVolumeId: v1, MaxEntries: 1})
	next := list(&csi.ListSnapshotsRequest{SourceVolumeId: v1, MaxEntries: 1, StartingToken: page.NextToken})
	if ids(page)[0] != s1.SnapshotId || ids(next)[0] != s3.SnapshotId || next.NextToken != "" {
		t.Fatalf("pages %v (%q), %v (%q)", ids(page), page.NextToken, ids(next), next.NextToken)
	}
	_, err := d.ListSnapshots(ctx, &csi.ListSnapshotsRequest{SourceVolumeId: v1, StartingToken: "99"})
	wantCode(t, err, codes.Aborted)

	// Without a filter, the controller lists its default zone: this one
	// has none.
	_, err = d.ListSnapshots(ctx, &csi.ListSnapshotsRequest{})
	wantCode(t, err, codes.FailedPrecondition)
	d.cfg.DefaultZone = zone
	if got := ids(list(&csi.ListSnapshotsRequest{})); len(got) != 3 {
		t.Fatalf("unfiltered: %v", got)
	}
}

func expandReq(vol string, gibs int64, c *csi.VolumeCapability) *csi.ControllerExpandVolumeRequest {
	return &csi.ControllerExpandVolumeRequest{
		VolumeId:         vol,
		CapacityRange:    &csi.CapacityRange{RequiredBytes: gibs * gib},
		VolumeCapability: c,
	}
}

func TestExpandVolumeGrowsAndIsIdempotent(t *testing.T) {
	d, f := newDriver(t)
	ctx := context.Background()
	vol := mustCreate(t, d, "pvc-1") // 5 GiB
	mustPublish(t, d, vol, node)     // online: the volume is attached

	resp, err := d.ControllerExpandVolume(ctx, expandReq(vol, 8, mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.CapacityBytes != 8*gib || !resp.NodeExpansionRequired {
		t.Fatalf("response %v", resp)
	}
	if v := f.Volume(vol); v.GetSizeGib() != 8 {
		t.Fatalf("volume %v", v)
	}

	// Growing it to the size it has is a no-op, and calls nothing.
	resp, err = d.ControllerExpandVolume(ctx, expandReq(vol, 8, mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)))
	if err != nil || resp.CapacityBytes != 8*gib {
		t.Fatalf("again: %v, %v", resp, err)
	}
	if n := f.Calls["UpdateVolume"]; n != 1 {
		t.Fatalf("UpdateVolume called %d times, want 1", n)
	}

	// Shrinking is refused.
	_, err = d.ControllerExpandVolume(ctx, expandReq(vol, 6, nil))
	wantCode(t, err, codes.OutOfRange)
	if v := f.Volume(vol); v.GetSizeGib() != 8 {
		t.Fatalf("a refused shrink changed the volume: %v", v)
	}
}

func TestExpandVolumeOfABlockVolumeNeedsNoNode(t *testing.T) {
	d, _ := newDriver(t)
	block := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
	resp, err := d.ControllerExpandVolume(context.Background(), expandReq(mustCreate(t, d, "pvc-1"), 6, block))
	if err != nil || resp.NodeExpansionRequired {
		t.Fatalf("%v, %v", resp, err)
	}
}

func TestExpandVolumeWaitsForAResizeUnderWay(t *testing.T) {
	d, f := newDriver(t)
	vol := mustCreate(t, d, "pvc-1")
	v := f.Volume(vol)
	v.SizeGib, v.PendingSizeGib, v.ResizeState = 5, 7, pvsv1beta1.Volume_RESIZING
	f.PutVolume(v)
	time.AfterFunc(20*time.Millisecond, func() {
		v := f.Volume(vol)
		v.SizeGib, v.PendingSizeGib, v.ResizeState = 7, 0, pvsv1beta1.Volume_RESIZE_COMPLETED
		f.PutVolume(v)
	})
	resp, err := d.ControllerExpandVolume(context.Background(), expandReq(vol, 7, nil))
	if err != nil || resp.CapacityBytes != 7*gib {
		t.Fatalf("%v, %v", resp, err)
	}
	if n := f.Calls["UpdateVolume"]; n != 0 {
		t.Fatalf("UpdateVolume called %d times while a resize to the same size was under way", n)
	}
}

func TestExpandVolumeRefusals(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	_, err := d.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{VolumeId: parent + "/volumes/vol-1"})
	wantCode(t, err, codes.InvalidArgument)
	_, err = d.ControllerExpandVolume(ctx, expandReq(parent+"/volumes/vol-gone", 6, nil))
	wantCode(t, err, codes.NotFound)
	_, err = d.ControllerExpandVolume(ctx, expandReq("not-a-volume", 6, nil))
	wantCode(t, err, codes.NotFound)
}
