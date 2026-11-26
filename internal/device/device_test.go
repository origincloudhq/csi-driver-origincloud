package device

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// node lays out a node's /dev and /sys in a temporary directory: the disks
// it is given by name, with their serials, in the order the guest found
// them, and udev's by-id links when links is set.
func node(t *testing.T, links bool, disks map[string]string) Finder {
	t.Helper()
	root := t.TempDir()
	f := Finder{DevDir: filepath.Join(root, "dev"), SysDir: filepath.Join(root, "sys")}
	mkdir(t, filepath.Join(f.DevDir, "disk", "by-id"))
	for name, serial := range disks {
		write(t, filepath.Join(f.DevDir, name), "")
		write(t, filepath.Join(f.SysDir, "block", name, "serial"), serial+"\n")
		if links {
			if err := os.Symlink("../../"+name, filepath.Join(f.DevDir, "disk", "by-id", "virtio-"+serial)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return f
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A disk attached to a running instance often gets a name out of the
// order it was attached in; the serial finds it whatever its name.
func TestFindGoesBySerialNotByName(t *testing.T) {
	for _, links := range []bool{true, false} {
		f := node(t, links, map[string]string{"vdb": "vol-second", "vdd": "vol-first"})
		got, err := f.Find("vol-first")
		if err != nil {
			t.Fatalf("links %v: %v", links, err)
		}
		if want := filepath.Join(f.DevDir, "vdd"); got != want {
			t.Errorf("links %v: Find = %s, want %s", links, got, want)
		}
	}
}

func TestFindResolvesTheLinkToTheDevice(t *testing.T) {
	f := node(t, true, map[string]string{"vdb": "vol-a"})
	got, err := f.Find("vol-a")
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(got); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("Find = %s, want the device, not a link", got)
	}
}

func TestFindAVolumeThatIsNotAttached(t *testing.T) {
	f := node(t, true, map[string]string{"vdb": "vol-a"})
	if _, err := f.Find("vol-b"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Find = %v, want ErrNotFound", err)
	}
	empty := Finder{DevDir: t.TempDir(), SysDir: t.TempDir()}
	if _, err := empty.Find("vol-b"); !errors.Is(err, ErrNotFound) {
		t.Errorf("on a node without disks: Find = %v, want ErrNotFound", err)
	}
}

func TestFindRefusesWhatIsNoVolumeID(t *testing.T) {
	f := node(t, true, nil)
	for _, id := range []string{"", "..", "../vda", "a/b"} {
		if _, err := f.Find(id); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Find(%q) = %v, want a refusal", id, err)
		}
	}
}

func TestSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vdb")
	write(t, path, "0123456789")
	size, err := Finder{}.Size(path)
	if err != nil || size != 10 {
		t.Errorf("Size = %d, %v; want 10", size, err)
	}
}
