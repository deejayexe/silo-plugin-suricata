package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// Scheduled tasks: the server health check and the weekly digest.
//
// Silo runs a scheduled_task.v1 capability when its binding's trigger fires
// (an admin sets that up under Admin > Plugins; a binding change needs a
// Silo restart). The run request carries only a task key,
// "plugin:<installation>:<capability>", and no event ID, so these tasks make
// their sends idempotent on their own:
//
//   - A health alert's ID comes from the state change it announces: the
//     admin's change counter plus the problems involved. A run that crashes
//     or fails after sending repeats the same change with the same ID and
//     sends nothing twice; the state only moves on once the sends are done.
//   - A digest's ID is the ISO week of the run in the admin's time zone, so a
//     repeat run (a restart, a second Silo server, or a binding left on its
//     startup trigger) sends at most one digest per admin a week. The task is
//     meant to run every hour: an admin's digest goes out only in the hour
//     they chose, since a run can't tell a scheduled start from a restart.
//
// Both go to admins who switched them on, after their role is re-checked with
// the read-only key, like every admin alert. A run has Silo's 10 s call
// deadline, so it works inside taskTimeout and fetches everything side by
// side.

const (
	capHealthCheck  = "health_check"
	capWeeklyDigest = "weekly_digest"

	healthStateKey = "health"

	// taskTimeout bounds one run. Silo gives a plugin call 10 s.
	taskTimeout = 8 * time.Second
	// taskLookupBudget bounds the Silo reads of one run, leaving the rest for
	// sending.
	taskLookupBudget = 5 * time.Second
	// healthClearMargin is how far under its limit a disk or memory reading
	// must fall before the problem counts as fixed, so a value hovering at
	// the limit doesn't alert and recover every few minutes.
	healthClearMargin = 2
	// healthHeartbeat is how often the saved result is refreshed when
	// nothing changed.
	healthHeartbeat = 30 * time.Minute

	digestDays       = 7
	digestTopLimit   = 25 // Silo's cap on a top-activity list
	maxDigestEntries = 5
	// maxDigestRunes keeps a digest clear of Gotify's 1024-character limit.
	maxDigestRunes = 1000
)

// taskServer answers Silo's scheduled task calls.
type taskServer struct {
	pluginv1.UnimplementedScheduledTaskServer
	plugin *Plugin
}

// taskCapability picks the capability ID out of a task key
// ("plugin:<installation>:<capability>").
func taskCapability(key string) string {
	if parts := strings.SplitN(key, ":", 3); len(parts) == 3 && parts[0] == "plugin" {
		return parts[2]
	}
	return key
}

// Run runs one task. The output is kept in Silo's task history, so it holds
// counts and no names.
func (s *taskServer) Run(ctx context.Context, req *pluginv1.RunScheduledTaskRequest) (*pluginv1.RunScheduledTaskResponse, error) {
	p := s.plugin
	ctx, cancel := context.WithTimeout(ctx, p.taskTimeout)
	defer cancel()
	var out map[string]any
	var err error
	switch id := taskCapability(req.GetTaskKey()); id {
	case capRecentlyAdded:
		out, err = p.runRecentlyAdded(ctx, p.taskNow())
	case capHealthCheck:
		out, err = p.runHealthCheck(ctx, p.taskNow())
	case capWeeklyDigest:
		out, err = p.runDigest(ctx, p.taskNow())
	default:
		return nil, fmt.Errorf("this plugin has no scheduled task %q", id)
	}
	if err != nil {
		return nil, err
	}
	output, err := structpb.NewStruct(out)
	if err != nil {
		return &pluginv1.RunScheduledTaskResponse{}, nil
	}
	return &pluginv1.RunScheduledTaskResponse{Output: output}, nil
}

func isKeyError(err error) bool {
	return errors.Is(err, errSiloKeyRejected) || errors.Is(err, errSiloKeyScope)
}

// warnKeyRefused logs a refused read-only key once until the key changes.
func (p *Plugin) warnKeyRefused(log *slog.Logger, err error) {
	if !p.adminKeyWarned.Swap(true) {
		log.Warn("Silo refused the read-only key, so admin alerts are paused until it's fixed. Set up the key again from the admin section of the Gotify page.", "error", err)
	}
}

