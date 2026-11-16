package review

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReleasedSpaceUsesAllocatedBlocksAndHardlinks(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "media")
	if err := os.WriteFile(path, make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := holdFile(path, root)
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	estimate := f.releasedSpaceEstimate()
	if estimate == nil || estimate.Bytes <= 0 || estimate.Basis != "allocated_blocks" {
		t.Fatalf("estimate: %+v", estimate)
	}
	if err := os.Link(path, filepath.Join(root, "other")); err != nil {
		t.Fatal(err)
	}
	estimate = f.releasedSpaceEstimate()
	if estimate == nil || estimate.Bytes != 0 || estimate.Basis != "hardlinks" {
		t.Fatalf("hardlink estimate: %+v", estimate)
	}
}
func TestReleasedSpaceDoesNotClaimSparseLogicalSize(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sparse")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	file.Close()
	f, err := holdFile(path, root)
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	estimate := f.releasedSpaceEstimate()
	if estimate == nil || estimate.Bytes >= 1<<30 {
		t.Fatalf("sparse logical size reported: %+v", estimate)
	}
	os.Remove(path)
	if f.releasedSpaceEstimate() != nil {
		t.Fatal("missing file has a known estimate")
	}
}
func TestDeletionSpaceTotalsDeduplicateAndPreserveUnknown(t *testing.T) {
	known := Version{Path: "/alias", ResolvedPath: "/real", Deleted: true, SpaceReleased: &SpaceEstimate{Bytes: 4096, Basis: "allocated_blocks"}}
	groups := []Group{{Versions: []Version{known, {Path: "/unknown", Deleted: true}}}, {Versions: []Version{known, {Path: "/hardlink", Deleted: true, SpaceReleased: &SpaceEstimate{Basis: "hardlinks"}}}}}
	totals := deletionSpace(groups)
	if totals.DeletedFiles != 3 || totals.EstimatedBytes != 4096 || totals.UnknownFiles != 1 {
		t.Fatalf("totals: %+v", totals)
	}
	h := NewHandler()
	h.scan = Scan{ID: "old", Status: "complete", Groups: groups}
	h.archiveScanLocked()
	if summary(h.history[0].Scan, false).Space != totals {
		t.Fatal("space totals missing from history")
	}
}
