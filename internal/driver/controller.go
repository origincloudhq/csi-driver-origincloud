package driver

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/google/uuid"
	computev1beta1 "go.origincloud.eu/sdk/compute/v1beta1"
	"go.origincloud.eu/sdk/core"
	pvsv1beta1 "go.origincloud.eu/sdk/pvs/v1beta1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const gib = int64(1) << 30

// PublishContextDevice is the publish context key under which
// ControllerPublishVolume tells the node plugin the device name the guest
// sees the volume under, such as `vdb`.
const PublishContextDevice = "device"

// ControllerGetCapabilities lists what the controller does. It advertises
// SINGLE_NODE_MULTI_WRITER so that a CO may ask for
// SINGLE_NODE_SINGLE_WRITER (Kubernetes' ReadWriteOncePod); with it,
// Kubernetes sends ReadWriteOnce as SINGLE_NODE_MULTI_WRITER, which is
// still one node.
func (d *Driver) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	var caps []*csi.ControllerServiceCapability
	for _, t := range []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	} {
		caps = append(caps, &csi.ControllerServiceCapability{Type: &csi.ControllerServiceCapability_Rpc{
			Rpc: &csi.ControllerServiceCapability_RPC{Type: t},
		}})
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: caps}, nil
}

// CreateVolume creates a PVS volume in the zone the CO asks for, or
// returns the one an earlier attempt of the same name created.
//
// It is idempotent by the CSI volume name twice over: the volume carries
// the name in a tag, which a retry looks for first, and the create call
// carries a request id derived from the name, so that a retry whose first
// attempt reached the API but whose answer was lost gets that attempt's
// operation rather than a second volume.
func (d *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume name is missing")
	}
	if len(name) > 256 || strings.TrimSpace(name) != name {
		return nil, status.Errorf(codes.InvalidArgument, "the volume name %q can't be stored in a tag: at most 256 characters, no leading or trailing spaces", name)
	}
	if err := checkCapabilities(req.GetVolumeCapabilities()); err != nil {
		return nil, err
	}
	if req.GetVolumeContentSource() != nil {
		return nil, status.Error(codes.InvalidArgument, "creating a volume from a snapshot or another volume isn't supported yet")
	}
	storageClass, err := storageClassOf(req.GetParameters())
	if err != nil {
		return nil, err
	}
	sizeGiB, err := sizeOf(req.GetCapacityRange())
	if err != nil {
		return nil, err
	}
	zone, err := d.zoneOf(req.GetAccessibilityRequirements())
	if err != nil {
		return nil, err
	}
	parent := zonal{Project: d.cfg.Project, Zone: zone}.Parent()

	vol, err := d.findVolume(ctx, parent, name)
	if err != nil {
		return nil, err
	}
	if vol != nil {
		if !fits(vol, req.GetCapacityRange(), storageClass) {
			return nil, status.Errorf(codes.AlreadyExists, "volume %q exists as %s with %d GiB of %s, which the request doesn't fit", name, vol.GetName(), vol.GetSizeGib(), vol.GetStorageClass())
		}
	} else {
		tags := map[string]string{tagVolumeName: name, tagCreatedBy: Name}
		if len(name) <= 128 {
			tags["origin:name"] = name
		}
		if vol, err = d.createVolume(ctx, parent, name, &pvsv1beta1.Volume{
			SizeGib:      sizeGiB,
			StorageClass: storageClass,
			Tags:         tags,
		}); err != nil {
			return nil, err
		}
	}
	if vol, err = d.settleVolume(ctx, vol); err != nil {
		return nil, err
	}
	z, ok := parseVolumeID(vol.GetName())
	if !ok {
		return nil, status.Errorf(codes.Internal, "the API named the volume %q, which isn't a zonal volume name", vol.GetName())
	}
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId:      vol.GetName(),
		CapacityBytes: int64(vol.GetSizeGib()) * gib,
		AccessibleTopology: []*csi.Topology{{
			Segments: map[string]string{TopologyZoneKey: z.Zone},
		}},
	}}, nil
}

