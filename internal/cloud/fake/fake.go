// Package fake is an in-memory stand-in for the Origin Cloud API the
// controller calls, for unit tests and the csi-sanity suite. It keeps the
// API's observable behavior the driver relies on: a repeated request id
// returns the first request's operation, an in-use volume can't be deleted,
// a volume is attached to at most one instance, a volume is created only
// from a protected snapshot and never shrinks, a snapshot is deleted only
// once unprotected and unprotected only once no volume comes from it, and
// errors carry the platform's reasons.
package fake

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync"

	"github.com/origincloudhq/csi-driver-origincloud/internal/cloud"
	computev1beta1 "go.origincloud.eu/sdk/compute/v1beta1"
	"go.origincloud.eu/sdk/core"
	pvsv1beta1 "go.origincloud.eu/sdk/pvs/v1beta1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Cloud is the fake API. The zero value is not usable; call New.
type Cloud struct {
	mu          sync.Mutex
	volumes     map[string]*pvsv1beta1.Volume
	snapshots   map[string]*pvsv1beta1.Snapshot
	instances   map[string]*computev1beta1.Instance
	attachments map[string]*computev1beta1.VolumeAttachment
	// requests maps a request id to the result its first request had, so
	// that a repeat returns that result again.
	requests map[string]any
	next     int

	// Hold, when set, is called by every operation's Wait before it
	// returns; a test blocks or fails waits with it.
	Hold func(ctx context.Context, kind string) error
	// Fail, when set, is asked before each call that changes something,
	// by its kind ("CreateVolume", ...); a non-nil error refuses the call.
	Fail func(kind string) error

	// Calls counts the calls that changed something, by kind.
	Calls map[string]int
}

// New returns an empty fake.
func New() *Cloud {
	return &Cloud{
		volumes:     map[string]*pvsv1beta1.Volume{},
		snapshots:   map[string]*pvsv1beta1.Snapshot{},
		instances:   map[string]*computev1beta1.Instance{},
		attachments: map[string]*computev1beta1.VolumeAttachment{},
		requests:    map[string]any{},
		Calls:       map[string]int{},
	}
}

// Cloud returns the fake as the driver's cloud.Cloud.
func (c *Cloud) Cloud() cloud.Cloud {
	return cloud.Cloud{Volumes: volumes{c}, Snapshots: snapshots{c}, Instances: instances{c}, Attachments: attachments{c}}
}

// AddInstance adds a running instance, `projects/{project}/zones/{zone}/instances/{id}`.
func (c *Cloud) AddInstance(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.instances[name] = &computev1beta1.Instance{Name: name, State: computev1beta1.Instance_RUNNING}
}

// PutVolume stores v as it is, replacing any volume of its name.
func (c *Cloud) PutVolume(v *pvsv1beta1.Volume) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.volumes[v.GetName()] = proto.Clone(v).(*pvsv1beta1.Volume)
}

// PutAttachment stores a as it is, replacing any attachment of its name.
func (c *Cloud) PutAttachment(a *computev1beta1.VolumeAttachment) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attachments[a.GetName()] = proto.Clone(a).(*computev1beta1.VolumeAttachment)
}

// RemoveAttachment finishes a detach: the attachment is gone and its
// volume available.
func (c *Cloud) RemoveAttachment(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	att, ok := c.attachments[name]
	if !ok {
		return
	}
	delete(c.attachments, name)
	volume, _ := strings.CutPrefix(att.Volume, pvsFullName(""))
	if vol, ok := c.volumes[volume]; ok && vol.State == pvsv1beta1.Volume_IN_USE {
		vol.State = pvsv1beta1.Volume_AVAILABLE
		vol.Attachment = ""
	}
}

// Volume returns the volume of name, or nil.
func (c *Cloud) Volume(name string) *pvsv1beta1.Volume {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.volumes[name]; ok {
		return proto.Clone(v).(*pvsv1beta1.Volume)
	}
	return nil
}

