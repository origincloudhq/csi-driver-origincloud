package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// GetPluginInfo names the driver.
func (d *Driver) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: Name, VendorVersion: d.cfg.Version}, nil
}

// GetPluginCapabilities says the driver has a controller service and that
// its volumes are reachable only from nodes in their zone.
func (d *Driver) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	service := func(t csi.PluginCapability_Service_Type) *csi.PluginCapability {
		return &csi.PluginCapability{Type: &csi.PluginCapability_Service_{
			Service: &csi.PluginCapability_Service{Type: t},
		}}
	}
	return &csi.GetPluginCapabilitiesResponse{Capabilities: []*csi.PluginCapability{
		service(csi.PluginCapability_Service_CONTROLLER_SERVICE),
		service(csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS),
	}}, nil
}

// Probe answers ready: the controller holds no state to warm up, and a
// failing API shows in the calls themselves.
func (d *Driver) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
