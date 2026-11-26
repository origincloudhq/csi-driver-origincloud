package driver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/origincloudhq/csi-driver-origincloud/internal/cloud/fake"
	computev1beta1 "go.origincloud.eu/sdk/compute/v1beta1"
	"go.origincloud.eu/sdk/core"
	pvsv1beta1 "go.origincloud.eu/sdk/pvs/v1beta1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	zone   = "eu-central1-az1"
	parent = "projects/p1/zones/" + zone
	node   = parent + "/instances/i-node1"
	node2  = parent + "/instances/i-node2"
)

func newDriver(t *testing.T) (*Driver, *fake.Cloud) {
	t.Helper()
	f := fake.New()
	f.AddInstance(node)
	f.AddInstance(node2)
	d, err := New(Config{Cloud: f.Cloud(), Project: "p1", PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return d, f
}

func mountCap(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}

func createReq(name string) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:               name,
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 5 * gib},
		VolumeCapabilities: []*csi.VolumeCapability{mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Preferred: []*csi.Topology{{Segments: map[string]string{TopologyZoneKey: zone}}},
		},
	}
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("got %v (%v), want %v", got, err, want)
	}
}

func TestCreateVolumeIsIdempotentByName(t *testing.T) {
	d, f := newDriver(t)
	ctx := context.Background()
	first, err := d.CreateVolume(ctx, createReq("pvc-1"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := d.CreateVolume(ctx, createReq("pvc-1"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Volume.VolumeId != again.Volume.VolumeId {
		t.Fatalf("a retry created a second volume: %s, then %s", first.Volume.VolumeId, again.Volume.VolumeId)
	}
	if n := f.Calls["CreateVolume"]; n != 1 {
		t.Fatalf("CreateVolume called %d times, want 1", n)
	}
	vol := f.Volume(first.Volume.VolumeId)
	if vol.GetSizeGib() != 5 || vol.GetTags()[tagVolumeName] != "pvc-1" {
		t.Fatalf("volume %v", vol)
	}
	if got := first.Volume.AccessibleTopology[0].Segments[TopologyZoneKey]; got != zone {
		t.Fatalf("topology zone %q", got)
	}
	if first.Volume.CapacityBytes != 5*gib {
		t.Fatalf("capacity %d", first.Volume.CapacityBytes)
	}
}

// A first attempt whose wait timed out leaves a volume being created; the
// retry finds it by its tag and waits for it, rather than creating another.
func TestCreateVolumeRetryWaitsForTheFirstAttempt(t *testing.T) {
	d, f := newDriver(t)
	f.Hold = func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := d.CreateVolume(ctx, createReq("pvc-slow"))
	wantCode(t, err, codes.DeadlineExceeded)

	vols := f.Volumes()
	if len(vols) != 1 || vols[0].GetState() != pvsv1beta1.Volume_CREATING {
		t.Fatalf("volumes after the timeout: %v", vols)
	}
	// The volume becomes available a little later.
	go func() {
		time.Sleep(10 * time.Millisecond)
		v := f.Volume(vols[0].GetName())
		v.State = pvsv1beta1.Volume_AVAILABLE
		f.PutVolume(v)
	}()
	f.Hold = nil
	resp, err := d.CreateVolume(context.Background(), createReq("pvc-slow"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Volume.VolumeId != vols[0].GetName() || f.Calls["CreateVolume"] != 1 {
		t.Fatalf("retry returned %s after %d creates", resp.Volume.VolumeId, f.Calls["CreateVolume"])
	}
}

// When the tag lookup misses (the API's answer to the first create was
// lost before the volume was listed), the derived request id still makes
// the API return the first attempt's operation.
func TestCreateVolumeSendsTheSameRequestIDOnRetry(t *testing.T) {
	a := createRequestID(parent, "pvc-1")
	if a != createRequestID(parent, "pvc-1") {
		t.Fatal("the request id differs between attempts")
	}
	if a == createRequestID(parent, "pvc-2") || a == createRequestID("projects/p1/zones/other", "pvc-1") {
		t.Fatal("the request id doesn't depend on the name and zone")
	}
}

func TestCreateVolumeFailsForAVolumeInError(t *testing.T) {
	d, f := newDriver(t)
	f.PutVolume(&pvsv1beta1.Volume{
		Name: parent + "/volumes/vol-bad", SizeGib: 5, State: pvsv1beta1.Volume_ERROR, StateReason: "no space",
		Tags: map[string]string{tagVolumeName: "pvc-bad"},
	})
	_, err := d.CreateVolume(context.Background(), createReq("pvc-bad"))
	wantCode(t, err, codes.Internal)
	if !strings.Contains(err.Error(), "no space") {
		t.Fatalf("the error doesn't say why: %v", err)
	}
}

func TestCreateVolumeRefusesAnIncompatibleRepeat(t *testing.T) {
	d, _ := newDriver(t)
	if _, err := d.CreateVolume(context.Background(), createReq("pvc-1")); err != nil {
		t.Fatal(err)
	}
	bigger := createReq("pvc-1")
	bigger.CapacityRange.RequiredBytes = 10 * gib
	_, err := d.CreateVolume(context.Background(), bigger)
	wantCode(t, err, codes.AlreadyExists)
}

func TestCreateVolumeTopologyAndParameters(t *testing.T) {
	d, f := newDriver(t)
	req := createReq("pvc-z")
	req.AccessibilityRequirements = &csi.TopologyRequirement{
		Requisite: []*csi.Topology{{Segments: map[string]string{TopologyZoneKey: "eu-central1-az2"}}},
	}
	req.Parameters = map[string]string{
		ParameterStorageClass:         "//pvs.origincloudapis.eu/storageClasses/gp2",
		"csi.storage.k8s.io/pvc/name": "data",
	}
	resp, err := d.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Volume.VolumeId, "projects/p1/zones/eu-central1-az2/volumes/") {
		t.Fatalf("volume %s isn't in the requisite zone", resp.Volume.VolumeId)
	}
	if got := f.Volume(resp.Volume.VolumeId).GetStorageClass(); got != "//pvs.origincloudapis.eu/storageClasses/gp2" {
		t.Fatalf("storage class %q", got)
	}

	for _, bad := range []map[string]string{
		{ParameterStorageClass: "gp2"},
		{ParameterStorageClass: "//compute.origincloudapis.eu/storageClasses/gp2"},
		{"type": "ssd"},
	} {
		req := createReq("pvc-bad")
		req.Parameters = bad
		_, err := d.CreateVolume(context.Background(), req)
		wantCode(t, err, codes.InvalidArgument)
	}

	noZone := createReq("pvc-nozone")
	noZone.AccessibilityRequirements = nil
	_, err = d.CreateVolume(context.Background(), noZone)
	wantCode(t, err, codes.InvalidArgument)
}

func TestCreateVolumeAccessModes(t *testing.T) {
	d, _ := newDriver(t)
	for mode, want := range map[csi.VolumeCapability_AccessMode_Mode]codes.Code{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:        codes.OK,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER: codes.OK,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:  codes.OK,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:   codes.InvalidArgument,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:    codes.InvalidArgument,
	} {
		req := createReq("pvc-" + strings.ToLower(mode.String()))
		req.VolumeCapabilities = []*csi.VolumeCapability{mountCap(mode)}
		_, err := d.CreateVolume(context.Background(), req)
		wantCode(t, err, want)
	}
}

func TestSizeOf(t *testing.T) {
	for _, tc := range []struct {
		required, limit int64
		want            int32
		code            codes.Code
	}{
		{0, 0, 1, codes.OK},
		{1, 0, 1, codes.OK},
		{gib, 0, 1, codes.OK},
		{gib + 1, 0, 2, codes.OK},
		{gib + 1, 3 * gib, 2, codes.OK},
		{gib + 1, gib + 2, 0, codes.OutOfRange},
		{0, gib / 2, 0, codes.OutOfRange},
		{2 * gib, gib, 0, codes.InvalidArgument},
	} {
		got, err := sizeOf(&csi.CapacityRange{RequiredBytes: tc.required, LimitBytes: tc.limit})
		if status.Code(err) != tc.code || got != tc.want {
			t.Errorf("sizeOf(%d, %d) = %d, %v; want %d, %v", tc.required, tc.limit, got, err, tc.want, tc.code)
		}
	}
}

func TestDeleteVolumeIsIdempotent(t *testing.T) {
	d, f := newDriver(t)
	ctx := context.Background()
	resp, err := d.CreateVolume(ctx, createReq("pvc-1"))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Volume.VolumeId
	for range 2 {
		if _, err := d.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: id}); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.Volume(id).GetState(); got != pvsv1beta1.Volume_DELETED {
		t.Fatalf("volume is %v", got)
	}
	if n := f.Calls["DeleteVolume"]; n != 1 {
		t.Fatalf("DeleteVolume called %d times, want 1", n)
	}
	// IDs this driver never issued, and volumes that never existed, are
	// gone already.
	for _, id := range []string{"not-a-volume", parent + "/volumes/vol-never"} {
		if _, err := d.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: id}); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	// A recreated claim of the same name gets a new volume: the deleted
	// one doesn't count.
	again, err := d.CreateVolume(ctx, createReq("pvc-1"))
	if err != nil {
		t.Fatal(err)
	}
	if again.Volume.VolumeId == id {
		t.Fatal("CreateVolume returned the deleted volume")
	}
}

func TestDeleteVolumeWaitsForADeleteUnderWay(t *testing.T) {
	d, f := newDriver(t)
	name := parent + "/volumes/vol-going"
	f.PutVolume(&pvsv1beta1.Volume{Name: name, SizeGib: 1, State: pvsv1beta1.Volume_DELETING})
	go func() {
		time.Sleep(10 * time.Millisecond)
		f.PutVolume(&pvsv1beta1.Volume{Name: name, SizeGib: 1, State: pvsv1beta1.Volume_DELETED})
	}()
	if _, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: name}); err != nil {
		t.Fatal(err)
	}
	if n := f.Calls["DeleteVolume"]; n != 0 {
		t.Fatalf("DeleteVolume called %d times while a delete was under way", n)
	}
}

func TestDeleteVolumeRefusesAnAttachedVolume(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	vol := mustCreate(t, d, "pvc-1")
	mustPublish(t, d, vol, node)
	_, err := d.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: vol})
	wantCode(t, err, codes.FailedPrecondition)
}

