package driver

import (
	"strings"

	"github.com/google/uuid"
	"go.origincloud.eu/sdk/core"
)

// A volume's CSI volume ID is the PVS volume's relative name,
// `projects/{project}/zones/{zone}/volumes/{volume}`, and a node's CSI node
// ID is its instance's, `projects/{project}/zones/{zone}/instances/{instance}`.
// Both carry the zone, so the controller needs no lookup to know where a
// volume or a node is.

// zonal is a resource in a zone, parsed from its relative name.
type zonal struct {
	Project, Zone, ID string
}

// Parent is the zone, `projects/{project}/zones/{zone}`.
func (z zonal) Parent() string { return "projects/" + z.Project + "/zones/" + z.Zone }

// parseZonal parses `projects/{project}/zones/{zone}/{collection}/{id}`,
// and reports whether name has that form.
func parseZonal(name, collection string) (zonal, bool) {
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[2] != "zones" || parts[4] != collection {
		return zonal{}, false
	}
	for _, p := range []string{parts[1], parts[3], parts[5]} {
		if !segment(p) {
			return zonal{}, false
		}
	}
	return zonal{Project: parts[1], Zone: parts[3], ID: parts[5]}, true
}

// segment reports whether s may be one segment of a resource name.
func segment(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !segmentRune(r) {
			return false
		}
	}
	return true
}

func segmentRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
}

func parseVolumeID(id string) (zonal, bool) { return parseZonal(id, "volumes") }
func parseNodeID(id string) (zonal, bool)   { return parseZonal(id, "instances") }

// volumeName is the relative name of the volume z.
func volumeName(z zonal) string { return z.Parent() + "/volumes/" + z.ID }

// instanceName is the relative name of the instance z.
func instanceName(z zonal) string { return z.Parent() + "/instances/" + z.ID }

// pvsFullName is a PVS resource's full name, which a reference to it from
// another service takes.
func pvsFullName(relative string) string { return "//pvs." + core.Domain + "/" + relative }

// requestNamespace scopes the request ids the driver derives, so that they
// can't collide with ids another client derives the same way.
var requestNamespace = uuid.MustParse("8f0f6f43-36a4-5b55-9a0e-6f1d6c1f1c2a")

// createRequestID is the request id of the CreateVolume call for the CSI
// volume name in parent. It is the same for every retry, so a retry whose
// first attempt the API received returns that attempt's operation instead
// of creating a second volume.
func createRequestID(parent, name string) string {
	return uuid.NewSHA1(requestNamespace, []byte("CreateVolume\x00"+parent+"\x00"+name)).String()
}

// A snapshot's CSI snapshot ID is the PVS snapshot's relative name,
// `projects/{project}/zones/{zone}/snapshots/{snapshot}`: the zone a
// volume restored from it is created in.
func parseSnapshotID(id string) (zonal, bool) { return parseZonal(id, "snapshots") }

// snapshotRequestID is the request id of the CreateSnapshot call for the
// CSI snapshot name in parent, the same for every retry.
func snapshotRequestID(parent, name string) string {
	return uuid.NewSHA1(requestNamespace, []byte("CreateSnapshot\x00"+parent+"\x00"+name)).String()
}

// relativeOf is the relative name in a PVS full name, or "" when full
// isn't one.
func relativeOf(full string) string {
	relative, ok := strings.CutPrefix(full, pvsFullName(""))
	if !ok {
		return ""
	}
	return relative
}