// taskAdmins loads the admins who switched on the alert kind, as candidate
// recipients. It returns an error only when the settings can't be read.
func (p *Plugin) taskAdmins(ctx context.Context, log *slog.Logger, kind string) ([]*recipient, error) {
	admins, err := waitForHost(ctx, func() ([]*AdminSettings, error) { return p.admins.List(ctx) })
	if err != nil {
		log.Warn("could not load admin alert settings", "error", err)
		return nil, fmt.Errorf("load admin alert settings: %w", err)
	}
	var out []*recipient
	for _, a := range admins {
		if a.alert(kind).Enabled {
			out = append(out, newRecipient(a, kind, ""))
		}
	}
	return out, nil
}

// --- Server health check ---

// Dependency states in a health result.
const (
	depOK   = "ok"
	depDown = "down"
	depOff  = "off" // not configured
)

type healthNode struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Healthy bool   `json:"healthy"`
}

type healthDisk struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	// Pct is how full the disk is; -1 when Silo has no fresh measurement.
	Pct int `json:"pct"`
}

// healthResult is what one check saw. A part whose read failed has its OK
// flag false, and the check then neither raises nor clears problems about it.
type healthResult struct {
	At time.Time `json:"at"`

	NodesOK bool `json:"nodes_ok"`
	// Nodes are the enabled nodes that have been health-checked at least once.
	Nodes []healthNode `json:"nodes,omitempty"`

	ServerOK   bool   `json:"server_ok"`
	Restart    bool   `json:"restart"`
	RestartWhy string `json:"restart_why,omitempty"`
	// Postgres and Redis are depOK, depDown, depOff, or empty when Silo
	// didn't say.
	Postgres string `json:"postgres,omitempty"`
	Redis    string `json:"redis,omitempty"`

	ResourcesOK bool `json:"resources_ok"`
	// MemPct is how much memory is in use; -1 when Silo didn't say.
	MemPct int          `json:"mem_pct"`
	Disks  []healthDisk `json:"disks,omitempty"`
}

func pct(used, total float64) int {
	if total <= 0 {
		return -1
	}
	return int(math.Round(used / total * 100))
}

func diskName(d siloDisk, i int) string {
	switch {
	case d.Scratch || d.Role == "scratch":
		return "transcode scratch"
	case strings.HasPrefix(d.Role, "library-"):
		return "library " + strings.TrimPrefix(d.Role, "library-")
	case d.Role != "":
		return d.Role
	case d.Path != "":
		return d.Path
	}
	return "disk " + strconv.Itoa(i+1)
}

func diskKey(d siloDisk, i int) string {
	return "disk:" + cmp.Or(d.Role, d.Path, "#"+strconv.Itoa(i+1))
}

func depState(c siloComponent) string {
	switch {
	case !c.Configured:
		return depOff
	case c.OK == nil:
		return ""
	case *c.OK:
		return depOK
	}
	return depDown
}

// readHealth reads the three health routes side by side. A route that fails
// leaves its part of the result unset; err is set when nothing could be read
// (or the key was refused), and then the run has nothing to go on.
func (p *Plugin) readHealth(ctx context.Context, log *slog.Logger, base, key string, now time.Time) (*healthResult, error) {
	res := &healthResult{At: now, MemPct: -1}
	var nodes []siloNode
	var status *siloServerStatus
	var resources *siloResources
	var nodesErr, statusErr, resErr error
	var wg sync.WaitGroup
	wg.Go(func() { nodes, nodesErr = p.silo.NodeSummaries(ctx, base, key) })
	wg.Go(func() { status, statusErr = p.silo.ServerStatus(ctx, base, key) })
	wg.Go(func() { resources, resErr = p.silo.SystemResources(ctx, base, key) })
	wg.Wait()

	for _, err := range []error{nodesErr, statusErr, resErr} {
		if isKeyError(err) {
			return nil, err
		}
	}
	if nodesErr != nil && statusErr != nil && resErr != nil {
		log.Warn("could not read Silo's health", "nodes", nodesErr, "status", statusErr, "resources", resErr)
		return nil, fmt.Errorf("couldn't read Silo's health: %w", errors.Join(nodesErr, statusErr, resErr))
	}
	for what, err := range map[string]error{"node list": nodesErr, "server status": statusErr, "system resources": resErr} {
		if err != nil {
			log.Warn("could not read Silo's "+what+"; skipping the checks that need it", "error", err)
		}
	}

	if nodesErr == nil {
		res.NodesOK = true
		for _, n := range nodes {
			if n.Enabled && n.LastHealthCheck != nil && n.ID != "" {
				res.Nodes = append(res.Nodes, healthNode{ID: n.ID, Name: n.Name, Type: n.Type, Healthy: n.Healthy})
			}
		}
	}
	if statusErr == nil {
		res.ServerOK = true
		res.Restart = status.RestartRequired
		res.RestartWhy = errorSummary(cmp.Or(strings.Join(status.RestartRequiredReasons, "; "), status.RestartRequiredReason))
		res.Postgres, res.Redis = depState(status.Health.Postgres), depState(status.Health.Redis)
	}
	// A sample the host calls stale, or one it never took, says nothing
	// current about disks or memory.
	if resErr == nil && resources.Available && !resources.Stale && resources.System != nil {
		res.ResourcesOK = true
		sys := resources.System
		res.MemPct = pct(float64(sys.MemUsedMB), float64(sys.MemTotalMB))
		for i, d := range sys.Disks {
			disk := healthDisk{Key: diskKey(d, i), Name: diskName(d, i), Pct: pct(d.UsedGB, d.TotalGB)}
			if d.Unavailable || d.Stale {
				disk.Pct = -1
			}
			res.Disks = append(res.Disks, disk)
		}
	}
	return res, nil
}

