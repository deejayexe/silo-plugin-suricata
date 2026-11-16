package main

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Playback lifecycle events Silo publishes on its admin sessions channel
// (silo-server internal/api/handlers/playback_lifecycle.go). The event ID is
// "playback-lifecycle:<seq>".
const (
	eventSessionStarted = "session.started"
	eventSessionPaused  = "session.paused"
	eventSessionResumed = "session.resumed"
	eventSessionStopped = "session.stopped"
)

func isSessionEvent(name string) bool {
	switch name {
	case eventSessionStarted, eventSessionPaused, eventSessionResumed, eventSessionStopped:
		return true
	}
	return false
}

// Playback end reasons (silo-server playback_admin_store.go). "aborted" means
// Silo gave up (media missing or the transport failed); "replaced" means a
// Jellyfin client switched play method and playback goes on in a new session.
const (
	endReasonAborted  = "aborted"
	endReasonReplaced = "replaced"
)

// lifecycleEvent is a session.* event's payload (silo-server apiv2
// AdminPlaybackLifecycleEvent). Events are not ordered: a stop can arrive
// before its start.
type lifecycleEvent struct {
	SessionID string `json:"session_id"`
	Seq       int64  `json:"seq"`
	At        string `json:"at"`
	State     string `json:"state"`
	EndReason string `json:"end_reason"`
	// Session is the live admin row on started, paused and resumed; missing
	// when no server has a live row.
	Session *adminSession `json:"session"`
	// History is the admin history entry a stop recorded.
	History *adminHistory `json:"history"`
}

// adminSession is the part of Silo's AdminPlaybackSession the alerts use.
type adminSession struct {
	SessionID             string   `json:"session_id"`
	UserID                string   `json:"user_id"`
	Username              string   `json:"username"`
	ProfileID             string   `json:"profile_id"`
	ProfileName           string   `json:"profile_name"`
	MediaTitle            string   `json:"media_title"`
	MediaType             string   `json:"media_type"`
	SeriesName            string   `json:"series_name"`
	EpisodeName           string   `json:"episode_name"`
	SeasonNumber          *int     `json:"season_number"`
	EpisodeNumber         *int     `json:"episode_number"`
	ContentID             string   `json:"content_id"`
	PosterURL             string   `json:"poster_url"`
	DeviceID              string   `json:"device_id"`
	DevicePlatform        string   `json:"device_platform"`
	ClientName            string   `json:"client_name"`
	ClientVersion         string   `json:"client_version"`
	ClientLabel           string   `json:"client_label"`
	ClientIP              string   `json:"client_ip"`
	StreamLocation        string   `json:"stream_location"`
	PlayMethod            string   `json:"play_method"`
	EffectivePlayMethod   string   `json:"effective_play_method"`
	VideoDecision         string   `json:"video_decision"`
	AudioDecision         string   `json:"audio_decision"`
	SourceVideoResolution string   `json:"source_video_resolution"`
	TargetResolution      string   `json:"target_resolution"`
	StreamBitrateKbps     *int64   `json:"stream_bitrate_kbps"`
	TranscodeHWAccel      string   `json:"transcode_hw_accel"`
	PositionSeconds       float64  `json:"position_seconds"`
	FileDuration          *float64 `json:"file_duration"` // seconds
	IsPaused              bool     `json:"is_paused"`
	StartedAt             string   `json:"started_at"`
	// HasPlaybackControl is false for players Silo can't send commands to
	// over a live connection (messages need one; a stop falls back to ending
	// the session).
	HasPlaybackControl *bool `json:"has_playback_control"`
}