// createVolume creates the volume spec for the CSI name in parent and
// waits for the operation.
//
// The request id is derived from the name, so a retry gets the first
// attempt's operation. That operation can be an old one: a volume of the
// same name was created and deleted before. Then the volume it returns is
// gone, and the volume is created again under a request id derived from
// the gone volume's name as well, which a retry derives the same way.
func (d *Driver) createVolume(ctx context.Context, parent, name string, spec *pvsv1beta1.Volume) (*pvsv1beta1.Volume, error) {
	requestID := createRequestID(parent, name)
	for range 8 {
		op, err := d.cfg.Cloud.Volumes.CreateVolume(ctx, &pvsv1beta1.CreateVolumeRequest{
			Parent:    parent,
			Volume:    spec,
			RequestId: requestID,
		})
		if err != nil {
			return nil, apiStatus(err, "creating volume "+name)
		}
		vol, err := op.Wait(ctx)
		if err != nil {
			return nil, apiStatus(err, "waiting for volume "+name+" to be created")
		}
		current, err := d.cfg.Cloud.Volumes.GetVolume(ctx, &pvsv1beta1.GetVolumeRequest{Name: vol.GetName()})
		switch {
		case isNotFound(err):
		case err != nil:
			return nil, apiStatus(err, "reading volume "+vol.GetName())
		case current.GetState() != pvsv1beta1.Volume_DELETING && current.GetState() != pvsv1beta1.Volume_DELETED:
			return current, nil
		}
		requestID = createRequestID(parent, name+"\x00"+vol.GetName())
	}
	return nil, status.Errorf(codes.Internal, "creating volume %s kept returning volumes that were deleted", name)
}

// findVolume returns the volume in parent that carries the CSI name, or
// nil. A volume on its way out doesn't count.
func (d *Driver) findVolume(ctx context.Context, parent, name string) (*pvsv1beta1.Volume, error) {
	for vol, err := range d.cfg.Cloud.Volumes.AllVolumes(ctx, &pvsv1beta1.ListVolumesRequest{Parent: parent}) {
		if err != nil {
			return nil, apiStatus(err, "listing the volumes of "+parent)
		}
		if vol.GetTags()[tagVolumeName] != name {
			continue
		}
		switch vol.GetState() {
		case pvsv1beta1.Volume_DELETING, pvsv1beta1.Volume_DELETED:
			continue
		}
		return vol, nil
	}
	return nil, nil
}

// settleVolume waits for a volume that is being created to be available,
// and fails for one that ended in ERROR.
func (d *Driver) settleVolume(ctx context.Context, vol *pvsv1beta1.Volume) (*pvsv1beta1.Volume, error) {
	err := d.poll(ctx, func() (bool, error) {
		switch vol.GetState() {
		case pvsv1beta1.Volume_ERROR:
			return false, status.Errorf(codes.Internal, "volume %s failed: %s", vol.GetName(), vol.GetStateReason())
		case pvsv1beta1.Volume_CREATING, pvsv1beta1.Volume_STATE_UNSPECIFIED:
		default:
			return true, nil
		}
		next, err := d.cfg.Cloud.Volumes.GetVolume(ctx, &pvsv1beta1.GetVolumeRequest{Name: vol.GetName()})
		if err != nil {
			return false, apiStatus(err, "reading volume "+vol.GetName())
		}
		vol = next
		return false, nil
	})
	return vol, err
}

// fits reports whether an existing volume satisfies a repeated request.
func fits(vol *pvsv1beta1.Volume, r *csi.CapacityRange, storageClass string) bool {
	size := int64(vol.GetSizeGib()) * gib
	if size < r.GetRequiredBytes() {
		return false
	}
	if r.GetLimitBytes() > 0 && size > r.GetLimitBytes() {
		return false
	}
	return storageClass == "" || vol.GetStorageClass() == storageClass
}

// DeleteVolume deletes a volume. A volume that doesn't exist, or whose ID
// this driver never issued, is deleted already.
func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	id := req.GetVolumeId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume ID is missing")
	}
	if _, ok := parseVolumeID(id); !ok {
		return &csi.DeleteVolumeResponse{}, nil
	}
	gone := false
	err := d.poll(ctx, func() (bool, error) {
		vol, err := d.cfg.Cloud.Volumes.GetVolume(ctx, &pvsv1beta1.GetVolumeRequest{Name: id})
		if isNotFound(err) {
			gone = true
			return true, nil
		}
		if err != nil {
			return false, apiStatus(err, "reading volume "+id)
		}
		switch vol.GetState() {
		case pvsv1beta1.Volume_DELETED:
			gone = true
			return true, nil
		case pvsv1beta1.Volume_IN_USE:
			return false, status.Errorf(codes.FailedPrecondition, "volume %s is attached (%s); detach it first", id, vol.GetAttachment())
		case pvsv1beta1.Volume_DELETING, pvsv1beta1.Volume_CREATING:
			// Another call is changing it: wait for it to settle.
			return false, nil
		}
		return true, nil
	})
	if err != nil || gone {
		return deleted(err)
	}
	op, err := d.cfg.Cloud.Volumes.DeleteVolume(ctx, &pvsv1beta1.DeleteVolumeRequest{Name: id, RequestId: uuid.NewString()})
	if isNotFound(err) {
		return deleted(nil)
	}
	if err != nil {
		return nil, apiStatus(err, "deleting volume "+id)
	}
	if _, err := op.Wait(ctx); err != nil && !isNotFound(err) {
		return nil, apiStatus(err, "waiting for volume "+id+" to be deleted")
	}
	return deleted(nil)
}

