// Package cloud is the driver's view of the Origin Cloud API: the calls the
// controller makes on PVS volumes and on compute volume attachments, as
// interfaces the driver depends on, and their implementation over
// go.origincloud.eu/sdk.
//
// The interfaces return operations rather than finished results, so that the
// driver decides how long to wait for them, and so that a test can fake an
// operation that is still under way or fails.
package cloud

import (
	"context"
	"iter"

	computev1beta1 "go.origincloud.eu/sdk/compute/v1beta1"
	"go.origincloud.eu/sdk/core"
	pvsv1beta1 "go.origincloud.eu/sdk/pvs/v1beta1"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Operation is a long-running operation the API started. Wait blocks until
// it is done and returns its result or the error it failed with, or ctx's
// error when ctx ends first; the operation goes on either way.
type Operation[T any] interface {
	Wait(ctx context.Context) (T, error)
}

// Volumes are PVS volumes.
type Volumes interface {
	GetVolume(ctx context.Context, req *pvsv1beta1.GetVolumeRequest) (*pvsv1beta1.Volume, error)
	AllVolumes(ctx context.Context, req *pvsv1beta1.ListVolumesRequest) iter.Seq2[*pvsv1beta1.Volume, error]
	CreateVolume(ctx context.Context, req *pvsv1beta1.CreateVolumeRequest) (Operation[*pvsv1beta1.Volume], error)
	DeleteVolume(ctx context.Context, req *pvsv1beta1.DeleteVolumeRequest) (Operation[*emptypb.Empty], error)
}

// Instances are compute instances; the controller only reads them.
type Instances interface {
	GetInstance(ctx context.Context, req *computev1beta1.GetInstanceRequest) (*computev1beta1.Instance, error)
}

// Attachments are compute volume attachments: a volume attached to an
// instance.
type Attachments interface {
	GetVolumeAttachment(ctx context.Context, req *computev1beta1.GetVolumeAttachmentRequest) (*computev1beta1.VolumeAttachment, error)
	AllVolumeAttachments(ctx context.Context, req *computev1beta1.ListVolumeAttachmentsRequest) iter.Seq2[*computev1beta1.VolumeAttachment, error]
	CreateVolumeAttachment(ctx context.Context, req *computev1beta1.CreateVolumeAttachmentRequest) (Operation[*computev1beta1.VolumeAttachment], error)
	DeleteVolumeAttachment(ctx context.Context, req *computev1beta1.DeleteVolumeAttachmentRequest) (Operation[*emptypb.Empty], error)
}

// Cloud bundles the APIs the controller calls.
type Cloud struct {
	Volumes     Volumes
	Instances   Instances
	Attachments Attachments
}

// New returns the Cloud of client, which calls the public REST API.
func New(client *core.Client) Cloud {
	return Cloud{
		Volumes:     sdkVolumes{pvsv1beta1.NewVolumeClient(client)},
		Instances:   computev1beta1.NewInstanceClient(client),
		Attachments: sdkAttachments{computev1beta1.NewVolumeAttachmentClient(client)},
	}
}

// sdkVolumes adapts the SDK's volume client, whose calls return the
// concrete *core.Operation, to Volumes.
type sdkVolumes struct{ *pvsv1beta1.VolumeClient }

func (v sdkVolumes) CreateVolume(ctx context.Context, req *pvsv1beta1.CreateVolumeRequest) (Operation[*pvsv1beta1.Volume], error) {
	op, err := v.VolumeClient.CreateVolume(ctx, req)
	if err != nil {
		return nil, err
	}
	return op, nil
}

func (v sdkVolumes) DeleteVolume(ctx context.Context, req *pvsv1beta1.DeleteVolumeRequest) (Operation[*emptypb.Empty], error) {
	op, err := v.VolumeClient.DeleteVolume(ctx, req)
	if err != nil {
		return nil, err
	}
	return op, nil
}

// sdkAttachments adapts the SDK's volume attachment client to Attachments.
type sdkAttachments struct {
	*computev1beta1.VolumeAttachmentClient
}

func (a sdkAttachments) CreateVolumeAttachment(ctx context.Context, req *computev1beta1.CreateVolumeAttachmentRequest) (Operation[*computev1beta1.VolumeAttachment], error) {
	op, err := a.VolumeAttachmentClient.CreateVolumeAttachment(ctx, req)
	if err != nil {
		return nil, err
	}
	return op, nil
}

func (a sdkAttachments) DeleteVolumeAttachment(ctx context.Context, req *computev1beta1.DeleteVolumeAttachmentRequest) (Operation[*emptypb.Empty], error) {
	op, err := a.VolumeAttachmentClient.DeleteVolumeAttachment(ctx, req)
	if err != nil {
		return nil, err
	}
	return op, nil
}
