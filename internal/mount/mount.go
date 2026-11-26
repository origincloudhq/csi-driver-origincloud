// Package mount is the node plugin's view of the node's filesystems: what
// a device holds, formatting and mounting it, bind mounts, the mount table,
// growing a filesystem and its usage. The node plugin depends on the
// Mounter interface; New returns its implementation on Linux, over
// k8s.io/mount-utils and the node's blkid, mkfs, fsck and resize tools.
package mount

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
	mountutils "k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"
)

// Mount is one entry of the node's mount table.
type Mount struct {
	// Path is where it is mounted.
	Path string
	// Device is the block device it comes from, such as /dev/vdb: the
	// device of a filesystem, and for a bind mount the device of what was
	// bound, a directory on its filesystem or the device itself. Empty for
	// a mount of no block device (tmpfs, proc, ...).
	Device string
	// Options are the mount's options, such as ro.
	Options []string
}

// Stats is the usage of a mounted filesystem.
type Stats struct {
	TotalBytes, AvailableBytes, UsedBytes    int64
	TotalInodes, AvailableInodes, UsedInodes int64
}

// Mounter does what the node plugin does to the node's filesystems.
type Mounter interface {
	// Filesystem returns what device holds: the type of its filesystem
	// (ext4, xfs, ...), or "" when it holds nothing at all, neither a
	// filesystem nor a partition table. Any other answer means data.
	Filesystem(device string) (string, error)
	// FormatAndMount mounts device at target as fsType, first creating an
	// fsType filesystem on it only if it holds nothing at all. A device
	// that holds anything is never formatted.
	FormatAndMount(device, target, fsType string, options []string) error
	// Mount mounts source at target. With the option bind, source is a
	// directory or a device bound to target; ro then makes the bind
	// read-only.
	Mount(source, target, fsType string, options []string) error
	// Unmount unmounts what is mounted at target, the latest mount there.
	Unmount(target string) error
	// List lists the mount table.
	List() ([]Mount, error)
	// Resize grows the filesystem on device, mounted at path, to fill it.
	Resize(device, path string) error
	// Stats returns the usage of the filesystem mounted at path.
	Stats(path string) (Stats, error)
}

// partitioned is what mount-utils' blkid reading answers for a device
// that holds a partition table.
const partitioned = "unknown data, probably partitions"

// Linux is the Mounter of the node the plugin runs on.
type Linux struct {
	sfm       *mountutils.SafeFormatAndMount
	exec      utilexec.Interface
	mountInfo string
}

// New returns the node's Mounter.
func New() *Linux {
	exec := utilexec.New()
	return newLinux(mountutils.New(""), exec, "/proc/self/mountinfo")
}

func newLinux(m mountutils.Interface, exec utilexec.Interface, mountInfo string) *Linux {
	return &Linux{sfm: mountutils.NewSafeFormatAndMount(m, exec), exec: exec, mountInfo: mountInfo}
}

// Filesystem reads what device holds with blkid, probing the device
// itself rather than trusting a cache.
func (l *Linux) Filesystem(device string) (string, error) {
	fs, err := l.sfm.GetDiskFormat(device)
	if err != nil {
		return "", fmt.Errorf("reading what %s holds: %w", device, err)
	}
	if fs == partitioned {
		return "partition table", nil
	}
	return fs, nil
}

// FormatAndMount formats device only when blkid finds neither a
// filesystem nor a partition table on it, then mounts it. A device that
// holds a filesystem is checked with fsck first, as mount-utils does.
func (l *Linux) FormatAndMount(device, target, fsType string, options []string) error {
	return l.sfm.FormatAndMount(device, target, fsType, options)
}

// Mount mounts source at target.
func (l *Linux) Mount(source, target, fsType string, options []string) error {
	return l.sfm.Mount(source, target, fsType, options)
}

// Unmount unmounts target.
func (l *Linux) Unmount(target string) error {
	return l.sfm.Unmount(target)
}

// List reads the mount table from mountinfo, which, unlike /proc/mounts,
// tells a bind mount of a device apart: its source is the filesystem /dev
// lives on (devtmpfs) and its root the device's path there.
func (l *Linux) List() ([]Mount, error) {
	infos, err := mountutils.ParseMountInfo(l.mountInfo)
	if err != nil {
		return nil, fmt.Errorf("reading the mount table: %w", err)
	}
	return mountsOf(infos), nil
}

func mountsOf(infos []mountutils.MountInfo) []Mount {
	out := make([]Mount, 0, len(infos))
	for _, mi := range infos {
		m := Mount{Path: mi.MountPoint, Options: mi.MountOptions}
		switch {
		case mi.FsType == "devtmpfs" && mi.Root != "/":
			m.Device = filepath.Join("/dev", mi.Root)
		case strings.HasPrefix(mi.Source, "/dev/"):
			m.Device = mi.Source
		}
		out = append(out, m)
	}
	return out
}

// Resize grows the filesystem with resize2fs or xfs_growfs, which
// mount-utils picks by what blkid reads.
func (l *Linux) Resize(device, path string) error {
	if _, err := mountutils.NewResizeFs(l.exec).Resize(device, path); err != nil {
		return fmt.Errorf("growing the filesystem on %s: %w", device, err)
	}
	return nil
}

// Stats reads the usage with statfs.
func (l *Linux) Stats(path string) (Stats, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(path, &s); err != nil {
		return Stats{}, fmt.Errorf("reading the usage of %s: %w", path, err)
	}
	bsize := s.Bsize
	return Stats{
		TotalBytes:      int64(s.Blocks) * bsize,
		AvailableBytes:  int64(s.Bavail) * bsize,
		UsedBytes:       int64(s.Blocks-s.Bfree) * bsize,
		TotalInodes:     int64(s.Files),
		AvailableInodes: int64(s.Ffree),
		UsedInodes:      int64(s.Files - s.Ffree),
	}, nil
}