type verdictKind int

const (
	verdictUnknown verdictKind = iota // can't tell right now: keep what we knew
	verdictOK
	verdictBad
)

// verdict is what a check says about one thing: bad is the line announcing a
// problem, fixed the line announcing its recovery.
type verdict struct {
	kind       verdictKind
	bad, fixed string
}

// verdicts judges every part of the result for one admin, by their limits.
// open holds the problems the admin was already told about, which stay
// problems until they're clearly under the limit.
func (r *healthResult) verdicts(a *AdminSettings, open map[string]healthProblem) map[string]verdict {
	vs := map[string]verdict{}
	if r.NodesOK {
		for _, n := range r.Nodes {
			label := titleCase(cmp.Or(n.Type, "stream")) + " node “" + n.Name + "”"
			v := verdict{kind: verdictOK, bad: label + " isn't healthy", fixed: label + " is healthy again"}
			if !n.Healthy {
				v.kind = verdictBad
			}
			vs["node:"+n.ID] = v
		}
	}
	if r.ServerOK {
		v := verdict{kind: verdictOK, bad: "Silo needs a restart", fixed: "Silo no longer needs a restart"}
		if r.Restart {
			v.kind = verdictBad
			if r.RestartWhy != "" {
				v.bad += " (" + r.RestartWhy + ")"
			}
		}
		vs["restart"] = v
		for _, d := range []struct{ key, name, state string }{{"postgres", "The database (Postgres)", r.Postgres}, {"redis", "Redis", r.Redis}} {
			v := verdict{kind: verdictUnknown, bad: d.name + " isn't responding", fixed: d.name + " is responding again"}
			switch d.state {
			case depOK:
				v.kind = verdictOK
			case depDown:
				v.kind = verdictBad
			case depOff:
				continue // not set up here, so nothing to judge
			}
			vs["dep:"+d.key] = v
		}
	}
	if r.ResourcesOK {
		judge := func(key string, value, limit int, bad, fixed func() string) {
			v := verdict{kind: verdictUnknown}
			if value >= 0 {
				_, wasOpen := open[key]
				switch {
				case value > limit, wasOpen && value > limit-healthClearMargin:
					v = verdict{kind: verdictBad, bad: bad()}
				default:
					v = verdict{kind: verdictOK, fixed: fixed()}
				}
			}
			vs[key] = v
		}
		if limit := a.Limits.MemoryPct; limit > 0 {
			judge("memory", r.MemPct, limit,
				func() string { return fmt.Sprintf("Memory is %d%% used (limit %d%%)", r.MemPct, limit) },
				func() string { return fmt.Sprintf("Memory is down to %d%%", r.MemPct) })
		}
		if limit := a.Limits.DiskPct; limit > 0 {
			for _, d := range r.Disks {
				judge(d.Key, d.Pct, limit,
					func() string { return fmt.Sprintf("Disk “%s” is %d%% full (limit %d%%)", d.Name, d.Pct, limit) },
					func() string { return fmt.Sprintf("Disk “%s” is down to %d%%", d.Name, d.Pct) })
			}
		}
	}
	return vs
}