// Volumes returns every volume, deleted ones included.
func (c *Cloud) Volumes() []*pvsv1beta1.Volume {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*pvsv1beta1.Volume
	for _, v := range c.volumes {
		out = append(out, proto.Clone(v).(*pvsv1beta1.Volume))
	}
	return out
}

// Attachments returns every attachment.
func (c *Cloud) Attachments() []*computev1beta1.VolumeAttachment {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*computev1beta1.VolumeAttachment
	for _, a := range c.attachments {
		out = append(out, proto.Clone(a).(*computev1beta1.VolumeAttachment))
	}
	return out
}

// Error returns the *core.Error the API answers reason with.
func Error(reason, format string, args ...any) error {
	return &core.Error{Reason: reason, Status: reason, Message: fmt.Sprintf(format, args...)}
}

func notFound(name string) error { return Error("NOT_FOUND", "%s not found", name) }

// change records a call that changes something and asks Fail about it.
// The caller holds mu.
func (c *Cloud) change(kind string) error {
	c.Calls[kind]++
	if c.Fail != nil {
		return c.Fail(kind)
	}
	return nil
}

func (c *Cloud) id(prefix string) string {
	c.next++
	return fmt.Sprintf("%s-%06d", prefix, c.next)
}

// op is a fake operation: done already, its result decided when it was
// started, and Wait passes through Hold.
type op[T any] struct {
	c      *Cloud
	kind   string
	result T
	err    error
	// done, when set, runs once Hold let the wait through: the change
	// the operation makes when it finishes.
	done func()
}