func deleted(err error) (*csi.DeleteVolumeResponse, error) {
	if err != nil {
		return nil, err
	}
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerPublishVolume attaches a volume to the node's instance, or
// returns the attachment that holds it there already. The publish context
// names the device the guest sees it under.
func (d *Driver) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	volumeID, nodeID := req.GetVolumeId(), req.GetNodeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume ID is missing")
	}
	if nodeID == "" {
		return nil, status.Error(codes.InvalidArgument, "the node ID is missing")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "the volume capability is missing")
	}
	if err := checkCapabilities([]*csi.VolumeCapability{req.GetVolumeCapability()}); err != nil {
		return nil, err
	}
	vz, ok := parseVolumeID(volumeID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "volume %q not found", volumeID)
	}
	nz, ok := parseNodeID(nodeID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "node %q not found", nodeID)
	}
	if _, err := d.cfg.Cloud.Volumes.GetVolume(ctx, &pvsv1beta1.GetVolumeRequest{Name: volumeID}); err != nil {
		return nil, apiStatus(err, "reading volume "+volumeID)
	}
	inst, err := d.cfg.Cloud.Instances.GetInstance(ctx, &computev1beta1.GetInstanceRequest{Name: nodeID})
	if err != nil {
		return nil, apiStatus(err, "reading node "+nodeID)
	}
	if inst.GetState() == computev1beta1.Instance_TERMINATED {
		return nil, status.Errorf(codes.NotFound, "node %s is terminated", nodeID)
	}
	if vz.Project != nz.Project || vz.Zone != nz.Zone {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s is in %s, node %s in %s: a volume attaches only in its own zone", volumeID, vz.Parent(), nodeID, nz.Parent())
	}

	att, err := d.attachmentOn(ctx, vz, nz)
	if err != nil {
		return nil, err
	}
	if att == nil {
		op, err := d.cfg.Cloud.Attachments.CreateVolumeAttachment(ctx, &computev1beta1.CreateVolumeAttachmentRequest{
			Parent:           nodeID,
			VolumeAttachment: &computev1beta1.VolumeAttachment{Volume: pvsFullName(volumeID)},
			// A fresh id each time: the same volume may be attached to the
			// same node again after it was detached. A retry finds the
			// first attempt's attachment by listing instead.
			RequestId: uuid.NewString(),
		})
		if err != nil {
			return nil, apiStatus(err, "attaching volume "+volumeID+" to node "+nodeID)
		}
		if att, err = op.Wait(ctx); err != nil {
			return nil, apiStatus(err, "waiting for volume "+volumeID+" to attach to node "+nodeID)
		}
	}
	if att, err = d.settleAttachment(ctx, att); err != nil {
		return nil, err
	}
	return &csi.ControllerPublishVolumeResponse{PublishContext: map[string]string{
		PublishContextDevice: att.GetDevice(),
	}}, nil
}

// attachmentOn returns the volume's attachment on the node, after waiting
// out a detach under way there; nil when there is none. It fails with
// FailedPrecondition when another instance holds the volume.
func (d *Driver) attachmentOn(ctx context.Context, vz, nz zonal) (*computev1beta1.VolumeAttachment, error) {
	var found *computev1beta1.VolumeAttachment
	err := d.poll(ctx, func() (bool, error) {
		found = nil
		atts, err := d.attachmentsOf(ctx, vz)
		if err != nil {
			return false, err
		}
		leaving := false
		for _, att := range atts {
			if !holds(att) {
				continue
			}
			if !onInstance(att, nz) {
				return false, status.Errorf(codes.FailedPrecondition, "volume %s is attached to another node: %s", volumeName(vz), att.GetName())
			}
			switch att.GetState() {
			case computev1beta1.VolumeAttachment_DETACHING, computev1beta1.VolumeAttachment_RELEASING:
				leaving = true
			default:
				found = att
			}
		}
		return found != nil || !leaving, nil
	})
	return found, err
}