// observes reports whether the check read the part of Silo a problem is
// about. A problem about something it couldn't read is neither confirmed nor
// cleared.
func (r *healthResult) observes(key string) bool {
	switch {
	case strings.HasPrefix(key, "node:"):
		return r.NodesOK
	case key == "restart", strings.HasPrefix(key, "dep:"):
		return r.ServerOK
	case key == "memory", strings.HasPrefix(key, "disk:"):
		return r.ResourcesOK
	}
	return true // a problem this plugin no longer knows about
}

// healthProblem is a problem an admin has been told about and that hasn't
// cleared. Text is the line that announced it.
type healthProblem struct {
	Text  string    `json:"text"`
	Since time.Time `json:"since"`
}

// healthAdmin is what one admin has been told. Seq counts the changes
// announced to them, so each change has an ID of its own.
type healthAdmin struct {
	Seq     int                      `json:"seq"`
	Open    map[string]healthProblem `json:"open,omitempty"`
	Updated time.Time                `json:"updated"`
}

// healthState is the "health" state key: the last result, for troubleshooting
// (decisions never read it), and what each admin has been told.
type healthState struct {
	Last   *healthResult           `json:"last,omitempty"`
	Admins map[string]*healthAdmin `json:"admins"`
}

func (st *healthState) entries() int {
	n := len(st.Admins)
	if st.Last != nil {
		n += len(st.Last.Nodes) + len(st.Last.Disks)
	}
	return n
}

// dropOldest sheds the saved result's detail first, then the admins who
// changed longest ago.
func (st *healthState) dropOldest(n int) {
	if l := st.Last; l != nil {
		for n > 0 && len(l.Nodes) > 0 {
			l.Nodes, n = l.Nodes[:len(l.Nodes)-1], n-1
		}
		for n > 0 && len(l.Disks) > 0 {
			l.Disks, n = l.Disks[:len(l.Disks)-1], n-1
		}
	}
	for _, k := range oldestFirst(st.Admins, func(a *healthAdmin) time.Time { return a.Updated }) {
		if n == 0 {
			return
		}
		delete(st.Admins, k)
		n--
	}
}

// healthStore owns the "health" key.
type healthStore struct {
	state stateBackend
	// mu makes one run's read, decisions and write one step in this process.
	mu sync.Mutex
}

func newHealthStore(state stateBackend) *healthStore { return &healthStore{state: state} }

func (h *healthStore) read(ctx context.Context) (*healthState, []byte, error) {
	raw, found, err := h.state.Read(ctx, healthStateKey)
	if err != nil {
		return nil, nil, err
	}
	st := &healthState{}
	if found && json.Unmarshal(raw, st) != nil {
		// Damaged state: start over. Admins are told about what's wrong now.
		st = &healthState{}
	}
	if st.Admins == nil {
		st.Admins = map[string]*healthAdmin{}
	}
	for id, a := range st.Admins {
		if a == nil {
			delete(st.Admins, id)
		} else if a.Open == nil {
			a.Open = map[string]healthProblem{}
		}
	}
	return st, raw, nil
}

func (h *healthStore) write(ctx context.Context, st *healthState, old []byte) error {
	raw, err := encodeCapped(st)
	if err != nil || string(raw) == string(old) {
		return err
	}
	return h.state.Write(ctx, healthStateKey, raw)
}

// healthChange is one admin's change since they were last told.
type healthChange struct {
	admin *AdminSettings
	prev  *healthAdmin
	// next is what they'll have been told once the pushes are done.
	next map[string]healthProblem
	// New problems and fixed ones, as lines, with the keys they came from.
	newLines, fixedLines []string
	newKeys, fixedKeys   []string
	// ongoing lists the problems still open that were announced earlier.
	ongoing []string
}

func (c *healthChange) pushes() bool { return len(c.newKeys) > 0 || len(c.fixedKeys) > 0 }

