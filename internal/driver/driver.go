// Package driver implements the CSI services of the Origin CSI driver,
// pvs.csi.origincloud.eu: Identity; the controller plugin (Driver), which
// creates, grows and deletes PVS volumes, attaches them to and detaches
// them from the instances that are the cluster's nodes, and takes,
// restores from and deletes their snapshots; and the node plugin (Node),
// which formats, mounts and grows an attached volume on its node.
package driver

import (
	"fmt"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/origincloudhq/csi-driver-origincloud/internal/cloud"
)

const (
	// Name is the CSI driver's name.
	Name = "pvs.csi.origincloud.eu"

	// TopologyZoneKey is the topology segment that carries a node's or a
	// volume's zone.
	TopologyZoneKey = "topology.pvs.csi.origincloud.eu/zone"

	// ParameterStorageClass is the StorageClass parameter naming the PVS
	// storage class by its full name,
	// `//pvs.origincloudapis.eu/storageClasses/{storage_class}`. The
	// platform's default class when absent.
	ParameterStorageClass = "storageClass"

	// tagVolumeName is the tag a volume the driver created carries its CSI
	// volume name in. CreateVolume finds an earlier attempt's volume by it.
	tagVolumeName = "csi.origincloud.eu/volume-name"
	// tagCreatedBy marks the volumes the driver created.
	tagCreatedBy = "csi.origincloud.eu/created-by"
	// tagSnapshotName is the tag a snapshot the driver took carries its
	// CSI snapshot name in. CreateSnapshot finds an earlier attempt's
	// snapshot by it.
	tagSnapshotName = "csi.origincloud.eu/snapshot-name"
	// tagCreatedAt is the time, RFC 3339, the driver first asked for a
	// snapshot: its CSI creation time, which the API doesn't report.
	tagCreatedAt = "csi.origincloud.eu/created-at"
)

// Config configures the driver.
type Config struct {
	// Version is the driver's version, reported by GetPluginInfo.
	Version string
	// Cloud is the API the controller calls.
	Cloud cloud.Cloud
	// Project is the project the controller creates volumes in.
	Project string
	// DefaultZone is the zone a volume is created in when the CO sends no
	// topology requirement. Empty refuses such a request.
	DefaultZone string
	// PollInterval is how often the controller reads a resource another
	// call is changing, while it waits for it to settle. One second when 0.
	PollInterval time.Duration
}

// Driver serves the CSI Identity and Controller services.
type Driver struct {
	identity
	csi.UnimplementedControllerServer

	cfg Config
}

// New returns a driver for cfg.
func New(cfg Config) (*Driver, error) {
	if cfg.Project == "" || !segment(cfg.Project) {
		return nil, fmt.Errorf("the project must be a project id, got %q", cfg.Project)
	}
	if cfg.DefaultZone != "" && !segment(cfg.DefaultZone) {
		return nil, fmt.Errorf("the default zone must be a zone code, got %q", cfg.DefaultZone)
	}
	if cfg.Cloud.Volumes == nil || cfg.Cloud.Snapshots == nil || cfg.Cloud.Instances == nil || cfg.Cloud.Attachments == nil {
		return nil, fmt.Errorf("the driver needs a cloud")
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	return &Driver{identity: identity{version: cfg.Version}, cfg: cfg}, nil
}