// adminHistory is the part of Silo's AdminPlaybackHistoryEntry the alerts
// use. Most fields are nullable.
type adminHistory struct {
	SessionID       string   `json:"session_id"`
	UserID          string   `json:"user_id"`
	Username        string   `json:"username"`
	ProfileID       string   `json:"profile_id"`
	ProfileName     string   `json:"profile_name"`
	MediaItemID     string   `json:"media_item_id"`
	MediaTitle      string   `json:"media_title"`
	MediaType       string   `json:"media_type"`
	SeriesTitle     string   `json:"series_title"`
	SeasonNumber    *int     `json:"season_number"`
	EpisodeNumber   *int     `json:"episode_number"`
	PlayMethod      string   `json:"play_method"`
	VideoDecision   *string  `json:"video_decision"`
	AudioDecision   *string  `json:"audio_decision"`
	PlayedSeconds   *float64 `json:"played_seconds"`
	WatchedSeconds  float64  `json:"watched_seconds"`
	DurationSeconds *float64 `json:"duration_seconds"`
	Completed       bool     `json:"completed"`
	ClientIP        *string  `json:"client_ip"`
	DeviceID        *string  `json:"device_id"`
	DevicePlatform  *string  `json:"device_platform"`
	ClientName      *string  `json:"client_name"`
	ClientVersion   *string  `json:"client_version"`
	EndReason       *string  `json:"end_reason"`
	StartedAt       string   `json:"started_at"`
}

// playback is one session event reduced to what filters, noise control and
// templates need, whichever row it came from.
type playback struct {
	Event     string // session.started, .paused, .resumed or .stopped
	SessionID string
	Seq       int64
	At        time.Time

	UserID, Username, ProfileName string

	MediaType, MediaTitle, Series, EpisodeTitle string
	Season, Episode                             *int
	ContentID                                   string
	// PosterURL is the item's poster from Silo's live row (a stop has none).
	PosterURL string

	DeviceID, Platform, ClientName, ClientVersion, ClientLabel, ClientIP string

	// Location is "local", "remote" or "" when unknown.
	Location string
	// Method is direct, remux, direct_stream, transcode, or "" when unknown.
	Method                             string
	SourceResolution, TargetResolution string
	BitrateKbps                        int64
	HWAccel                            string

	Position float64 // seconds, on start/pause/resume
	Duration float64 // seconds, 0 when unknown

	// FromRow is set when the facts came from Silo's live session row, whose
	// location and method are Silo's own rather than guessed.
	FromRow   bool
	StartedAt time.Time

	// Stop only.
	EndReason string
	Played    *float64
	Watched   float64
	Completed bool
}

// parseLifecycle decodes a session.* payload. ok is false when the event
// can't be used: no session ID, or no row saying whose session it is.
func parseLifecycle(eventName string, raw []byte, now time.Time) (pb playback, ok bool) {
	var ev lifecycleEvent
	if err := json.Unmarshal(raw, &ev); err != nil || ev.SessionID == "" {
		return playback{}, false
	}
	pb = playback{Event: eventName, SessionID: ev.SessionID, Seq: ev.Seq, At: now}
	if at, err := time.Parse(time.RFC3339Nano, ev.At); err == nil {
		pb.At = at
	}
	switch {
	case eventName == eventSessionStopped && ev.History != nil:
		pb.fromHistory(ev.History)
	case ev.Session != nil:
		pb.fromSession(ev.Session)
	case ev.History != nil:
		pb.fromHistory(ev.History)
	default:
		return pb, false
	}
	pb.EndReason = ev.EndReason
	if pb.EndReason == "" && ev.History != nil && ev.History.EndReason != nil {
		pb.EndReason = *ev.History.EndReason
	}
	if pb.Location == "" {
		pb.Location = locationFromIP(pb.ClientIP)
	}
	return pb, pb.UserID != ""
}

func (pb *playback) fromSession(s *adminSession) {
	pb.UserID, pb.Username, pb.ProfileName = s.UserID, s.Username, s.ProfileName
	pb.MediaType, pb.MediaTitle, pb.Series = s.MediaType, s.MediaTitle, s.SeriesName
	pb.EpisodeTitle = s.EpisodeName
	if pb.EpisodeTitle == "" && s.SeriesName != "" {
		pb.EpisodeTitle = s.MediaTitle
	}
	pb.Season, pb.Episode, pb.ContentID = s.SeasonNumber, s.EpisodeNumber, s.ContentID
	pb.PosterURL = s.PosterURL
	pb.DeviceID, pb.Platform, pb.ClientName, pb.ClientVersion = s.DeviceID, s.DevicePlatform, s.ClientName, s.ClientVersion
	pb.ClientLabel, pb.ClientIP, pb.Location = s.ClientLabel, s.ClientIP, s.StreamLocation
	pb.Method = s.EffectivePlayMethod
	if pb.Method == "" {
		pb.Method = s.PlayMethod
	}
	pb.SourceResolution, pb.TargetResolution, pb.HWAccel = s.SourceVideoResolution, s.TargetResolution, s.TranscodeHWAccel
	if s.StreamBitrateKbps != nil {
		pb.BitrateKbps = *s.StreamBitrateKbps
	}
	pb.Position = s.PositionSeconds
	if s.FileDuration != nil {
		pb.Duration = *s.FileDuration
	}
	pb.FromRow = true
	if at, err := time.Parse(time.RFC3339Nano, s.StartedAt); err == nil {
		pb.StartedAt = at
	}
}