// changeFor compares what the check found with what the admin was told.
func changeFor(a *AdminSettings, prev *healthAdmin, r *healthResult, now time.Time) *healthChange {
	c := &healthChange{admin: a, prev: prev, next: map[string]healthProblem{}}
	vs := r.verdicts(a, prev.Open)
	for _, key := range slices.Sorted(maps.Keys(prev.Open)) {
		told := prev.Open[key]
		v, judged := vs[key]
		switch {
		case !judged && r.observes(key):
			// The thing is gone (a removed node, a limit switched off):
			// there's nothing to recover.
		case !judged, v.kind == verdictUnknown:
			c.next[key] = told // couldn't look
			c.ongoing = append(c.ongoing, told.Text)
		case v.kind == verdictBad:
			c.next[key] = told
			c.ongoing = append(c.ongoing, v.bad)
		default:
			c.fixedKeys, c.fixedLines = append(c.fixedKeys, key), append(c.fixedLines, v.fixed)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(vs)) {
		if _, told := prev.Open[key]; told || vs[key].kind != verdictBad {
			continue
		}
		c.next[key] = healthProblem{Text: vs[key].bad, Since: now}
		c.newKeys, c.newLines = append(c.newKeys, key), append(c.newLines, vs[key].bad)
	}
	return c
}

// changedOpen reports whether the problems the admin was told about differ
// from what they'll have been told.
func (c *healthChange) changedOpen() bool {
	if len(c.next) != len(c.prev.Open) {
		return true
	}
	for k, v := range c.next {
		if old, ok := c.prev.Open[k]; !ok || old.Text != v.Text {
			return true
		}
	}
	return false
}

// healthID names a change: the admin's change counter plus the problems
// involved. A repeat of a failed run gets the same ID; a change that differs
// (a problem came or went in between) gets another.
func healthID(seq int, keys []string) string {
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return "health:" + strconv.Itoa(seq+1) + ":" + hex.EncodeToString(sum[:4])
}

func ongoingLine(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return "Still open: " + strings.Join(lines, "; ")
}

// runHealthCheck is the health check task: read Silo's health, work out what
// changed for each admin who wants it, and tell them.
func (p *Plugin) runHealthCheck(ctx context.Context, now time.Time) (map[string]any, error) {
	log := p.logger.With("task", capHealthCheck)
	cfg := p.config()
	if cfg.MonitorAPIKey == "" {
		return map[string]any{"status": "skipped: no read-only Silo key"}, nil
	}
	candidates, err := p.taskAdmins(ctx, log, alertHealth)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		// Nobody wants it. Forget what was told, so an admin who turns it on
		// later hears about whatever is wrong then.
		if err := p.forgetHealth(ctx, now); err != nil {
			log.Warn("could not clear the saved health state", "error", err)
		}
		return map[string]any{"status": "skipped: no admin has health alerts on"}, nil
	}
	base, err := waitForHost(ctx, func() (string, error) { return p.siloBase(ctx) })
	if err != nil {
		return nil, err
	}

	lctx, cancel := context.WithTimeout(ctx, taskLookupBudget)
	var result *healthResult
	var readErr error
	var confirmed []*recipient
	var stop bool
	var wg sync.WaitGroup
	wg.Go(func() { result, readErr = p.readHealth(lctx, log, base, cfg.MonitorAPIKey, now) })
	wg.Go(func() { confirmed, stop, _ = p.confirmAdmins(lctx, log, base, cfg.MonitorAPIKey, candidates) })
	wg.Wait()
	cancel()
	if isKeyError(readErr) {
		p.warnKeyRefused(log, readErr)
		return map[string]any{"status": "skipped: Silo refused the read-only key"}, nil
	}
	if readErr != nil {
		return nil, readErr
	}
	if stop {
		return map[string]any{"status": "skipped: Silo refused the read-only key"}, nil
	}

	p.health.mu.Lock()
	defer p.health.mu.Unlock()
	type stored struct {
		st  *healthState
		raw []byte
	}
	loaded, err := waitForHost(ctx, func() (stored, error) {
		st, raw, err := p.health.read(ctx)
		return stored{st, raw}, err
	})
	st, raw := loaded.st, loaded.raw
	if err != nil {
		log.Warn("could not read the saved health state", "error", err)
		return nil, fmt.Errorf("read the saved health state: %w", err)
	}

	wanted := map[string]bool{}
	for _, r := range candidates {
		wanted[strconv.Itoa(r.admin.UserID)] = true
	}
	st.forgetUnwanted(wanted, now)

	var changes []*healthChange
	var recipients []*recipient
	byRecipient := map[*recipient]*healthChange{}
	for _, c := range confirmed {
		id := strconv.Itoa(c.admin.UserID)
		prev := st.Admins[id]
		if prev == nil {
			// A new admin's counter starts at the hour, so it can't repeat the
			// IDs of an entry that was dropped long ago and may still be in
			// the handled log.
			prev = &healthAdmin{Seq: int(now.Unix() / 3600), Open: map[string]healthProblem{}}
		}
		ch := changeFor(c.admin, prev, result, now)
		changes = append(changes, ch)
		ongoing := ongoingLine(ch.ongoing)
		if len(ch.newKeys) > 0 {
			r := newRecipient(c.admin, alertHealth, healthID(prev.Seq, ch.newKeys))
			r.vars = map[string]string{"problems": strings.Join(ch.newLines, "\n"), "count": strconv.Itoa(len(ch.newKeys)), "ongoing": ongoing}
			recipients, byRecipient[r] = append(recipients, r), ch
		}
		if len(ch.fixedKeys) > 0 && c.admin.alert(alertHealthRecovered).Enabled {
			r := newRecipient(c.admin, alertHealthRecovered, healthID(prev.Seq, ch.fixedKeys))
			r.vars = map[string]string{"resolved": strings.Join(ch.fixedLines, "\n"), "count": strconv.Itoa(len(ch.fixedKeys)), "ongoing": ongoing}
			recipients, byRecipient[r] = append(recipients, r), ch
		}
	}

	kept, readAgain, err := p.dropHandled(ctx, log, recipients)
	if err != nil {
		log.Warn("could not read handled events", "error", err)
		return nil, fmt.Errorf("read handled events: %w", err)
	}
	// An admin's change is held back, and tried again next run, when a send
	// could still go through, or (for everyone) when an admin's Gotify
	// settings couldn't be read. An admin whose role couldn't be checked isn't
	// in the changes at all.
	hold := readAgain
	held := map[*healthChange]bool{}
	sent := 0
	for r, retry := range p.sendAll(ctx, log, kept, now, 1) {
		if retry {
			held[byRecipient[r]] = true
		} else {
			sent++
		}
	}

	pending := 0
	for _, ch := range changes {
		if hold || held[ch] {
			pending++
			continue
		}
		entry := &healthAdmin{Seq: ch.prev.Seq, Open: ch.next, Updated: ch.prev.Updated}
		if ch.pushes() {
			entry.Seq++
		}
		if ch.pushes() || ch.changedOpen() || ch.prev.Updated.IsZero() {
			entry.Updated = now
		}
		st.Admins[strconv.Itoa(ch.admin.UserID)] = entry
	}

	open := 0
	for _, a := range st.Admins {
		open += len(a.Open)
	}
	if st.Last == nil || !sameHealth(st.Last, result) || now.Sub(st.Last.At) >= healthHeartbeat {
		st.Last = result
	}
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer wcancel()
	if err := p.health.write(wctx, st, raw); err != nil {
		// The sends are done; the cost is that the next run may announce the
		// same change again under the same ID, which the handled log stops.
		log.Warn("could not save the health state", "error", err)
	}
	if pending > 0 {
		return nil, errors.New("some health alerts couldn't be sent yet; the next run will try again")
	}
	return map[string]any{
		"status": "ok", "admins": len(changes), "alerts_handled": sent, "open_problems": open,
		"nodes_checked": len(result.Nodes), "resources_read": result.ResourcesOK,
	}, nil
}

