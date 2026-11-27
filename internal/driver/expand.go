package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/google/uuid"
	pvsv1beta1 "go.origincloud.eu/sdk/pvs/v1beta1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// ControllerExpandVolume grows a volume, attached or not, to the capacity
// range's required bytes rounded up to whole GiB. Growing it to the size
// it has changes nothing; a smaller size is refused with OutOfRange, as a
// PVS volume never shrinks. A resize already under way is waited for
// first. A filesystem volume then needs the node to grow its filesystem.
func (d *Driver) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	id := req.GetVolumeId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume ID is missing")
	}
	if req.GetCapacityRange() == nil {
		return nil, status.Error(codes.InvalidArgument, "the capacity range is missing")
	}
	if c := req.GetVolumeCapability(); c != nil {
		if err := checkCapabilities([]*csi.VolumeCapability{c}); err != nil {
			return nil, err
		}
	}
	size, err := sizeOf(req.GetCapacityRange())
	if err != nil {
		return nil, err
	}
	if _, ok := parseVolumeID(id); !ok {
		return nil, status.Errorf(codes.NotFound, "volume %q not found", id)
	}

	vol, err := d.settleResize(ctx, id)
	if err != nil {
		return nil, err
	}
	switch {
	case size < vol.GetSizeGib():
		return nil, status.Errorf(codes.OutOfRange, "volume %s has %d GiB and never shrinks: %d GiB is smaller", id, vol.GetSizeGib(), size)
	case size > vol.GetSizeGib():
		op, err := d.cfg.Cloud.Volumes.UpdateVolume(ctx, &pvsv1beta1.UpdateVolumeRequest{
			Volume:     &pvsv1beta1.Volume{Name: id, SizeGib: size},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"size_gib"}},
			// A fresh id each time: a retry finds the first attempt's
			// resize on the volume and waits for it instead.
			RequestId: uuid.NewString(),
		})
		if err != nil {
			return nil, apiStatus(err, "growing volume "+id)
		}
		if _, err := op.Wait(ctx); err != nil {
			return nil, apiStatus(err, "waiting for volume "+id+" to grow")
		}
		if vol, err = d.settleResize(ctx, id); err != nil {
			return nil, err
		}
		if vol.GetResizeState() == pvsv1beta1.Volume_RESIZE_FAILED || vol.GetSizeGib() < size {
			return nil, status.Errorf(codes.Internal, "volume %s didn't grow to %d GiB, it has %d: %s", id, size, vol.GetSizeGib(), vol.GetResizeReason())
		}
	}
	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         int64(vol.GetSizeGib()) * gib,
		NodeExpansionRequired: req.GetVolumeCapability().GetBlock() == nil,
	}, nil
}

// settleResize reads a volume once no resize of it is under way, and
// fails for one that is gone or can't be resized.
func (d *Driver) settleResize(ctx context.Context, id string) (*pvsv1beta1.Volume, error) {
	var vol *pvsv1beta1.Volume
	err := d.poll(ctx, func() (bool, error) {
		var err error
		vol, err = d.cfg.Cloud.Volumes.GetVolume(ctx, &pvsv1beta1.GetVolumeRequest{Name: id})
		if err != nil {
			return false, apiStatus(err, "reading volume "+id)
		}
		switch vol.GetState() {
		case pvsv1beta1.Volume_DELETING, pvsv1beta1.Volume_DELETED:
			return false, status.Errorf(codes.NotFound, "volume %s is %s", id, vol.GetState())
		case pvsv1beta1.Volume_ERROR:
			return false, status.Errorf(codes.FailedPrecondition, "volume %s failed: %s", id, vol.GetStateReason())
		case pvsv1beta1.Volume_CREATING, pvsv1beta1.Volume_STATE_UNSPECIFIED:
			return false, nil
		}
		return vol.GetResizeState() != pvsv1beta1.Volume_RESIZING, nil
	})
	return vol, err
}