func (pb *playback) fromHistory(h *adminHistory) {
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	pb.UserID, pb.Username, pb.ProfileName = h.UserID, h.Username, h.ProfileName
	pb.MediaType, pb.MediaTitle, pb.Series = h.MediaType, h.MediaTitle, h.SeriesTitle
	if h.SeriesTitle != "" {
		pb.EpisodeTitle = h.MediaTitle
	}
	pb.Season, pb.Episode, pb.ContentID = h.SeasonNumber, h.EpisodeNumber, h.MediaItemID
	pb.DeviceID, pb.Platform, pb.ClientName, pb.ClientVersion = str(h.DeviceID), str(h.DevicePlatform), str(h.ClientName), str(h.ClientVersion)
	pb.ClientIP = str(h.ClientIP)
	pb.Method = historyMethod(h)
	pb.Played, pb.Watched, pb.Completed = h.PlayedSeconds, h.WatchedSeconds, h.Completed
	if h.DurationSeconds != nil {
		pb.Duration = *h.DurationSeconds
	}
}

// historyMethod reads the play method off a history entry, which has no
// effective_play_method: copied video with converted audio is direct_stream.
func historyMethod(h *adminHistory) string {
	video, audio := "", ""
	if h.VideoDecision != nil {
		video = *h.VideoDecision
	}
	if h.AudioDecision != nil {
		audio = *h.AudioDecision
	}
	switch {
	case video == "transcode":
		return "transcode"
	case audio == "transcode" && (video == "direct" || video == "remux"):
		return "direct_stream"
	case video != "":
		return video
	}
	return h.PlayMethod
}

// locationFromIP guesses local or remote from an address when Silo's own
// classification isn't on the row (history entries don't carry it).
func locationFromIP(ip string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return ""
	}
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() {
		return "local"
	}
	// 100.64.0.0/10 (carrier-grade NAT, also Tailscale) counts as local.
	if addr.Is4() && netip.MustParsePrefix("100.64.0.0/10").Contains(addr) {
		return "local"
	}
	return "remote"
}

// showKey groups a user's plays for collapsing binges: the series for an
// episode, otherwise the item.
func (pb playback) showKey() string {
	switch {
	case pb.Series != "":
		return "series:" + strings.ToLower(pb.Series)
	case pb.ContentID != "":
		return "item:" + pb.ContentID
	}
	return "title:" + strings.ToLower(pb.MediaTitle)
}

// isTranscode reports whether the video is being converted.
func (pb playback) isTranscode() bool { return pb.Method == "transcode" }

// alertKind maps the event to an admin alert type.
func (pb playback) alertKind() string {
	switch pb.Event {
	case eventSessionStarted:
		return alertStreamStarted
	case eventSessionPaused, eventSessionResumed:
		return alertStreamPaused
	case eventSessionStopped:
		if pb.EndReason == endReasonAborted {
			return alertPlaybackFailed
		}
		return alertStreamStopped
	}
	return ""
}