// sameHealth compares two results apart from when they were taken.
func sameHealth(a, b *healthResult) bool {
	x, y := *a, *b
	x.At, y.At = time.Time{}, time.Time{}
	rx, _ := json.Marshal(x)
	ry, _ := json.Marshal(y)
	return string(rx) == string(ry)
}

// forgetHealth forgets the problems admins were told about, when nobody has
// health alerts on.
func (p *Plugin) forgetHealth(ctx context.Context, now time.Time) error {
	p.health.mu.Lock()
	defer p.health.mu.Unlock()
	st, raw, err := p.health.read(ctx)
	if err != nil || len(st.Admins) == 0 {
		return err
	}
	st.forgetUnwanted(nil, now)
	return p.health.write(ctx, st, raw)
}

// forgetUnwanted forgets the open problems of admins who no longer have
// health alerts on, so turning them back on reports whatever is wrong then.
// Their change counters stay (an ID is never used twice for different
// changes) until they've been idle for a month.
func (st *healthState) forgetUnwanted(wanted map[string]bool, now time.Time) {
	for id, a := range st.Admins {
		switch {
		case wanted[id]:
		case now.Sub(a.Updated) > 30*24*time.Hour:
			delete(st.Admins, id)
		case len(a.Open) > 0:
			a.Open, a.Updated = map[string]healthProblem{}, now
		}
	}
}