func (o *op[T]) Wait(ctx context.Context) (T, error) {
	var zero T
	if o.c.Hold != nil {
		if err := o.c.Hold(ctx, o.kind); err != nil {
			return zero, err
		}
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if o.done != nil {
		o.c.mu.Lock()
		o.done()
		o.done = nil
		o.c.mu.Unlock()
	}
	return o.result, o.err
}

type volumes struct{ c *Cloud }

func (v volumes) GetVolume(_ context.Context, req *pvsv1beta1.GetVolumeRequest) (*pvsv1beta1.Volume, error) {
	v.c.mu.Lock()
	defer v.c.mu.Unlock()
	vol, ok := v.c.volumes[req.GetName()]
	if !ok || vol.GetState() == pvsv1beta1.Volume_DELETED {
		return nil, notFound(req.GetName())
	}
	return proto.Clone(vol).(*pvsv1beta1.Volume), nil
}

func (v volumes) AllVolumes(_ context.Context, req *pvsv1beta1.ListVolumesRequest) iter.Seq2[*pvsv1beta1.Volume, error] {
	v.c.mu.Lock()
	var page []*pvsv1beta1.Volume
	for name, vol := range v.c.volumes {
		if strings.HasPrefix(name, req.GetParent()+"/volumes/") && vol.GetState() != pvsv1beta1.Volume_DELETED {
			page = append(page, proto.Clone(vol).(*pvsv1beta1.Volume))
		}
	}
	v.c.mu.Unlock()
	return func(yield func(*pvsv1beta1.Volume, error) bool) {
		for _, vol := range page {
			if !yield(vol, nil) {
				return
			}
		}
	}
}

func (v volumes) CreateVolume(_ context.Context, req *pvsv1beta1.CreateVolumeRequest) (cloud.Operation[*pvsv1beta1.Volume], error) {
	c := v.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if prior, ok := c.requests[req.GetRequestId()]; ok && req.GetRequestId() != "" {
		return prior.(*op[*pvsv1beta1.Volume]), nil
	}
	if err := c.change("CreateVolume"); err != nil {
		return nil, err
	}
	if req.GetVolume().GetSizeGib() < 1 {
		return nil, Error("BAD_REQUEST", "invalid input on field sizeGib: must be at least 1")
	}
	if err := c.checkSource(req.GetParent(), req.GetVolume()); err != nil {
		return nil, err
	}
	vol := proto.Clone(req.GetVolume()).(*pvsv1beta1.Volume)
	vol.Name = req.GetParent() + "/volumes/" + c.id("vol")
	vol.State = pvsv1beta1.Volume_CREATING
	if vol.StorageClass == "" {
		vol.StorageClass = "//pvs.origincloudapis.eu/storageClasses/gp1"
	}
	c.volumes[vol.Name] = vol
	ready := proto.Clone(vol).(*pvsv1beta1.Volume)
	ready.State = pvsv1beta1.Volume_AVAILABLE
	o := &op[*pvsv1beta1.Volume]{c: c, kind: "CreateVolume", result: ready, done: func() {
		if cur, ok := c.volumes[ready.Name]; ok && cur.State == pvsv1beta1.Volume_CREATING {
			cur.State = pvsv1beta1.Volume_AVAILABLE
		}
	}}
	if req.GetRequestId() != "" {
		c.requests[req.GetRequestId()] = o
	}
	return o, nil
}

func (v volumes) DeleteVolume(_ context.Context, req *pvsv1beta1.DeleteVolumeRequest) (cloud.Operation[*emptypb.Empty], error) {
	c := v.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.change("DeleteVolume"); err != nil {
		return nil, err
	}
	vol, ok := c.volumes[req.GetName()]
	if !ok || vol.State == pvsv1beta1.Volume_DELETED {
		return nil, notFound(req.GetName())
	}
	if vol.State == pvsv1beta1.Volume_IN_USE {
		return nil, Error("CONFLICT", "volume %s is attached", req.GetName())
	}
	vol.State = pvsv1beta1.Volume_DELETING
	name := vol.Name
	return &op[*emptypb.Empty]{c: c, kind: "DeleteVolume", result: &emptypb.Empty{}, done: func() {
		if cur, ok := c.volumes[name]; ok {
			cur.State = pvsv1beta1.Volume_DELETED
		}
	}}, nil
}

type instances struct{ c *Cloud }

func (i instances) GetInstance(_ context.Context, req *computev1beta1.GetInstanceRequest) (*computev1beta1.Instance, error) {
	i.c.mu.Lock()
	defer i.c.mu.Unlock()
	inst, ok := i.c.instances[req.GetName()]
	if !ok {
		return nil, notFound(req.GetName())
	}
	return proto.Clone(inst).(*computev1beta1.Instance), nil
}

type attachments struct{ c *Cloud }

func (a attachments) GetVolumeAttachment(_ context.Context, req *computev1beta1.GetVolumeAttachmentRequest) (*computev1beta1.VolumeAttachment, error) {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	att, ok := a.c.attachments[req.GetName()]
	if !ok {
		return nil, notFound(req.GetName())
	}
	return proto.Clone(att).(*computev1beta1.VolumeAttachment), nil
}

func (a attachments) AllVolumeAttachments(_ context.Context, req *computev1beta1.ListVolumeAttachmentsRequest) iter.Seq2[*computev1beta1.VolumeAttachment, error] {
	a.c.mu.Lock()
	prefix := req.GetParent() + "/volumeAttachments/"
	if zone, ok := strings.CutSuffix(req.GetParent(), "/instances/-"); ok {
		prefix = zone + "/instances/"
	}
	var page []*computev1beta1.VolumeAttachment
	for name, att := range a.c.attachments {
		if strings.HasPrefix(name, prefix) && (req.GetVolume() == "" || att.GetVolume() == req.GetVolume()) {
			page = append(page, proto.Clone(att).(*computev1beta1.VolumeAttachment))
		}
	}
	a.c.mu.Unlock()
	return func(yield func(*computev1beta1.VolumeAttachment, error) bool) {
		for _, att := range page {
			if !yield(att, nil) {
				return
			}
		}
	}
}

// pvsFullName is the full name of the PVS volume relative names.
func pvsFullName(relative string) string { return "//pvs." + core.Domain + "/" + relative }

func (a attachments) CreateVolumeAttachment(_ context.Context, req *computev1beta1.CreateVolumeAttachmentRequest) (cloud.Operation[*computev1beta1.VolumeAttachment], error) {
	c := a.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if prior, ok := c.requests[req.GetRequestId()]; ok && req.GetRequestId() != "" {
		return prior.(*op[*computev1beta1.VolumeAttachment]), nil
	}
	if err := c.change("CreateVolumeAttachment"); err != nil {
		return nil, err
	}
	if _, ok := c.instances[req.GetParent()]; !ok {
		return nil, notFound(req.GetParent())
	}
	full := req.GetVolumeAttachment().GetVolume()
	relative, ok := strings.CutPrefix(full, pvsFullName(""))
	if !ok {
		return nil, Error("BAD_REQUEST", "invalid input on field volumeAttachment.volume: not a full PVS volume name: %q", full)
	}
	vol, ok := c.volumes[relative]
	if !ok || vol.State == pvsv1beta1.Volume_DELETED {
		return nil, notFound(full)
	}
	if vol.State != pvsv1beta1.Volume_AVAILABLE {
		return nil, Error("CONFLICT", "volume %s is %s", relative, vol.State)
	}
	devices := map[string]bool{}
	for _, att := range c.attachments {
		if strings.HasPrefix(att.Name, req.GetParent()+"/") {
			devices[att.Device] = true
		}
	}
	device := ""
	for d := 'b'; d <= 'z'; d++ {
		if !devices["vd"+string(d)] {
			device = "vd" + string(d)
			break
		}
	}
	if device == "" {
		return nil, Error("CONFLICT", "instance %s has no free device", req.GetParent())
	}
	att := &computev1beta1.VolumeAttachment{
		Name:   req.GetParent() + "/volumeAttachments/" + c.id("att"),
		Volume: full,
		Device: device,
		State:  computev1beta1.VolumeAttachment_ATTACHING,
	}
	c.attachments[att.Name] = att
	vol.State = pvsv1beta1.Volume_IN_USE
	vol.Attachment = att.Name[strings.LastIndex(att.Name, "/")+1:]
	ready := proto.Clone(att).(*computev1beta1.VolumeAttachment)
	ready.State = computev1beta1.VolumeAttachment_ATTACHED
	o := &op[*computev1beta1.VolumeAttachment]{c: c, kind: "CreateVolumeAttachment", result: ready, done: func() {
		if cur, ok := c.attachments[ready.Name]; ok && cur.State == computev1beta1.VolumeAttachment_ATTACHING {
			cur.State = computev1beta1.VolumeAttachment_ATTACHED
		}
	}}
	if req.GetRequestId() != "" {
		c.requests[req.GetRequestId()] = o
	}
	return o, nil
}

func (a attachments) DeleteVolumeAttachment(_ context.Context, req *computev1beta1.DeleteVolumeAttachmentRequest) (cloud.Operation[*emptypb.Empty], error) {
	c := a.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.change("DeleteVolumeAttachment"); err != nil {
		return nil, err
	}
	att, ok := c.attachments[req.GetName()]
	if !ok {
		return nil, notFound(req.GetName())
	}
	if att.Boot {
		return nil, Error("CONFLICT", "the boot volume can't be detached")
	}
	att.State = computev1beta1.VolumeAttachment_DETACHING
	name := att.Name
	volume, _ := strings.CutPrefix(att.Volume, pvsFullName(""))
	return &op[*emptypb.Empty]{c: c, kind: "DeleteVolumeAttachment", result: &emptypb.Empty{}, done: func() {
		delete(c.attachments, name)
		if vol, ok := c.volumes[volume]; ok && vol.State == pvsv1beta1.Volume_IN_USE {
			vol.State = pvsv1beta1.Volume_AVAILABLE
			vol.Attachment = ""
		}
	}}, nil
}
