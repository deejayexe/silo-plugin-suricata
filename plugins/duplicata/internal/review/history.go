package review

import (
	"encoding/json"
	"errors"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

const maxHistoryEntries = 10
const maxHistoryBytes = 64 * 1024 * 1024

type historyEntry struct {
	Scan  Scan
	Bytes int
}
type analysisSummary struct {
	ID          string      `json:"id"`
	LibraryName string      `json:"library_name"`
	Status      string      `json:"status"`
	StartedAt   time.Time   `json:"started_at"`
	FinishedAt  time.Time   `json:"finished_at"`
	Processed   int         `json:"processed"`
	Total       int         `json:"total"`
	Groups      int         `json:"group_count"`
	Deleted     int         `json:"deleted_count"`
	Current     bool        `json:"current"`
	Space       SpaceTotals `json:"space"`
}

func queryString(req *pluginv1.HandleHTTPRequest, key string) string {
	if req.GetQuery() != nil {
		if v := req.GetQuery().GetFields()[key]; v != nil {
			return v.GetStringValue()
		}
	}
	return ""
}

// Call with h.mu held. Ownership transfers to history when the active scan is replaced.
// Archived results are read-only: deletion still requires the current complete scan.
func (h *Handler) archiveScanLocked() {
	if h.scan.ID == "" {
		return
	}
	scan := h.scan
	if scan.Status == "running" {
		scan.Status = "cancelled"
		scan.FinishedAt = time.Now().UTC()
	}
	data, err := json.Marshal(scan)
	if err != nil || len(data) > maxHistoryBytes {
		return
	}
	h.history = append([]historyEntry{{Scan: scan, Bytes: len(data)}}, h.history...)
	total := 0
	keep := 0
	for _, entry := range h.history {
		if keep == maxHistoryEntries || total+entry.Bytes > maxHistoryBytes {
			break
		}
		total += entry.Bytes
		keep++
	}
	// Copy the retained slice so evicted entries can be collected.
	h.history = append([]historyEntry(nil), h.history[:keep]...)
}
func (h *Handler) selectedScanLocked(id string) *Scan {
	if id == "" || id == h.scan.ID {
		return &h.scan
	}
	for i := range h.history {
		if h.history[i].Scan.ID == id {
			return &h.history[i].Scan
		}
	}
	return nil
}
func summary(scan Scan, current bool) analysisSummary {
	deleted := map[string]bool{}
	for _, group := range scan.Groups {
		for _, v := range group.Versions {
			if v.Deleted {
				path := v.ResolvedPath
				if path == "" {
					path = v.Path
				}
				deleted[path] = true
			}
		}
	}
	name := scan.LibraryName
	if name == "" {
		name = scan.Library
	}
	return analysisSummary{scan.ID, name, scan.Status, scan.StartedAt, scan.FinishedAt, scan.Processed, scan.Total, len(scan.Groups), len(deleted), current, deletionSpace(scan.Groups)}
}
func (h *Handler) historyList() *pluginv1.HandleHTTPResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	items := []analysisSummary{}
	if h.scan.ID != "" {
		items = append(items, summary(h.scan, true))
	}
	for _, entry := range h.history {
		items = append(items, summary(entry.Scan, false))
	}
	return jsonResponse(200, map[string]any{"items": items, "max_entries": maxHistoryEntries, "max_bytes": maxHistoryBytes})
}
func (h *Handler) removeHistory(req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	var in struct {
		ID string `json:"scan_id"`
	}
	if json.Unmarshal(req.Body, &in) != nil || in.ID == "" {
		return failure(400, errors.New("Select an analysis"))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scan.ID == in.ID {
		return failure(409, errors.New("Use Save to history to move the current analysis to history"))
	}
	for i, entry := range h.history {
		if entry.Scan.ID == in.ID {
			copy(h.history[i:], h.history[i+1:])
			h.history[len(h.history)-1] = historyEntry{}
			h.history = h.history[:len(h.history)-1]
			return jsonResponse(200, map[string]bool{"removed": true})
		}
	}
	return failure(404, errors.New("Analysis is no longer in memory"))
}

// Restore an analysis for review without changing its captured fingerprints or
// completeness. Deletion revalidates live membership and both files as usual.
func (h *Handler) restoreHistory(req *pluginv1.HandleHTTPRequest) *pluginv1.HandleHTTPResponse {
	var in struct {
		ID string `json:"scan_id"`
	}
	if json.Unmarshal(req.Body, &in) != nil || in.ID == "" {
		return failure(400, errors.New("Select an analysis"))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scan.Status == "running" {
		return failure(409, errors.New("Stop the running scan before continuing a previous analysis"))
	}
	if h.scan.ID == in.ID {
		h.pending = deletePlan{}
		return jsonResponse(200, map[string]string{"scan_id": h.scan.ID})
	}
	for i, entry := range h.history {
		if entry.Scan.ID == in.ID {
			selected := entry.Scan
			copy(h.history[i:], h.history[i+1:])
			h.history[len(h.history)-1] = historyEntry{}
			h.history = h.history[:len(h.history)-1]
			h.archiveScanLocked()
			h.scan = selected
			h.pending = deletePlan{}
			return jsonResponse(200, map[string]string{"scan_id": h.scan.ID})
		}
	}
	return failure(404, errors.New("Analysis is no longer in memory"))
}