// --- Weekly digest ---

// digestStats is a week's numbers as the digest uses them.
type digestStats struct {
	top      *siloTopActivity
	activity *siloPlaybackActivity
}

// fmtWatchTime is "41h 20m", or "35m" under an hour.
func fmtWatchTime(seconds int64) string {
	m := max(seconds, 0) / 60
	if m >= 60 {
		return fmt.Sprintf("%dh %dm", m/60, m%60)
	}
	return fmt.Sprintf("%dm", m)
}

func shortText(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > limit {
		return string([]rune(s)[:limit-1]) + "…"
	}
	return s
}

func percent(part, whole int64) string {
	if whole <= 0 {
		return "0%"
	}
	return strconv.Itoa(int(math.Round(float64(part)/float64(whole)*100))) + "%"
}

// vars fills the digest's placeholders for one admin, listing at most n top
// viewers and n top titles. The admin's ignored users are left out of the
// viewers and the watch time.
func (d *digestStats) vars(a *AdminSettings, n int) map[string]string {
	rel := d.activity.Reliability
	plays := rel.SessionsStarted
	if plays == 0 {
		for _, b := range d.activity.Buckets {
			plays += b.Direct + b.Remux + b.Transcode
		}
	}

	type viewer struct {
		name        string
		plays, secs int64
	}
	var viewers []viewer
	var total int64
	for _, p := range d.top.Profiles {
		if slices.Contains(a.Filters.IgnoreUsers, p.UserID) {
			continue
		}
		name := shortText(p.Username, 24)
		if p.ProfileName != "" && !strings.EqualFold(p.ProfileName, p.Username) {
			name += " (" + shortText(p.ProfileName, 24) + ")"
		}
		viewers = append(viewers, viewer{name, p.Plays, p.TotalSeconds})
		total += p.TotalSeconds
	}
	slices.SortStableFunc(viewers, func(x, y viewer) int { return cmp.Compare(y.secs, x.secs) })
	watch := fmtWatchTime(total)
	if d.top.Limit > 0 && len(d.top.Profiles) >= d.top.Limit {
		watch = "at least " + watch // the list was cut off
	}

	var users, titles []string
	for i, v := range viewers[:min(n, len(viewers))] {
		users = append(users, fmt.Sprintf("%d. %s · %s · %s", i+1, v.name, playsText(v.plays), fmtWatchTime(v.secs)))
	}
	for i, t := range d.top.Titles[:min(n, len(d.top.Titles))] {
		titles = append(titles, fmt.Sprintf("%d. %s · %s", i+1, shortText(cmp.Or(t.Title, "Unknown title"), 40), playsText(t.Plays)))
	}
	list := func(heading string, lines []string) string {
		if len(lines) == 0 {
			return ""
		}
		return heading + "\n" + strings.Join(lines, "\n")
	}
	return map[string]string{
		"plays":         strconv.FormatInt(plays, 10),
		"watch_time":    watch,
		"profiles":      strconv.FormatInt(rel.UniqueProfiles, 10),
		"transcodes":    strconv.FormatInt(rel.TranscodeStarts, 10),
		"transcode_pct": percent(rel.TranscodeStarts, rel.SessionsStarted),
		"completion":    strconv.Itoa(int(math.Round(rel.CompletionRate*100))) + "%",
		"days":          strconv.Itoa(digestDays),
		"top_users":     list("Top viewers", users),
		"top_titles":    list("Top titles", titles),
	}
}

func playsText(n int64) string {
	if n == 1 {
		return "1 play"
	}
	return strconv.FormatInt(n, 10) + " plays"
}