func mustCreate(t *testing.T, d *Driver, name string) string {
	t.Helper()
	resp, err := d.CreateVolume(context.Background(), createReq(name))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Volume.VolumeId
}

func publishReq(vol, nodeID string) *csi.ControllerPublishVolumeRequest {
	return &csi.ControllerPublishVolumeRequest{
		VolumeId:         vol,
		NodeId:           nodeID,
		VolumeCapability: mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	}
}

func mustPublish(t *testing.T, d *Driver, vol, nodeID string) string {
	t.Helper()
	resp, err := d.ControllerPublishVolume(context.Background(), publishReq(vol, nodeID))
	if err != nil {
		t.Fatal(err)
	}
	return resp.PublishContext[PublishContextDevice]
}

func TestPublishIsIdempotent(t *testing.T) {
	d, f := newDriver(t)
	vol := mustCreate(t, d, "pvc-1")
	first := mustPublish(t, d, vol, node)
	again := mustPublish(t, d, vol, node)
	if first == "" || first != again {
		t.Fatalf("devices %q, then %q", first, again)
	}
	if n := f.Calls["CreateVolumeAttachment"]; n != 1 {
		t.Fatalf("CreateVolumeAttachment called %d times, want 1", n)
	}
}

func TestPublishToAnotherNodeFails(t *testing.T) {
	d, _ := newDriver(t)
	vol := mustCreate(t, d, "pvc-1")
	mustPublish(t, d, vol, node)
	_, err := d.ControllerPublishVolume(context.Background(), publishReq(vol, node2))
	wantCode(t, err, codes.FailedPrecondition)
}

