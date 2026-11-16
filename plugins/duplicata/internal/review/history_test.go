package review

import (
	"context"
	"encoding/json"
	"fmt"
	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func historyRequest(h *Handler, method, path string, query map[string]any, body map[string]string) *pluginv1.HandleHTTPResponse {
	req := &pluginv1.HandleHTTPRequest{Method: method, Path: path, Headers: map[string]string{"X-Silo-User-Role": "admin", "X-Silo-User-Id": "admin", "Content-Type": "application/json"}}
	if query != nil {
		req.Query, _ = structpb.NewStruct(query)
	}
	if body != nil {
		req.Body, _ = json.Marshal(body)
	}
	res, _ := h.Handle(context.Background(), req)
	return res
}
func TestHistoryPreservesResultsAndPagesAfterClear(t *testing.T) {
	h := NewHandler()
	groups := make([]Group, 25)
	for i := range groups {
		groups[i] = Group{ID: fmt.Sprint(i), Versions: []Version{{ID: "deleted", Path: "/file", Deleted: true, DeletedAt: "2026-11-15T10:00:00Z", KeptPath: "/retained"}}}
	}
	h.scan = Scan{ID: "previous", Library: "lib", LibraryName: "Movies", Status: "complete", Groups: groups, StartedAt: time.Now(), FinishedAt: time.Now(), Total: 100000, Processed: 100000}
	res := historyRequest(h, "POST", "/clear", nil, map[string]string{"csrf": h.csrf})
	if res.StatusCode != 200 || h.scan.ID != "" {
		t.Fatalf("clear: %s", res.Body)
	}
	res = historyRequest(h, "GET", "/history", nil, nil)
	var history struct {
		Items []analysisSummary `json:"items"`
	}
	json.Unmarshal(res.Body, &history)
	if len(history.Items) != 1 || history.Items[0].LibraryName != "Movies" || history.Items[0].Deleted != 1 || history.Items[0].Groups != 25 || history.Items[0].Current {
		t.Fatalf("history: %s", res.Body)
	}
	res = historyRequest(h, "GET", "/results", map[string]any{"scan_id": "previous", "offset": "20"}, nil)
	var result struct {
		Groups     []Group `json:"groups"`
		Historical bool    `json:"historical"`
		Total      int     `json:"total"`
	}
	json.Unmarshal(res.Body, &result)
	if res.StatusCode != 200 || !result.Historical || len(result.Groups) != 5 || result.Total != 25 || result.Groups[0].Versions[0].KeptPath != "/retained" {
		t.Fatalf("results: %s", res.Body)
	}
	fresh := NewHandler()
	if len(fresh.history) != 0 {
		t.Fatal("history survived a new plugin instance")
	}
	if historyRequest(fresh, "GET", "/results", map[string]any{"scan_id": "previous"}, nil).StatusCode != 404 {
		t.Fatal("history survived restart")
	}
}
func TestHistorySurvivesConfigurationAndArchivesStoppedScan(t *testing.T) {
	h := NewHandler()
	h.scan = Scan{ID: "running", Library: "lib", Status: "running", Groups: []Group{}}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.Configure(DefaultConfig())
	if ctx.Err() == nil || len(h.history) != 1 || h.history[0].Scan.Status != "cancelled" || h.history[0].Scan.FinishedAt.IsZero() {
		t.Fatal("running analysis not archived on reconfiguration")
	}
}
func TestHistoryRetentionAndRemovalSecurity(t *testing.T) {
	h := NewHandler()
	for i := 0; i < 12; i++ {
		h.scan = Scan{ID: fmt.Sprint(i), Status: "complete"}
		h.archiveScanLocked()
	}
	h.scan = Scan{Status: "idle"}
	if len(h.history) != 10 || h.selectedScanLocked("0") != nil || h.selectedScanLocked("11") == nil {
		t.Fatal("history retention failed")
	}
	res := historyRequest(h, "POST", "/history/remove", nil, map[string]string{"scan_id": "11"})
	if res.StatusCode != 403 {
		t.Fatal("removal without CSRF accepted")
	}
	res = historyRequest(h, "POST", "/history/remove", nil, map[string]string{"scan_id": "11", "csrf": h.csrf})
	if res.StatusCode != 200 || h.selectedScanLocked("11") != nil {
		t.Fatal("record not removed")
	}
	req := &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/history"}
	res, _ = h.Handle(context.Background(), req)
	if res.StatusCode != 403 {
		t.Fatal("history accessible without administrator access")
	}
}
func TestHistoryEvictsEntriesBeyondMemoryBudget(t *testing.T) {
	h := NewHandler()
	h.history = []historyEntry{{Scan: Scan{ID: "large"}, Bytes: maxHistoryBytes}}
	h.scan = Scan{ID: "small", Status: "complete"}
	h.archiveScanLocked()
	if len(h.history) != 1 || h.history[0].Scan.ID != "small" {
		t.Fatal("memory budget exceeded")
	}
}

func TestStartingNewScanArchivesPreviousAndFinishes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/user/libraries":
			fmt.Fprint(w, `{"items":[{"id":"lib","name":"Series"}]}`)
		case "/api/v2/catalog":
			fmt.Fprint(w, `{"items":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := DefaultConfig()
	cfg.APIKey = "test"
	client, err := NewClient(server.URL, cfg)
	if err != nil {
		t.Fatal(err)
	}
	client.interval = 0
	h := NewHandler()
	h.cfg = cfg
	h.client = client
	h.scan = Scan{ID: "old", Library: "lib", Status: "complete", Groups: []Group{{ID: "old-group"}}}
	h.pending = deletePlan{Token: "stale-confirmation"}
	res := historyRequest(h, "POST", "/scan", nil, map[string]string{"library_id": "lib", "csrf": h.csrf})
	if res.StatusCode != 202 {
		t.Fatalf("start: %s", res.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		status := h.scan.Status
		finished := h.scan.FinishedAt
		name := h.scan.LibraryName
		old := h.selectedScanLocked("old")
		archived := old != nil && len(old.Groups) == 1
		pending := h.pending.Token
		h.mu.Unlock()
		if !archived || pending != "" {
			t.Fatal("previous results or pending confirmation not handled")
		}
		if status == "complete" {
			if finished.IsZero() || name != "Series" {
				t.Fatal("missing analysis metadata")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scan did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRestoreHistorySwitchesCurrentWithoutRescanningOrDuplicating(t *testing.T) {
	h := NewHandler()
	h.scan = Scan{ID: "saved", Status: "complete", Groups: []Group{{ID: "group", Versions: []Version{{ID: "deleted", Deleted: true, Snapshot: fingerprint{Ino: 42}, SpaceReleased: &SpaceEstimate{Bytes: 4096}}}}}}
	h.archiveScanLocked()
	h.scan = Scan{ID: "current", Status: "complete"}
	h.pending = deletePlan{Token: "old"}
	restore := func() *pluginv1.HandleHTTPResponse {
		return historyRequest(h, "POST", "/history/restore", nil, map[string]string{"scan_id": "saved", "csrf": h.csrf})
	}
	if res := restore(); res.StatusCode != 200 {
		t.Fatalf("restore: %s", res.Body)
	}
	if h.scan.ID != "saved" || h.scan.Status != "complete" || h.scan.Groups[0].Versions[0].Snapshot.Ino != 42 || !h.scan.Groups[0].Versions[0].Deleted || h.pending.Token != "" {
		t.Fatal("saved state changed or confirmation not cleared")
	}
	if len(h.history) != 1 || h.history[0].Scan.ID != "current" {
		t.Fatal("previous current analysis not archived correctly")
	}
	if res := restore(); res.StatusCode != 200 || len(h.history) != 1 {
		t.Fatal("repeated restore duplicated history")
	}
}
func TestRestoreHistorySecurityAndRunningScan(t *testing.T) {
	h := NewHandler()
	h.scan = Scan{ID: "saved", Status: "partial"}
	h.archiveScanLocked()
	h.scan = Scan{ID: "running", Status: "running"}
	body := map[string]string{"scan_id": "saved", "csrf": h.csrf}
	if res := historyRequest(h, "POST", "/history/restore", nil, map[string]string{"scan_id": "saved"}); res.StatusCode != 403 {
		t.Fatal("restore without CSRF accepted")
	}
	req := &pluginv1.HandleHTTPRequest{Method: "POST", Path: "/history/restore"}
	res, _ := h.Handle(context.Background(), req)
	if res.StatusCode != 403 {
		t.Fatal("non-admin restore accepted")
	}
	if res := historyRequest(h, "POST", "/history/restore", nil, body); res.StatusCode != 409 || h.scan.ID != "running" || len(h.history) != 1 {
		t.Fatal("running scan replaced")
	}
	h.scan.Status = "complete"
	if res := historyRequest(h, "POST", "/history/restore", nil, body); res.StatusCode != 200 || h.scan.Status != "partial" {
		t.Fatal("incomplete analysis changed on restore")
	}
	if res := historyRequest(h, "POST", "/history/restore", nil, map[string]string{"scan_id": "missing", "csrf": h.csrf}); res.StatusCode != 404 {
		t.Fatal("missing analysis restored")
	}
}
