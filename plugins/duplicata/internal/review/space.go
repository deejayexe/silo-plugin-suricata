package review

import "golang.org/x/sys/unix"

// Estimates allocated file data released by an unlink, never filesystem free-space
// deltas (which include unrelated activity). Shared blocks, snapshots and open file
// descriptors can prevent immediate recovery even when the last hardlink is removed.
type SpaceEstimate struct {
	Bytes int64  `json:"bytes"`
	Basis string `json:"basis"`
}
type SpaceTotals struct {
	DeletedFiles   int   `json:"deleted_files"`
	EstimatedBytes int64 `json:"estimated_bytes"`
	UnknownFiles   int   `json:"unknown_files"`
}

func (f *heldFile) releasedSpaceEstimate() *SpaceEstimate {
	var stat unix.Stat_t
	if err := unix.Fstatat(f.parent, f.name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil || stamp(&stat) != f.stamp || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil
	}
	if stat.Nlink > 1 {
		return &SpaceEstimate{Bytes: 0, Basis: "hardlinks"}
	}
	if stat.Blocks < 0 || stat.Blocks > int64(^uint64(0)>>1)/512 {
		return nil
	}
	return &SpaceEstimate{Bytes: stat.Blocks * 512, Basis: "allocated_blocks"}
}
func deletionSpace(groups []Group) SpaceTotals {
	totals := SpaceTotals{}
	seen := map[string]bool{}
	for _, group := range groups {
		for _, v := range group.Versions {
			if !v.Deleted {
				continue
			}
			path := v.ResolvedPath
			if path == "" {
				path = v.Path
			}
			if seen[path] {
				continue
			}
			seen[path] = true
			totals.DeletedFiles++
			if v.SpaceReleased == nil {
				totals.UnknownFiles++
			} else {
				totals.EstimatedBytes += v.SpaceReleased.Bytes
			}
		}
	}
	return totals
}
