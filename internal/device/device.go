// Package device finds the block device a PVS volume is on the node, and
// its size.
//
// The guest's kernel names its virtio disks in the order it finds them, so
// a name such as vdb says nothing dependable about which volume a disk is,
// least of all for a disk attached to a running instance. The serial does:
// the platform gives every volume's disk the volume's id (`vol-...`) as its
// virtio serial, the kernel exposes it as /sys/block/vdX/serial, and udev's
// stock rules link the disk as /dev/disk/by-id/virtio-<volume id>.
package device

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotFound is returned when the node has no disk for the volume: it
// isn't attached here, or the guest doesn't see it yet.
var ErrNotFound = errors.New("no disk on this node carries the volume's serial")

// Finder finds devices under the node's /dev and /sys.
type Finder struct {
	// DevDir is where the device nodes are; /dev when empty.
	DevDir string
	// SysDir is where sysfs is mounted; /sys when empty.
	SysDir string
}

func (f Finder) dev() string { return or(f.DevDir, "/dev") }
func (f Finder) sys() string { return or(f.SysDir, "/sys") }

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Find returns the device of the volume whose id, the last segment of its
// name, is volumeID: the target of /dev/disk/by-id/virtio-<id>, else the
// disk whose sysfs serial is the id, for a node without udev's links. The
// path returned has its symlinks resolved, such as /dev/vdb, which is how
// the mount table names the device.
func (f Finder) Find(volumeID string) (string, error) {
	if volumeID == "" || strings.ContainsAny(volumeID, "/\x00") || strings.HasPrefix(volumeID, ".") {
		return "", fmt.Errorf("%q is not a volume id", volumeID)
	}
	link := filepath.Join(f.dev(), "disk", "by-id", "virtio-"+volumeID)
	if path, err := filepath.EvalSymlinks(link); err == nil {
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("resolving %s: %w", link, err)
	}

	block := filepath.Join(f.sys(), "block")
	entries, err := os.ReadDir(block)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("listing %s: %w", block, err)
	}
	for _, e := range entries {
		serial, err := os.ReadFile(filepath.Join(block, e.Name(), "serial"))
		if err != nil || strings.TrimSpace(string(serial)) != volumeID {
			continue
		}
		path := filepath.Join(f.dev(), e.Name())
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("disk %s carries the volume's serial, but %s: %w", e.Name(), path, err)
		}
		return path, nil
	}
	return "", ErrNotFound
}

// Size returns the size of device in bytes, as the guest sees it now: it
// grows once the platform expanded the volume.
func (Finder) Size(device string) (int64, error) {
	f, err := os.Open(device)
	if err != nil {
		return 0, fmt.Errorf("opening %s: %w", device, err)
	}
	defer func() { _ = f.Close() }()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, fmt.Errorf("reading the size of %s: %w", device, err)
	}
	return size, nil
}
