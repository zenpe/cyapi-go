package sources

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReadVerifiedScriptsReleasesSnapshotAndRejectsTampering(t *testing.T) {
	manager, _ := fixture(t, archive(t, zipEntry{name: "music.js", body: "original"}, zipEntry{name: "readme.txt", body: "ignored"}))
	if _, e := manager.Sync(context.Background(), false, false); e != nil {
		t.Fatal(e)
	}
	gen, items, e := manager.ReadVerifiedScripts(context.Background(), "", 1024, 4096)
	if e != nil || len(items) != 1 {
		t.Fatal(items, e)
	}
	if string(items[0].Content) != "original" {
		t.Fatal(items)
	}
	f, e := os.OpenFile(filepath.Join(manager.o.Directory, "versions", gen, ".read.lock"), os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if e = lockFile(f, true); e != nil {
		t.Fatal("read lease leaked", e)
	}
	unlockFile(f)
	f.Close()
	if e = os.WriteFile(filepath.Join(manager.o.Directory, "versions", gen, "files", "music.js"), []byte("tampered"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, _, e = manager.ReadVerifiedScripts(context.Background(), "", 1024, 4096); e == nil {
		t.Fatal("same-size corruption not detected")
	}
}
