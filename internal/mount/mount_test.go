package mount

import (
	"slices"
	"testing"

	mountutils "k8s.io/mount-utils"
	"k8s.io/utils/exec"
	exectesting "k8s.io/utils/exec/testing"
)

// node is a fake of the node's tools: it answers blkid with what the
// device holds and records every command it is asked to run.
type node struct {
	holds string // blkid's export output; "" is a device that holds nothing
	ran   []string
}

func (n *node) exec() *exectesting.FakeExec {
	run := func(cmd string, args ...string) exec.Cmd {
		n.ran = append(n.ran, cmd)
		answer := func() ([]byte, []byte, error) {
			if cmd == "blkid" && n.holds == "" {
				return nil, nil, &exectesting.FakeExitError{Status: 2}
			}
			if cmd == "blkid" {
				return []byte(n.holds), nil, nil
			}
			return nil, nil, nil
		}
		return exectesting.InitFakeCmd(&exectesting.FakeCmd{
			CombinedOutputScript: []exectesting.FakeAction{answer},
		}, cmd, args...)
	}
	fe := &exectesting.FakeExec{}
	for range 8 {
		fe.CommandScript = append(fe.CommandScript, run)
	}
	return fe
}

func formatAndMount(t *testing.T, holds, fsType string) (*node, *mountutils.FakeMounter) {
	t.Helper()
	n := &node{holds: holds}
	m := mountutils.NewFakeMounter(nil)
	l := newLinux(m, n.exec(), "")
	if err := l.FormatAndMount("/dev/vdb", "/staging", fsType, nil); err != nil {
		t.Fatalf("FormatAndMount: %v", err)
	}
	return n, m
}

func TestFormatAndMountFormatsADeviceThatHoldsNothing(t *testing.T) {
	n, m := formatAndMount(t, "", "xfs")
	if !slices.Contains(n.ran, "mkfs.xfs") {
		t.Errorf("ran %v, want mkfs.xfs", n.ran)
	}
	if len(m.MountPoints) != 1 || m.MountPoints[0].Path != "/staging" {
		t.Errorf("mounts %v, want one at /staging", m.MountPoints)
	}
}

func TestFormatAndMountNeverFormatsADeviceThatHoldsSomething(t *testing.T) {
	for name, holds := range map[string]string{
		"the same filesystem":    "TYPE=ext4\n",
		"another filesystem":     "TYPE=xfs\n",
		"a partition table":      "PTTYPE=gpt\n",
		"a filesystem in a part": "TYPE=ext4\nPTTYPE=dos\n",
	} {
		t.Run(name, func(t *testing.T) {
			n := &node{holds: holds}
			l := newLinux(mountutils.NewFakeMounter(nil), n.exec(), "")
			// A mismatch fails to mount on a real node; formatting is what
			// must never happen.
			_ = l.FormatAndMount("/dev/vdb", "/staging", "ext4", nil)
			for _, cmd := range n.ran {
				if cmd == "mkfs.ext4" || cmd == "mkfs.xfs" {
					t.Fatalf("ran %v on a device that holds %q", n.ran, holds)
				}
			}
		})
	}
}

func TestFilesystem(t *testing.T) {
	for holds, want := range map[string]string{
		"":             "",
		"TYPE=ext4\n":  "ext4",
		"TYPE=xfs\n":   "xfs",
		"PTTYPE=gpt\n": "partition table",
	} {
		n := &node{holds: holds}
		got, err := newLinux(mountutils.NewFakeMounter(nil), n.exec(), "").Filesystem("/dev/vdb")
		if err != nil || got != want {
			t.Errorf("blkid %q: Filesystem = %q, %v; want %q", holds, got, err, want)
		}
	}
}

func TestMountsOfNamesTheDeviceOfABindMount(t *testing.T) {
	got := mountsOf([]mountutils.MountInfo{
		{MountPoint: "/staging", Source: "/dev/vdb", FsType: "ext4", Root: "/"},
		{MountPoint: "/pods/a", Source: "/dev/vdb", FsType: "ext4", Root: "/"},
		{MountPoint: "/pods/block", Source: "udev", FsType: "devtmpfs", Root: "/vdc"},
		{MountPoint: "/dev", Source: "udev", FsType: "devtmpfs", Root: "/"},
		{MountPoint: "/tmp", Source: "tmpfs", FsType: "tmpfs", Root: "/"},
	})
	want := []string{"/dev/vdb", "/dev/vdb", "/dev/vdc", "", ""}
	for i, m := range got {
		if m.Device != want[i] {
			t.Errorf("%s: device %q, want %q", m.Path, m.Device, want[i])
		}
	}
}
