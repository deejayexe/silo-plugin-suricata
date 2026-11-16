package main

import (
	"fmt"
	"html"
	"maps"
	"regexp"
	"strings"
)

// notificationType describes one kind of Silo notification a profile can
// route to Gotify, with its default wording.
type notificationType struct {
	Key            string   `json:"key"`
	Label          string   `json:"label"`
	Description    string   `json:"description"`
	DefaultTitle   string   `json:"default_title"`
	DefaultMessage string   `json:"default_message"`
	DefaultEnabled bool     `json:"default_enabled"`
	Placeholders   []string `json:"placeholders"`
	// Sample fills the placeholders for previews and test sends.
	Sample map[string]string `json:"sample"`

	// The group fields are for types whose deliveries can be merged into one
	// push (a season drop): the wording of the merged push, its placeholders
	// and sample values. Empty for the others.
	DefaultGroupTitle   string            `json:"default_group_title,omitempty"`
	DefaultGroupMessage string            `json:"default_group_message,omitempty"`
	GroupPlaceholders   []string          `json:"group_placeholders,omitempty"`
	GroupSample         map[string]string `json:"group_sample,omitempty"`
}

// groupable reports whether deliveries of the type can be merged.
func (t notificationType) groupable() bool { return t.DefaultGroupTitle != "" }

func (t notificationType) defaults() TypeSettings {
	return TypeSettings{Enabled: t.DefaultEnabled, Title: t.DefaultTitle, Message: t.DefaultMessage}
}

// otherTypeKey covers delivery types this plugin has no dedicated entry for.
// Silo documents its delivery type registry as open, so new types land here
// until the plugin learns about them.
const otherTypeKey = "other"

var commonPlaceholders = []string{"profile", "type_label"}

var requestPlaceholders = []string{"title", "year", "title_year", "media_type"}

var notificationTypes = []notificationType{
	{
		Key:            "episode.available",
		Label:          "New episode available",
		Description:    "A new episode of a show you favorited, watchlisted or are watching was added.",
		DefaultTitle:   "New episode: {series}",
		DefaultMessage: "{sxe} · {episode_title}",
		DefaultEnabled: true,
		Placeholders:   append([]string{"series", "episode_title", "season", "episode", "sxe", "reasons"}, commonPlaceholders...),
		Sample: map[string]string{
			"series": "Severance", "episode_title": "Hello, Ms. Cobel", "season": "2", "episode": "1",
			"sxe": "S02E01", "reasons": "Watchlist, Next Up",
		},
		DefaultGroupTitle:   "{series}: {count} new episodes ({range})",
		DefaultGroupMessage: "{episodes}",
		GroupPlaceholders:   append([]string{"series", "count", "range", "first", "last", "episodes", "reasons"}, commonPlaceholders...),
		GroupSample: map[string]string{
			"series": "Severance", "count": "3", "range": "S02E01 to S02E03", "first": "S02E01", "last": "S02E03",
			"episodes": "S02E01 · Hello, Ms. Cobel\nS02E02 · Goodbye, Mrs. Selvig\nS02E03 · Who Is Alive?",
			"reasons":  "Watchlist, Next Up",
		},
	},
	{
		Key:            "request.approved",
		Label:          "Request approved",
		Description:    "An admin approved something you requested.",
		DefaultTitle:   "Request approved",
		DefaultMessage: "{title_year} was approved and is on its way.",
		DefaultEnabled: true,
		Placeholders:   append(append([]string{}, requestPlaceholders...), commonPlaceholders...),
		Sample: map[string]string{
			"title": "Dune: Part Two", "year": "2024", "title_year": "Dune: Part Two (2024)", "media_type": "Movie",
		},
	},
	{
		Key:            "request.declined",
		Label:          "Request declined",
		Description:    "An admin declined something you requested.",
		DefaultTitle:   "Request declined",
		DefaultMessage: "{title_year} was declined. {decline_reason}",
		DefaultEnabled: true,
		Placeholders:   append(append([]string{}, requestPlaceholders...), append([]string{"decline_reason"}, commonPlaceholders...)...),
		Sample: map[string]string{
			"title": "Dune: Part Two", "year": "2024", "title_year": "Dune: Part Two (2024)", "media_type": "Movie",
			"decline_reason": "It's already in the 4K library.",
		},
	},
	{
		Key:            "request.fulfilled",
		Label:          "Request available",
		Description:    "Something you requested is now in the library.",
		DefaultTitle:   "Now available",
		DefaultMessage: "{title_year} is ready to watch.",
		DefaultEnabled: true,
		Placeholders:   append(append([]string{}, requestPlaceholders...), commonPlaceholders...),
		Sample: map[string]string{
			"title": "Dune: Part Two", "year": "2024", "title_year": "Dune: Part Two (2024)", "media_type": "Movie",
		},
	},
	{
		Key:            otherTypeKey,
		Label:          "Other Silo notices",
		Description:    "Anything else Silo posts to your inbox, such as one of your webhooks being turned off after repeated failures.",
		DefaultTitle:   "Silo",
		DefaultMessage: "{type_label}",
		DefaultEnabled: true,
		Placeholders:   append([]string{"type"}, commonPlaceholders...),
		Sample:         map[string]string{"type": "webhook.auto_disabled", "type_label": "Webhook auto disabled"},
	},
}