func TestPublishNotFound(t *testing.T) {
	d, _ := newDriver(t)
	vol := mustCreate(t, d, "pvc-1")
	for _, tc := range []struct{ vol, node string }{
		{"garbage", node},
		{parent + "/volumes/vol-never", node},
		{vol, "garbage"},
		{vol, parent + "/instances/i-never"},
	} {
		_, err := d.ControllerPublishVolume(context.Background(), publishReq(tc.vol, tc.node))
		wantCode(t, err, codes.NotFound)
	}
}

func TestPublishAcrossZonesFails(t *testing.T) {
	d, f := newDriver(t)
	other := "projects/p1/zones/eu-central1-az2/instances/i-far"
	f.AddInstance(other)
	vol := mustCreate(t, d, "pvc-1")
	_, err := d.ControllerPublishVolume(context.Background(), publishReq(vol, other))
	wantCode(t, err, codes.FailedPrecondition)
}

// A first attempt whose wait timed out leaves an attachment under way;
// the retry finds and waits for it rather than attaching again.
func TestPublishRetryWaitsForTheFirstAttempt(t *testing.T) {
	d, f := newDriver(t)
	vol := mustCreate(t, d, "pvc-1")
	f.Hold = func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := d.ControllerPublishVolume(ctx, publishReq(vol, node))
	wantCode(t, err, codes.DeadlineExceeded)
	f.Hold = nil

	atts := f.Attachments()
	if len(atts) != 1 || atts[0].GetState() != computev1beta1.VolumeAttachment_ATTACHING {
		t.Fatalf("attachments after the timeout: %v", atts)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		a := atts[0]
		a.State = computev1beta1.VolumeAttachment_ATTACHED
		f.PutAttachment(a)
	}()
	if dev := mustPublish(t, d, vol, node); dev != atts[0].GetDevice() {
		t.Fatalf("device %q, want %q", dev, atts[0].GetDevice())
	}
	if n := f.Calls["CreateVolumeAttachment"]; n != 1 {
		t.Fatalf("CreateVolumeAttachment called %d times, want 1", n)
	}
}

