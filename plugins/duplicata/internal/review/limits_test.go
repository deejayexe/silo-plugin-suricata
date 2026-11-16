package review

import (
	"context"
	"encoding/json"
	"fmt"
	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDefaultLimitsAndValidation(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.RequestsPerSecond != 100 || cfg.Concurrency != 16 || cfg.DeleteEnabled {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if scanTimeout != 2*time.Hour {
		t.Fatalf("scan timeout: %v", scanTimeout)
	}
	cfg.APIKey = "test"
	for _, rate := range []float64{1, 100} {
		for _, workers := range []int{1, 16} {
			cfg.RequestsPerSecond, cfg.Concurrency = rate, workers
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, rate := range []float64{0, 101, math.NaN(), math.Inf(1), math.Inf(-1)} {
		cfg.RequestsPerSecond = rate
		if cfg.Validate() == nil {
			t.Fatalf("invalid rate accepted: %v", rate)
		}
	}
	cfg.RequestsPerSecond = 100
	for _, workers := range []int{0, 17} {
		cfg.Concurrency = workers
		if cfg.Validate() == nil {
			t.Fatalf("invalid concurrency: %d", workers)
		}
	}
}

func TestCatalogPaginationLimits(t *testing.T) {
	for _, kind := range []string{"movie", "episode"} {
		for _, total := range []int{100000, 100001} {
			t.Run(fmt.Sprintf("%s/%d", kind, total), func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					q := r.URL.Query()
					if r.URL.Path != "/api/v2/catalog" || q.Get("library_id") != "lib" || q.Get("type") != kind || q.Get("limit") != "200" {
						t.Errorf("invalid query: %s", r.URL)
					}
					offset := 0
					if q.Get("cursor") != "" {
						var err error
						offset, err = strconv.Atoi(q.Get("cursor"))
						if err != nil {
							t.Error(err)
						}
					}
					end := min(offset+200, total)
					items := make([]Item, 0, end-offset)
					for i := offset; i < end; i++ {
						items = append(items, Item{ID: strconv.Itoa(i), Type: kind})
					}
					var p page[Item]
					p.Items = items
					p.Page.HasMore = end < total
					if p.Page.HasMore {
						p.Page.NextCursor = strconv.Itoa(end)
					}
					json.NewEncoder(w).Encode(p)
				}))
				defer server.Close()
				cfg := DefaultConfig()
				cfg.APIKey = "test"
				c, err := NewClient(server.URL, cfg)
				if err != nil {
					t.Fatal(err)
				}
				c.interval = 0
				items, err := c.Items(context.Background(), "lib", kind)
				if total == 100001 {
					if err == nil || !strings.Contains(err.Error(), "100000") || items != nil {
						t.Fatalf("overflow accepted: %d, %v", len(items), err)
					}
				} else {
					if err != nil || len(items) != total {
						t.Fatalf("items=%d error=%v", len(items), err)
					}
					for i, item := range items {
						if item.ID != strconv.Itoa(i) {
							t.Fatalf("missing/reordered item %d: %s", i, item.ID)
						}
					}
				}
				if calls != (total+199)/200 {
					t.Fatalf("pages=%d", calls)
				}
			})
		}
	}
}

func TestPaginationAllowsSmallPagesBeyondOldPageLimit(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprintf(w, `{"items":[{"id":"%d"}],"page":{"has_more":%t,"next_cursor":"%d"}}`, calls, calls < 1001, calls)
	}))
	defer server.Close()
	cfg := DefaultConfig()
	cfg.APIKey = "test"
	c, _ := NewClient(server.URL, cfg)
	c.interval = 0
	items, err := c.Libraries(context.Background())
	if err != nil || len(items) != 1001 {
		t.Fatalf("items=%d error=%v", len(items), err)
	}
}

func TestPaginationRejectsMissingCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"items":[],"page":{"has_more":true}}`) }))
	defer server.Close()
	cfg := DefaultConfig()
	cfg.APIKey = "test"
	c, _ := NewClient(server.URL, cfg)
	if _, err := c.Libraries(context.Background()); err == nil {
		t.Fatal("missing cursor accepted")
	}
}

func TestRetryAfterAndSharedCooldown(t *testing.T) {
	if retryAfter("2", 0) != 2*time.Second {
		t.Fatal("Retry-After seconds ignored")
	}
	future := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	delay := retryAfter(future.Format(http.TimeFormat), 0)
	if delay < 58*time.Second || delay > time.Minute {
		t.Fatalf("Retry-After date ignored: %v", delay)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		fmt.Fprint(w, `{"items":[]}`)
	}))
	defer server.Close()
	cfg := DefaultConfig()
	cfg.APIKey = "test"
	c, _ := NewClient(server.URL, cfg)
	start := time.Now()
	if _, err := c.Libraries(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < time.Second || calls != 2 {
		t.Fatal("Retry-After ignored")
	}
	c.coolDown(time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Libraries(ctx); err == nil || calls != 2 {
		t.Fatal("shared cooldown bypassed")
	}
}

type deadlineTransport func(*http.Request) (*http.Response, error)

func (f deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestScanHandlerAppliesTwoHourDeadline(t *testing.T) {
	h := NewHandler()
	cfg := DefaultConfig()
	cfg.APIKey = "test"
	c, _ := NewClient("http://silo", cfg)
	c.interval = 0
	// Observe the scan deadline without the separate per-request timeout.
	c.http.Timeout = 0
	deadlines := make(chan time.Time, 1)
	c.http.Transport = deadlineTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v2/user/libraries" {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"items":[{"id":"lib"}]}`)), Header: make(http.Header)}, nil
		}
		deadline, ok := r.Context().Deadline()
		if !ok {
			deadlines <- time.Time{}
		} else {
			deadlines <- deadline
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	h.client = c
	req := &pluginv1.HandleHTTPRequest{Method: "POST", Path: "/scan", Headers: map[string]string{"X-Silo-User-Role": "admin", "X-Silo-User-Id": "1", "Content-Type": "application/json"}, Body: []byte(`{"library_id":"lib","csrf":"` + h.csrf + `"}`)}
	res, err := h.Handle(context.Background(), req)
	if err != nil || res.StatusCode != 202 {
		t.Fatalf("scan start: %v %v", res, err)
	}
	defer func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.cancel != nil {
			h.cancel()
		}
	}()
	select {
	case deadline := <-deadlines:
		remaining := time.Until(deadline)
		if remaining < 2*time.Hour-time.Minute || remaining > 2*time.Hour {
			t.Fatalf("scan deadline: %v", remaining)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not start")
	}
}
