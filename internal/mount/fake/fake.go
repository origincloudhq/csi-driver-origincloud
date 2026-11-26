// Package fake is an in-memory mount.Mounter for unit tests and the
// csi-sanity suite. It keeps what the node plugin relies on: formatting
// only a device that holds nothing, a mount table in which a bind mount
// carries the device of what it binds, and unmounting the latest mount at
// a path. It touches no file and no device.
package fake

import (
	"fmt"
	"path/filepath"
	"slices"
	"sync"

	"github.com/origincloudhq/csi-driver-origincloud/internal/mount"
)

// Mounter is the fake. The zero value is not usable; call New.
type Mounter struct {
	mu sync.Mutex
	// holds maps a device to what it holds; absent is nothing at all.
	holds  map[string]string
	mounts []mount.Mount

	// Formats counts the times each device was formatted.
	Formats map[string]int
	// Resizes counts the times each device's filesystem was grown.
	Resizes map[string]int
	// Usage is what Stats answers for every mounted path.
	Usage mount.Stats
}

// New returns a fake with no mounts, whose devices hold nothing.
func New() *Mounter {
	return &Mounter{
		holds:   map[string]string{},
		Formats: map[string]int{},
		Resizes: map[string]int{},
		Usage: mount.Stats{
			TotalBytes: 10 << 30, AvailableBytes: 9 << 30, UsedBytes: 1 << 30,
			TotalInodes: 1000, AvailableInodes: 990, UsedInodes: 10,
		},
	}
}

// SetFilesystem makes device hold what, such as ext4 or "partition table".
func (m *Mounter) SetFilesystem(device, what string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.holds[device] = what
}

// Filesystem returns what device holds.
func (m *Mounter) Filesystem(device string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.holds[device], nil
}

// FormatAndMount formats device only when it holds nothing, then mounts it.
func (m *Mounter) FormatAndMount(device, target, fsType string, options []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.holds[device] == "" {
		m.holds[device] = fsType
		m.Formats[device]++
	}
	m.mounts = append(m.mounts, mount.Mount{Path: filepath.Clean(target), Device: device, Options: slices.Clone(options)})
	return nil
}

// Mount mounts source at target. A bind mount of a mounted path carries
// that mount's device; any other source is taken for the device.
func (m *Mounter) Mount(source, target, _ string, options []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	device := source
	if slices.Contains(options, "bind") {
		if at, ok := m.latest(source); ok {
			device = at.Device
		}
	}
	m.mounts = append(m.mounts, mount.Mount{Path: filepath.Clean(target), Device: device, Options: slices.Clone(options)})
	return nil
}

// Unmount removes the latest mount at target.
func (m *Mounter) Unmount(target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	target = filepath.Clean(target)
	for i := len(m.mounts) - 1; i >= 0; i-- {
		if m.mounts[i].Path == target {
			m.mounts = slices.Delete(m.mounts, i, i+1)
			return nil
		}
	}
	return fmt.Errorf("umount %s: not mounted", target)
}

// List returns the mount table.
func (m *Mounter) List() ([]mount.Mount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.mounts), nil
}

// Resize counts the resize of device's filesystem.
func (m *Mounter) Resize(device, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.latest(path); !ok {
		return fmt.Errorf("%s is not mounted", path)
	}
	m.Resizes[device]++
	return nil
}

// Stats answers Usage for a mounted path.
func (m *Mounter) Stats(path string) (mount.Stats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.latest(path); !ok {
		return mount.Stats{}, fmt.Errorf("%s is not mounted", path)
	}
	return m.Usage, nil
}

func (m *Mounter) latest(path string) (mount.Mount, bool) {
	path = filepath.Clean(path)
	for i := len(m.mounts) - 1; i >= 0; i-- {
		if m.mounts[i].Path == path {
			return m.mounts[i], true
		}
	}
	return mount.Mount{}, false
}