// notificationTypeByKey returns the entry for a delivery type, falling back
// to the "other" entry for types without one.
func notificationTypeByKey(key string) notificationType {
	for _, t := range notificationTypes {
		if t.Key == key {
			return t
		}
	}
	return notificationTypes[len(notificationTypes)-1]
}

// humanizeType turns "webhook.auto_disabled" into "Webhook auto disabled".
func humanizeType(deliveryType string) string {
	s := strings.NewReplacer(".", " ", "_", " ").Replace(deliveryType)
	if s == "" {
		return "Notification"
	}
	return titleCase(s)
}

func seasonEpisode(season, episode *int) string {
	switch {
	case season != nil && episode != nil:
		return fmt.Sprintf("S%02dE%02d", *season, *episode)
	case episode != nil:
		return fmt.Sprintf("E%02d", *episode)
	default:
		return ""
	}
}

var placeholderPattern = regexp.MustCompile(`\{([a-z_]+)\}`)

// renderTemplate substitutes {placeholder}s line by line. Unknown
// placeholders are left as typed so mistakes are visible. A line whose known
// placeholders all came out empty is dropped ("Watched {watched}" with
// nothing watched), and separators left dangling or doubled by an empty
// value ("S02E01 · ", "Remote · · 8 Mbps") are tidied.
func renderTemplate(tmpl string, vars map[string]string, escapeHTML bool) string {
	lines := strings.Split(strings.ReplaceAll(tmpl, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			out = append(out, "")
			continue
		}
		known, filled := 0, 0
		rendered := placeholderPattern.ReplaceAllStringFunc(line, func(match string) string {
			value, ok := vars[match[1:len(match)-1]]
			if !ok {
				return match
			}
			known++
			if strings.TrimSpace(value) != "" {
				filled++
			}
			if escapeHTML {
				return html.EscapeString(value)
			}
			return value
		})
		if known > 0 && filled == 0 {
			continue
		}
		// A value can hold several lines (a list of streams); tidy each.
		for _, sub := range strings.Split(rendered, "\n") {
			if sub = tidyLine(sub); sub != "" {
				out = append(out, sub)
			}
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

var emptyBrackets = strings.NewReplacer("()", "", "[]", "")

// ";" is deliberately absent: it ends HTML entities in HTML mode.
const danglingSeparators = " \t·•|,:–—"

// separatorWords are the words that only separate other words; a run of
// them left by empty values is cut to one.
var separatorWords = map[string]bool{"·": true, "•": true, "|": true, "–": true, "—": true}

func tidyLine(line string) string {
	line = emptyBrackets.Replace(line)
	words := strings.Fields(line)
	kept := words[:0]
	for _, w := range words {
		if separatorWords[w] && len(kept) > 0 && separatorWords[kept[len(kept)-1]] {
			continue
		}
		kept = append(kept, w)
	}
	line = strings.Join(kept, " ")
	line = strings.TrimLeft(line, danglingSeparators)
	return strings.TrimRight(line, danglingSeparators+"-")
}

// Admin alert types. Each is a switch in an admin's settings.
const (
	alertStreamStarted  = "stream.started"
	alertStreamPaused   = "stream.paused" // paused and resumed
	alertStreamStopped  = "stream.stopped"
	alertPlaybackFailed = "playback.failed"

	alertStreamsUser   = "streams.user"
	alertStreamsServer = "streams.server"
	alertBandwidth     = "bandwidth"
	alertNewDevice     = "device.new"
	alertNewIP         = "ip.new"

	alertScanFailed = "scan.failed"
	alertJobFailed  = "job.failed"

	// Scheduled tasks. alertHealthRecovered is the wording for the recovery
	// push and has no switch of its own: it follows alertHealth.
	alertHealth          = "health"
	alertHealthRecovered = "health.recovered"
	alertDigest          = "digest.weekly"
)

// Alert groups, shown as headings on the page.
const (
	groupPlayback = "playback"
	groupWatchdog = "watchdog"
	groupServer   = "server"
	// groupScheduled alerts come from scheduled tasks rather than Silo's
	// events.
	groupScheduled = "scheduled"
)

// adminAlertType describes one admin alert with its default wording.
type adminAlertType struct {
	Key            string   `json:"key"`
	Group          string   `json:"group"`
	Label          string   `json:"label"`
	Description    string   `json:"description"`
	DefaultTitle   string   `json:"default_title"`
	DefaultMessage string   `json:"default_message"`
	Placeholders   []string `json:"placeholders"`
	// NoiseControlled alerts follow the admin's noise mode; the others
	// always send.
	NoiseControlled bool `json:"noise_controlled"`
	// Limits are the numbers the alert compares against.
	Limits []alertLimit `json:"limits,omitempty"`
	// Parent names the alert whose switch this one follows; it only has its
	// own wording.
	Parent string `json:"parent,omitempty"`
	// Sample fills the placeholders for previews and test sends.
	Sample map[string]string `json:"sample"`
}

// alertLimit describes a limit alert's number field. Key is its field in
// AdminLimits.
type alertLimit struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Unit  string `json:"unit"`
	Max   int    `json:"max"`
	// Optional limits may stay empty (zero) with the alert on; the check they
	// belong to is then skipped.
	Optional bool `json:"optional,omitempty"`
}

func (t adminAlertType) defaults() AlertSettings {
	return AlertSettings{Title: t.DefaultTitle, Message: t.DefaultMessage}
}

// playbackPlaceholders are filled on every playback alert.
var playbackPlaceholders = []string{
	"user", "profile", "user_profile", "title", "media_title", "series", "episode_title", "season", "episode", "sxe",
	"media_type", "device", "client", "platform", "ip", "location", "method",
}

func withPlaceholders(extra ...string) []string {
	return append(append([]string{}, playbackPlaceholders...), extra...)
}

// playbackSample is a remote transcode of an episode, for previews and tests.
var playbackSample = map[string]string{
	"user": "alice", "profile": "Alice", "user_profile": "alice (Alice)", "title": "Severance S02E01",
	"media_title": "Hello, Ms. Cobel", "series": "Severance", "episode_title": "Hello, Ms. Cobel",
	"season": "2", "episode": "1", "sxe": "S02E01", "media_type": "Episode",
	"device": "Living Room TV", "client": "Silo for Android TV 2.4.1", "platform": "android_tv",
	"ip": "203.0.113.24", "location": "Remote", "method": "Transcode",
	"video": "4K → 1080p", "resolution": "4K", "bitrate": "8.0 Mbps", "hw_accel": "nvenc",
	"action": "paused", "position": "12:34", "duration": "57:10", "progress": "22%",
	"watched": "42m", "finished": "not finished", "end_reason": "Silo couldn't keep playing it",
}

// sampleWith is the playback sample plus extra values.
func sampleWith(extra map[string]string) map[string]string {
	s := maps.Clone(playbackSample)
	maps.Copy(s, extra)
	return s
}

const sampleStreams = "alice (Alice) · Severance S02E01 · Living Room TV · 8.0 Mbps\n" +
	"alice (Alice) · Dune: Part Two · iPad · 12.0 Mbps\n" +
	"bob · The Bear S03E02 · Chrome · 4.0 Mbps"

var adminAlertTypes = []adminAlertType{
	{
		Key:            alertRecentlyAdded,
		Group:          groupScheduled,
		Label:          "New library content",
		Description:    "New movies and episodes added after enabling this alert. Checks the catalog on the recently_added schedule; no initial library flood. Posters and overview appear when the Silo host exposes them. Library/media-type filters apply; playback-only filters do not.",
		DefaultTitle:   "New in Silo: {title}",
		DefaultMessage: "{media_type} · {year}\n{quality}\n{rating} · {genres}\n{overview}",
		Placeholders:   []string{"title", "year", "media_type", "quality", "rating", "genres", "content_rating", "overview", "libraries"},
		Sample: map[string]string{
			"title": "Dune: Part Two", "year": "2024", "media_type": "Movie",
			"quality": "2160p · hevc · truehd", "rating": "TMDB 8.2 / 10",
			"genres": "Science fiction, Adventure", "content_rating": "PG-13",
			"overview": "Paul Atreides joins the Fremen.", "libraries": "Movies",
		},
	},
	{
		Key:             alertStreamStarted,
		Group:           groupPlayback,
		Label:           "Stream started",
		Description:     "Someone starts playing something.",
		DefaultTitle:    "{user_profile} started {title}",
		DefaultMessage:  "{episode_title}\n{device} · {client}\n{location} · {method} · {video} · {bitrate}",
		Placeholders:    withPlaceholders("video", "resolution", "bitrate", "hw_accel", "position", "progress"),
		NoiseControlled: true,
		Sample:          playbackSample,
	},
	{
		Key:             alertStreamPaused,
		Group:           groupPlayback,
		Label:           "Paused / resumed",
		Description:     "A stream is paused or resumed.",
		DefaultTitle:    "{user_profile} {action} {title}",
		DefaultMessage:  "{episode_title}\nAt {position} ({progress})\n{device}",
		Placeholders:    withPlaceholders("action", "position", "duration", "progress"),
		NoiseControlled: true,
		Sample:          playbackSample,
	},
	{
		Key:             alertStreamStopped,
		Group:           groupPlayback,
		Label:           "Stream stopped",
		Description:     "A stream ends.",
		DefaultTitle:    "{user_profile} stopped {title}",
		DefaultMessage:  "{episode_title}\nWatched {watched}\n{progress} · {finished}\n{device}",
		Placeholders:    withPlaceholders("watched", "position", "duration", "progress", "finished", "end_reason"),
		NoiseControlled: true,
		Sample:          playbackSample,
	},
	{
		Key:            alertPlaybackFailed,
		Group:          groupPlayback,
		Label:          "Playback failed",
		Description:    "Silo gave up on a stream, for example because the file is missing or the connection to the client failed.",
		DefaultTitle:   "Playback failed for {user_profile}",
		DefaultMessage: "{title}\n{episode_title}\n{device} · {client}\nAt {position}",
		Placeholders:   withPlaceholders("position", "end_reason"),
		Sample:         playbackSample,
	},
	{
		Key:            alertStreamsUser,
		Group:          groupWatchdog,
		Label:          "Too many streams for one person",
		Description:    "One account plays more streams at once than the limit. Sent once when the limit is crossed, and again only after they've dropped back to the limit or under.",
		DefaultTitle:   "{user} is playing {count} streams",
		DefaultMessage: "The limit is {limit}.\n{streams}",
		Placeholders:   withPlaceholders("count", "limit", "streams"),
		Limits:         []alertLimit{{Key: "user_streams", Label: "Alert when one account plays more than", Unit: "streams", Max: maxUserStreams}},
		Sample:         sampleWith(map[string]string{"count": "3", "limit": "2", "streams": sampleStreams}),
	},
	{
		Key:            alertStreamsServer,
		Group:          groupWatchdog,
		Label:          "Too many streams on the server",
		Description:    "More streams play at once, across everyone, than the limit. Sent once when the limit is crossed, and again only after it has dropped back to the limit or under.",
		DefaultTitle:   "{count} streams playing",
		DefaultMessage: "The limit is {limit}.\n{streams}",
		Placeholders:   withPlaceholders("count", "limit", "streams"),
		Limits:         []alertLimit{{Key: "server_streams", Label: "Alert when more streams than this play at once", Unit: "streams", Max: maxServerStreams}},
		Sample:         sampleWith(map[string]string{"count": "3", "limit": "2", "streams": sampleStreams}),
	},
	{
		Key:            alertBandwidth,
		Group:          groupWatchdog,
		Label:          "Bandwidth over the limit",
		Description:    "The streams playing at once add up to more than the limit, by the bitrate Silo reports for each. Sent once when the limit is crossed, and again only after it has dropped back under.",
		DefaultTitle:   "Streaming {total}",
		DefaultMessage: "Over the limit of {limit}, across {count} streams.\n{streams}",
		Placeholders:   withPlaceholders("total", "limit", "count", "streams"),
		Limits:         []alertLimit{{Key: "bandwidth_mbps", Label: "Alert when the total is over", Unit: "Mbps", Max: maxBandwidthMbps}},
		Sample:         sampleWith(map[string]string{"total": "24.0 Mbps", "limit": "20 Mbps", "count": "3", "streams": sampleStreams}),
	},
	{
		Key:            alertNewDevice,
		Group:          groupWatchdog,
		Label:          "New device",
		Description:    "Someone plays on a device their account hasn't used before. The first time the plugin sees an account it learns the devices Silo already knows for it, so you aren't told about those.",
		DefaultTitle:   "New device for {user_profile}",
		DefaultMessage: "{device} · {client}\n{location} · {ip}\nPlaying {title}",
		Placeholders:   withPlaceholders(),
		Sample:         playbackSample,
	},
	{
		Key:            alertNewIP,
		Group:          groupWatchdog,
		Label:          "New IP address",
		Description:    "Someone plays from a remote address their account hasn't used before. Local addresses are skipped, and IPv6 addresses count by network (/64), since phones change theirs often. The first time the plugin sees an account it learns the addresses Silo saw in the last 90 days.",
		DefaultTitle:   "New IP address for {user_profile}",
		DefaultMessage: "{ip} · {location}\n{device} · {client}\nPlaying {title}",
		Placeholders:   withPlaceholders(),
		Sample:         playbackSample,
	},
	{
		Key:            alertScanFailed,
		Group:          groupServer,
		Label:          "Library scan failed",
		Description:    "A library scan stops with an error.",
		DefaultTitle:   "Scan failed: {library}",
		DefaultMessage: "{error}\n{scan} · {path}",
		Placeholders:   []string{"library", "error", "scan", "path", "trigger"},
		Sample: map[string]string{
			"library": "Movies", "error": "open /media/movies: input/output error", "scan": "Full scan",
			"path": "/media/movies", "trigger": "Scheduled",
		},
	},
	{
		Key:            alertJobFailed,
		Group:          groupServer,
		Label:          "Job failed",
		Description:    "A background job Silo runs for admins (a library refresh, catalog import or export, storage move and the like) fails.",
		DefaultTitle:   "Job failed: {job}",
		DefaultMessage: "{error}\n{details}",
		Placeholders:   []string{"job", "error", "details"},
		Sample: map[string]string{
			"job": "Library refresh", "error": "metadata provider timed out", "details": "Refreshed 120 of 480 items",
		},
	},
	{
		Key:            alertHealth,
		Group:          groupScheduled,
		Label:          "Server health problems",
		Description:    "A scheduled check of Silo's health finds something new wrong: a node that isn't healthy, Silo needing a restart, the database or Redis not answering, or the server's disk or memory over your limits. You hear about each problem once, and again only if it comes back after a recovery.",
		DefaultTitle:   "Silo needs attention",
		DefaultMessage: "{problems}\n{ongoing}",
		Placeholders:   []string{"problems", "count", "ongoing"},
		Limits: []alertLimit{
			{Key: "disk_pct", Label: "Alert when a disk is more than", Unit: "% full", Max: maxPercentLimit, Optional: true},
			{Key: "memory_pct", Label: "Alert when memory use is over", Unit: "%", Max: maxPercentLimit, Optional: true},
		},
		Sample: map[string]string{
			"problems": "Transcode node “gpu-1” isn't healthy\nDisk “library-1” is 93% full (limit 90%)",
			"count":    "2",
			"ongoing":  "Still open: Silo needs a restart (plugin task binding)",
		},
	},
	{
		Key:            alertHealthRecovered,
		Group:          groupScheduled,
		Parent:         alertHealth,
		Label:          "Server recovered",
		Description:    "Sent when a problem you were told about is fixed. It's on whenever server health problems are.",
		DefaultTitle:   "Silo recovered",
		DefaultMessage: "{resolved}\n{ongoing}",
		Placeholders:   []string{"resolved", "count", "ongoing"},
		Sample: map[string]string{
			"resolved": "Transcode node “gpu-1” is healthy again\nDisk “library-1” is down to 84%",
			"count":    "2",
			"ongoing":  "",
		},
	},
	{
		Key:            alertDigest,
		Group:          groupScheduled,
		Label:          "Weekly digest",
		Description:    "A summary of the last 7 days: plays, time watched, who watched the most and the most played titles. The users you ignore are left out of the top viewers.",
		DefaultTitle:   "Silo this week",
		DefaultMessage: "{plays} plays · {watch_time} watched\n{profiles} profiles · {transcode_pct} transcoded · {completion} finished\n{top_users}\n{top_titles}",
		Placeholders:   []string{"plays", "watch_time", "profiles", "transcodes", "transcode_pct", "completion", "days", "top_users", "top_titles"},
		Sample: map[string]string{
			"plays": "128", "watch_time": "41h 20m", "profiles": "6", "transcodes": "41", "transcode_pct": "32%",
			"completion": "78%", "days": "7",
			"top_users":  "Top viewers\n1. alice (Kids) · 31 plays · 12h 5m\n2. bob (Bob) · 22 plays · 9h 40m\n3. sam (Sam) · 18 plays · 7h 2m",
			"top_titles": "Top titles\n1. Severance · 14 plays\n2. The Bear · 9 plays\n3. Dune: Part Two · 6 plays",
		},
	},
}

// adminAlertTypeByKey returns an alert type, or false for an unknown key.
func adminAlertTypeByKey(key string) (adminAlertType, bool) {
	for _, t := range adminAlertTypes {
		if t.Key == key {
			return t, true
		}
	}
	return adminAlertType{}, false
}

// isPlaybackAlert reports whether an alert is about one stream's playback,
// which the playback filters and the library filter apply to.
func isPlaybackAlert(key string) bool {
	t, _ := adminAlertTypeByKey(key)
	return t.Group == groupPlayback
}
