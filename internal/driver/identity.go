package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// identity is the CSI Identity service, which both plugins serve.
type identity struct {
	csi.UnimplementedIdentityServer

	version string
}

// GetPluginInfo names the driver.
func (i identity) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: Name, VendorVersion: i.version}, nil
}

// GetPluginCapabilities says the driver has a controller service, that
// its volumes are reachable only from nodes in their zone, and that a
// volume grows while it is attached. The node plugin answers the same:
// the capabilities are the driver's, whichever plugin is asked.
func (identity) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	service := func(t csi.PluginCapability_Service_Type) *csi.PluginCapability {
		return &csi.PluginCapability{Type: &csi.PluginCapability_Service_{
			Service: &csi.PluginCapability_Service{Type: t},
		}}
	}
	return &csi.GetPluginCapabilitiesResponse{Capabilities: []*csi.PluginCapability{
		service(csi.PluginCapability_Service_CONTROLLER_SERVICE),
		service(csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS),
		{Type: &csi.PluginCapability_VolumeExpansion_{
			VolumeExpansion: &csi.PluginCapability_VolumeExpansion{Type: csi.PluginCapability_VolumeExpansion_ONLINE},
		}},
	}}, nil
}

// Probe answers ready: neither plugin holds state to warm up, and a
// failing API or node shows in the calls themselves.
func (identity) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