// attachmentsOf lists the attachments of the volume in its zone.
func (d *Driver) attachmentsOf(ctx context.Context, vz zonal) ([]*computev1beta1.VolumeAttachment, error) {
	var out []*computev1beta1.VolumeAttachment
	for att, err := range d.cfg.Cloud.Attachments.AllVolumeAttachments(ctx, &computev1beta1.ListVolumeAttachmentsRequest{
		Parent: vz.Parent() + "/instances/-",
		Volume: pvsFullName(volumeName(vz)),
	}) {
		if err != nil {
			return nil, apiStatus(err, "listing the attachments of volume "+volumeName(vz))
		}
		out = append(out, att)
	}
	return out, nil
}

// holds reports whether an attachment holds its volume. A failed one
// doesn't.
func holds(att *computev1beta1.VolumeAttachment) bool {
	return att.GetState() != computev1beta1.VolumeAttachment_FAILED
}

// onInstance reports whether the attachment is on the node's instance.
func onInstance(att *computev1beta1.VolumeAttachment, nz zonal) bool {
	return strings.HasPrefix(att.GetName(), instanceName(nz)+"/volumeAttachments/")
}

// settleAttachment waits for an attachment under way to be attached.
func (d *Driver) settleAttachment(ctx context.Context, att *computev1beta1.VolumeAttachment) (*computev1beta1.VolumeAttachment, error) {
	err := d.poll(ctx, func() (bool, error) {
		switch att.GetState() {
		case computev1beta1.VolumeAttachment_ATTACHED:
			return true, nil
		case computev1beta1.VolumeAttachment_CREATING, computev1beta1.VolumeAttachment_ATTACHING, computev1beta1.VolumeAttachment_STATE_UNSPECIFIED:
		default:
			return false, status.Errorf(codes.Internal, "attachment %s is %s", att.GetName(), att.GetState())
		}
		next, err := d.cfg.Cloud.Attachments.GetVolumeAttachment(ctx, &computev1beta1.GetVolumeAttachmentRequest{Name: att.GetName()})
		if err != nil {
			return false, apiStatus(err, "reading attachment "+att.GetName())
		}
		att = next
		return false, nil
	})
	return att, err
}

// ControllerUnpublishVolume detaches a volume from the node's instance, or
// from every instance when the node ID is empty. A volume or node that
// doesn't exist is detached already.
func (d *Driver) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	volumeID, nodeID := req.GetVolumeId(), req.GetNodeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume ID is missing")
	}
	vz, ok := parseVolumeID(volumeID)
	if !ok {
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}
	var nz *zonal
	if nodeID != "" {
		z, ok := parseNodeID(nodeID)
		if !ok {
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}
		nz = &z
	}
	err := d.poll(ctx, func() (bool, error) {
		atts, err := d.attachmentsOf(ctx, vz)
		if err != nil {
			return false, err
		}
		pending := false
		for _, att := range atts {
			if att.GetBoot() || nz != nil && !onInstance(att, *nz) {
				continue
			}
			switch att.GetState() {
			case computev1beta1.VolumeAttachment_DETACHING, computev1beta1.VolumeAttachment_RELEASING:
				pending = true
				continue
			}
			op, err := d.cfg.Cloud.Attachments.DeleteVolumeAttachment(ctx, &computev1beta1.DeleteVolumeAttachmentRequest{
				Name:      att.GetName(),
				RequestId: uuid.NewString(),
			})
			if isNotFound(err) {
				continue
			}
			if err != nil {
				return false, apiStatus(err, "detaching "+att.GetName())
			}
			if _, err := op.Wait(ctx); err != nil && !isNotFound(err) {
				return false, apiStatus(err, "waiting for "+att.GetName()+" to detach")
			}
		}
		return !pending, nil
	})
	if err != nil {
		return nil, err
	}
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ValidateVolumeCapabilities confirms the capabilities the driver offers
// for an existing volume.
func (d *Driver) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	id := req.GetVolumeId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume ID is missing")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "the volume capabilities are missing")
	}
	if _, ok := parseVolumeID(id); !ok {
		return nil, status.Errorf(codes.NotFound, "volume %q not found", id)
	}
	if _, err := d.cfg.Cloud.Volumes.GetVolume(ctx, &pvsv1beta1.GetVolumeRequest{Name: id}); err != nil {
		return nil, apiStatus(err, "reading volume "+id)
	}
	if err := checkCapabilities(req.GetVolumeCapabilities()); err != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{Message: status.Convert(err).Message()}, nil
	}
	if _, err := storageClassOf(req.GetParameters()); err != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{Message: status.Convert(err).Message()}, nil
	}
	return &csi.ValidateVolumeCapabilitiesResponse{Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
		VolumeContext:      req.GetVolumeContext(),
		VolumeCapabilities: req.GetVolumeCapabilities(),
		Parameters:         req.GetParameters(),
	}}, nil
}