// vars fills the admin alert placeholders.
func (pb playback) vars() map[string]string {
	v := map[string]string{
		"user":          pb.Username,
		"profile":       pb.ProfileName,
		"media_title":   pb.MediaTitle,
		"series":        pb.Series,
		"episode_title": pb.EpisodeTitle,
		"season":        intString(pb.Season),
		"episode":       intString(pb.Episode),
		"sxe":           seasonEpisode(pb.Season, pb.Episode),
		"media_type":    mediaTypeLabel(pb.MediaType),
		"platform":      pb.Platform,
		"ip":            pb.ClientIP,
		"location":      titleCase(pb.Location),
		"method":        methodLabel(pb.Method),
		"resolution":    pb.SourceResolution,
		"hw_accel":      pb.HWAccel,
		"position":      clock(pb.Position),
		"duration":      clock(pb.Duration),
		"end_reason":    endReasonLabel(pb.EndReason),
	}
	if v["user"] == "" {
		v["user"] = "User " + pb.UserID
	}
	// "Josh (Kids)" when watching on a profile other than the account's name.
	v["user_profile"] = v["user"]
	if pb.ProfileName != "" && !strings.EqualFold(pb.ProfileName, pb.Username) {
		v["user_profile"] = v["user"] + " (" + pb.ProfileName + ")"
	}
	title := pb.MediaTitle
	if pb.Series != "" {
		title = strings.TrimSpace(pb.Series + " " + v["sxe"])
	}
	v["title"] = title

	client := strings.TrimSpace(pb.ClientName + " " + pb.ClientVersion)
	v["client"] = client
	device := pb.ClientLabel
	if device == "" {
		device = pb.Platform
	}
	if device == "" {
		device = client
	}
	v["device"] = device

	v["video"] = pb.SourceResolution
	if pb.isTranscode() && pb.TargetResolution != "" && pb.TargetResolution != pb.SourceResolution {
		v["video"] = strings.TrimSpace(pb.SourceResolution + " → " + pb.TargetResolution)
	}
	v["bitrate"] = ""
	if pb.BitrateKbps > 0 {
		v["bitrate"] = fmt.Sprintf("%.1f Mbps", float64(pb.BitrateKbps)/1000)
	}

	v["action"] = ""
	switch pb.Event {
	case eventSessionPaused:
		v["action"] = "paused"
	case eventSessionResumed:
		v["action"] = "resumed"
	}

	v["watched"], v["progress"], v["finished"] = "", "", ""
	if pb.Event == eventSessionStopped {
		if pb.Played != nil {
			v["watched"] = humanDuration(*pb.Played)
		}
		v["position"] = clock(pb.Watched)
		if pb.Duration > 0 {
			pct := int(pb.Watched / pb.Duration * 100)
			v["progress"] = strconv.Itoa(min(max(pct, 0), 100)) + "%"
		}
		v["finished"] = "not finished"
		if pb.Completed {
			v["finished"] = "finished"
		}
	} else if pb.Duration > 0 && pb.Position > 0 {
		v["progress"] = strconv.Itoa(min(int(pb.Position/pb.Duration*100), 100)) + "%"
	}
	return v
}

func mediaTypeLabel(t string) string {
	switch t {
	case "movie":
		return "Movie"
	case "episode":
		return "Episode"
	case "audiobook":
		return "Audiobook"
	}
	return titleCase(t)
}

func methodLabel(m string) string {
	switch m {
	case "direct":
		return "Direct play"
	case "remux":
		return "Remux"
	case "direct_stream":
		return "Direct stream"
	case "transcode":
		return "Transcode"
	}
	return titleCase(strings.ReplaceAll(m, "_", " "))
}

func endReasonLabel(r string) string {
	switch r {
	case "stopped":
		return "stopped by the client"
	case "terminated":
		return "stopped by an admin"
	case "expired":
		return "the client went quiet"
	case endReasonAborted:
		return "Silo couldn't keep playing it"
	case endReasonReplaced:
		return "the client restarted playback"
	}
	return r
}

func titleCase(s string) string {
	// Cut at a character, not a byte: a first byte of a multi-byte character
	// would come out as a replacement character.
	first, size := utf8.DecodeRuneInString(s)
	if size == 0 || first == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(first)) + s[size:]
}

// clock formats seconds as 1:02:03 or 2:03.
func clock(seconds float64) string {
	if seconds <= 0 {
		return ""
	}
	s := int(seconds)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// humanDuration formats seconds as "1h 5m", "42m" or "30s".
func humanDuration(seconds float64) string {
	s := int(seconds)
	switch {
	case s <= 0:
		return "0m"
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm", s/60)
	case s%3600/60 == 0:
		return fmt.Sprintf("%dh", s/3600)
	}
	return fmt.Sprintf("%dh %dm", s/3600, s%3600/60)
}