func TestPublishFailedAttachment(t *testing.T) {
	d, f := newDriver(t)
	vol := mustCreate(t, d, "pvc-1")
	f.Hold = func(context.Context, string) error {
		return fake.Error("SERVICE_UNAVAILABLE", "try again")
	}
	_, err := d.ControllerPublishVolume(context.Background(), publishReq(vol, node))
	wantCode(t, err, codes.Unavailable)
}

func TestUnpublishIsIdempotent(t *testing.T) {
	d, f := newDriver(t)
	ctx := context.Background()
	vol := mustCreate(t, d, "pvc-1")
	mustPublish(t, d, vol, node)
	for range 2 {
		if _, err := d.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{VolumeId: vol, NodeId: node}); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.Attachments()) != 0 {
		t.Fatalf("attachments left: %v", f.Attachments())
	}
	if n := f.Calls["DeleteVolumeAttachment"]; n != 1 {
		t.Fatalf("DeleteVolumeAttachment called %d times, want 1", n)
	}
	if got := f.Volume(vol).GetState(); got != pvsv1beta1.Volume_AVAILABLE {
		t.Fatalf("volume is %v", got)
	}
	// Attach to the same node again after the detach: a new attachment.
	mustPublish(t, d, vol, node)
	if n := f.Calls["CreateVolumeAttachment"]; n != 2 {
		t.Fatalf("CreateVolumeAttachment called %d times, want 2", n)
	}
	// Unknown volumes and nodes are detached already.
	for _, req := range []*csi.ControllerUnpublishVolumeRequest{
		{VolumeId: "garbage", NodeId: node},
		{VolumeId: parent + "/volumes/vol-never", NodeId: node},
		{VolumeId: vol, NodeId: "garbage"},
		{VolumeId: vol, NodeId: node2},
	} {
		if _, err := d.ControllerUnpublishVolume(ctx, req); err != nil {
			t.Fatalf("%v: %v", req, err)
		}
	}
	// An empty node ID detaches it from every node.
	if _, err := d.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{VolumeId: vol}); err != nil {
		t.Fatal(err)
	}
	if len(f.Attachments()) != 0 {
		t.Fatalf("attachments left: %v", f.Attachments())
	}
}

func TestUnpublishWaitsForADetachUnderWay(t *testing.T) {
	d, f := newDriver(t)
	vol := mustCreate(t, d, "pvc-1")
	mustPublish(t, d, vol, node)
	var once sync.Once
	f.Hold = func(ctx context.Context, _ string) error {
		var err error
		once.Do(func() {
			<-ctx.Done()
			err = ctx.Err()
		})
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := d.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{VolumeId: vol, NodeId: node})
	wantCode(t, err, codes.DeadlineExceeded)
	atts := f.Attachments()
	if len(atts) != 1 || atts[0].GetState() != computev1beta1.VolumeAttachment_DETACHING {
		t.Fatalf("attachments after the timeout: %v", atts)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		f.RemoveAttachment(atts[0].GetName())
	}()
	if _, err := d.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{VolumeId: vol, NodeId: node}); err != nil {
		t.Fatal(err)
	}
	if n := f.Calls["DeleteVolumeAttachment"]; n != 1 {
		t.Fatalf("DeleteVolumeAttachment called %d times, want 1", n)
	}
}