// checkCapabilities fails with InvalidArgument unless every capability is
// one the driver offers: a block device or a filesystem, on one node. A
// PVS volume attaches to one instance at a time, so no multi-node mode is
// offered.
func checkCapabilities(caps []*csi.VolumeCapability) error {
	if len(caps) == 0 {
		return status.Error(codes.InvalidArgument, "the volume capabilities are missing")
	}
	for _, c := range caps {
		if c.GetBlock() == nil && c.GetMount() == nil {
			return status.Error(codes.InvalidArgument, "a volume capability names neither block nor mount access")
		}
		switch mode := c.GetAccessMode().GetMode(); mode {
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:
		default:
			return status.Errorf(codes.InvalidArgument, "access mode %s isn't supported: a volume attaches to one node at a time", mode)
		}
	}
	return nil
}

// storageClassOf reads the StorageClass parameters. Kubernetes' own
// `csi.storage.k8s.io/` parameters are left to the sidecars.
func storageClassOf(params map[string]string) (string, error) {
	var class string
	for k, v := range params {
		switch {
		case k == ParameterStorageClass:
			n, err := core.ParseFullName(v)
			if err != nil || n.Service() != "pvs" || !strings.HasPrefix(n.Relative(), "storageClasses/") || strings.Count(n.Relative(), "/") != 1 {
				return "", status.Errorf(codes.InvalidArgument, "parameter %s must be a PVS storage class by its full name, such as //pvs.origincloudapis.eu/storageClasses/gp1, got %q", k, v)
			}
			class = v
		case strings.HasPrefix(k, "csi.storage.k8s.io/"):
		default:
			return "", status.Errorf(codes.InvalidArgument, "unknown parameter %q; the driver takes %q", k, ParameterStorageClass)
		}
	}
	return class, nil
}

// sizeOf returns the volume size in GiB a capacity range asks for: the
// required bytes rounded up to whole GiB, 1 GiB when none are required.
func sizeOf(r *csi.CapacityRange) (int32, error) {
	required, limit := r.GetRequiredBytes(), r.GetLimitBytes()
	if required < 0 || limit < 0 {
		return 0, status.Error(codes.InvalidArgument, "the capacity range is negative")
	}
	if limit > 0 && required > limit {
		return 0, status.Errorf(codes.InvalidArgument, "the required %d bytes exceed the limit of %d", required, limit)
	}
	size := max((required+gib-1)/gib, 1)
	if limit > 0 && size*gib > limit {
		return 0, status.Errorf(codes.OutOfRange, "a volume is a whole number of GiB, at least 1: none fits between %d and %d bytes", required, limit)
	}
	if size > math.MaxInt32 {
		return 0, status.Errorf(codes.OutOfRange, "%d GiB is too large", size)
	}
	return int32(size), nil
}

// zoneOf picks the zone to create a volume in: the first preferred
// topology's, else the first requisite's, else the default zone. With
// WaitForFirstConsumer the first preferred one is the zone of the node the
// scheduler picked.
func (d *Driver) zoneOf(req *csi.TopologyRequirement) (string, error) {
	for _, list := range [][]*csi.Topology{req.GetPreferred(), req.GetRequisite()} {
		for _, t := range list {
			zone, ok := t.GetSegments()[TopologyZoneKey]
			if !ok {
				continue
			}
			if !segment(zone) {
				return "", status.Errorf(codes.InvalidArgument, "topology key %s holds %q, which isn't a zone code", TopologyZoneKey, zone)
			}
			return zone, nil
		}
	}
	if d.cfg.DefaultZone == "" {
		return "", status.Errorf(codes.InvalidArgument, "the request names no zone under %s, and the controller has no default zone", TopologyZoneKey)
	}
	return d.cfg.DefaultZone, nil
}

// poll calls step until it reports done or fails, waiting the poll
// interval between calls, and fails with DeadlineExceeded or Canceled once
// ctx ends. The CO retries such a call, and the retry picks up where this
// one stopped.
func (d *Driver) poll(ctx context.Context, step func() (bool, error)) error {
	for {
		done, err := step()
		if err != nil || done {
			return err
		}
		t := time.NewTimer(d.cfg.PollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return apiStatus(ctx.Err(), "waiting")
		case <-t.C:
		}
	}
}