// fit picks the longest top lists (up to maxDigestEntries) that keep the
// admin's rendered digest under maxDigestRunes.
func (d *digestStats) fit(r *recipient) map[string]string {
	tmpl := r.admin.alert(alertDigest).Message
	vars := d.vars(r.admin, 0)
	for n := maxDigestEntries; n >= 0; n-- {
		vars = d.vars(r.admin, n)
		if utf8.RuneCountInString(renderTemplate(tmpl, vars, r.profile.HTML)) <= maxDigestRunes {
			break
		}
	}
	return vars
}

// isoWeek names the ISO week a time falls in, like "2026-W45".
func isoWeek(t time.Time) string {
	year, week := t.ISOWeek()
	return fmt.Sprintf("%d-W%02d", year, week)
}

// runDigest is the weekly digest task: read the last week's numbers and send
// them to each admin who wants them, once for each ISO week.
func (p *Plugin) runDigest(ctx context.Context, now time.Time) (map[string]any, error) {
	log := p.logger.With("task", capWeeklyDigest)
	cfg := p.config()
	if cfg.MonitorAPIKey == "" {
		return map[string]any{"status": "skipped: no read-only Silo key"}, nil
	}
	candidates, err := p.taskAdmins(ctx, log, alertDigest)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return map[string]any{"status": "skipped: no admin has the weekly digest on"}, nil
	}
	// Silo runs the task every hour (or once at each start, when it isn't
	// bound), and can't say which: each admin's digest goes out in their chosen
	// hour, in their time zone, or in a later run up to digestCatchUp after it
	// (Silo may have been down in that hour), and at most once per ISO week of
	// that time zone. The handled ID names the week of the chosen hour, so a
	// catch-up run that crosses midnight into the next week is still the same
	// digest.
	var due []*recipient
	for _, r := range candidates {
		if week, ok := r.admin.digestSlot(now); ok {
			due = append(due, newRecipient(r.admin, alertDigest, "digest:"+week))
		}
	}
	if len(due) == 0 {
		return map[string]any{"status": "skipped: no admin's digest is due now"}, nil
	}
	candidates = due
	base, err := waitForHost(ctx, func() (string, error) { return p.siloBase(ctx) })
	if err != nil {
		return nil, err
	}

	// Skip the Silo reads when every admin already has this week's digest.
	pending, again, err := p.dropHandled(ctx, log, candidates)
	if err != nil {
		log.Warn("could not read handled events", "error", err)
		return nil, fmt.Errorf("read handled events: %w", err)
	}
	if len(pending) == 0 && !again {
		return map[string]any{"status": "skipped: this week's digest is already sent"}, nil
	}

	lctx, cancel := context.WithTimeout(ctx, taskLookupBudget)
	stats := &digestStats{}
	var topErr, actErr error
	var confirmed []*recipient
	var stop, confirmAgain bool
	var wg sync.WaitGroup
	wg.Go(func() {
		stats.top, topErr = p.silo.TopActivity(lctx, base, cfg.MonitorAPIKey, digestDays, digestTopLimit)
	})
	wg.Go(func() { stats.activity, actErr = p.silo.PlaybackActivity(lctx, base, cfg.MonitorAPIKey, digestDays*24) })
	wg.Go(func() { confirmed, stop, confirmAgain = p.confirmAdmins(lctx, log, base, cfg.MonitorAPIKey, pending) })
	wg.Wait()
	cancel()
	for _, err := range []error{topErr, actErr} {
		if isKeyError(err) {
			p.warnKeyRefused(log, err)
			return map[string]any{"status": "skipped: Silo refused the read-only key"}, nil
		}
	}
	if stop {
		return map[string]any{"status": "skipped: Silo refused the read-only key"}, nil
	}
	if err := cmp.Or(topErr, actErr); err != nil {
		log.Warn("could not read the week's statistics", "top_activity", topErr, "playback_activity", actErr)
		return nil, fmt.Errorf("couldn't read the week's statistics: %w", err)
	}

	for _, r := range confirmed {
		r.vars = stats.fit(r)
	}
	// A task has no Silo retry, so a send that could still go through gets a
	// second try within the run.
	sent, retry := 0, 0
	for _, retryable := range p.sendAll(ctx, log, confirmed, now, 2) {
		if retryable {
			retry++
		} else {
			sent++
		}
	}
	if retry > 0 || again || confirmAgain {
		return nil, errors.New("some weekly digests couldn't be sent yet")
	}
	return map[string]any{"status": "ok", "digests_sent": sent}, nil
}