func TestValidateVolumeCapabilities(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	vol := mustCreate(t, d, "pvc-1")
	ok, err := d.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           vol,
		VolumeCapabilities: []*csi.VolumeCapability{mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER)},
	})
	if err != nil || ok.Confirmed == nil {
		t.Fatalf("%v, %v", ok, err)
	}
	rwx, err := d.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           vol,
		VolumeCapabilities: []*csi.VolumeCapability{mountCap(csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)},
	})
	if err != nil || rwx.Confirmed != nil || rwx.Message == "" {
		t.Fatalf("%v, %v", rwx, err)
	}
	_, err = d.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           parent + "/volumes/vol-never",
		VolumeCapabilities: []*csi.VolumeCapability{mountCap(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)},
	})
	wantCode(t, err, codes.NotFound)
}

func TestErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want codes.Code
	}{
		{&core.Error{Reason: "NOT_FOUND", Status: "NOT_FOUND"}, codes.NotFound},
		{&core.Error{Reason: "CONFLICT", Status: "FAILED_PRECONDITION"}, codes.FailedPrecondition},
		{&core.Error{Reason: "INSUFFICIENT_CAPACITY", Status: "RESOURCE_EXHAUSTED"}, codes.ResourceExhausted},
		{&core.Error{Reason: "RESOURCE_EXHAUSTED", Status: "RESOURCE_EXHAUSTED"}, codes.ResourceExhausted},
		{&core.Error{Status: "RESOURCE_EXHAUSTED"}, codes.ResourceExhausted},
		{&core.Error{Reason: "BAD_REQUEST", Status: "INVALID_ARGUMENT", FieldViolations: []core.FieldViolation{{Field: "quota"}}}, codes.ResourceExhausted},
		{&core.Error{Reason: "BAD_REQUEST", Status: "INVALID_ARGUMENT", Message: "invalid input on field quota: project volumes 10 of 10"}, codes.ResourceExhausted},
		{&core.Error{Reason: "BAD_REQUEST", Status: "INVALID_ARGUMENT"}, codes.InvalidArgument},
		{&core.Error{Reason: "FORBIDDEN", Status: "PERMISSION_DENIED"}, codes.PermissionDenied},
		{&core.Error{Reason: "UNAUTHORIZED", Status: "UNAUTHENTICATED"}, codes.Unauthenticated},
		{&core.Error{Reason: "SERVICE_UNAVAILABLE", Status: "UNAVAILABLE"}, codes.Unavailable},
		{&core.Error{Reason: "PAYMENT_METHOD_REQUIRED", Status: "FAILED_PRECONDITION"}, codes.FailedPrecondition},
		{&core.Error{Reason: "INTERNAL_SERVER_ERROR", Status: "INTERNAL"}, codes.Internal},
		{context.DeadlineExceeded, codes.DeadlineExceeded},
		{context.Canceled, codes.Canceled},
		{errors.New("connection reset"), codes.Internal},
	} {
		if got := status.Code(apiStatus(tc.err, "doing")); got != tc.want {
			t.Errorf("%v: got %v, want %v", tc.err, got, tc.want)
		}
	}
}

// The mapping reaches the CSI caller through each call.
func TestCallsMapAPIErrors(t *testing.T) {
	d, f := newDriver(t)
	vol := mustCreate(t, d, "pvc-1")
	for reason, want := range map[string]codes.Code{
		"CONFLICT":              codes.FailedPrecondition,
		"INSUFFICIENT_CAPACITY": codes.ResourceExhausted,
		"NOT_FOUND":             codes.NotFound,
	} {
		f.Fail = func(string) error { return fake.Error(reason, "refused") }
		_, err := d.CreateVolume(context.Background(), createReq("pvc-"+strings.ToLower(reason)))
		wantCode(t, err, want)
		_, err = d.ControllerPublishVolume(context.Background(), publishReq(vol, node))
		wantCode(t, err, want)
	}
}

func TestNewChecksTheConfig(t *testing.T) {
	f := fake.New()
	for _, cfg := range []Config{
		{Cloud: f.Cloud()},
		{Cloud: f.Cloud(), Project: "a/b"},
		{Cloud: f.Cloud(), Project: "p1", DefaultZone: "x/y"},
		{Project: "p1"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
}
