package main

import (
	"context"
	_ "embed"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
	// Embedded so time zones work on hosts without a zoneinfo database (a
	// minimal container): quiet hours and the digest hour depend on them.
	_ "time/tzdata"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
)

//go:embed manifest.json
var manifestJSON []byte

// version replaces manifest.json's version when set at build time with
// -ldflags "-X main.version=...". Dev builds use it (0.4.0-dev.12) so each push
// is newer than the last; release builds leave it empty.
var version string

//go:embed web/index.html
var indexHTML []byte

//go:embed web/surify-logo.webp
var surifyLogo []byte

// openAPIJSON describes the plugin's JSON API (served at api/openapi.json).
//
//go:embed web/openapi.json
var openAPIJSON []byte

// Plugin holds the state shared by the runtime, event and HTTP servers.
type Plugin struct {
	recentMu       sync.Mutex
	catalogMediaFn func(context.Context, runtimehost.ListLibraryMediaRequest) (*runtimehost.ListLibraryMediaResponse, error)
	logger         *slog.Logger
	store          *profileStore
	admins         *adminStore
	watch          *watchStore  // live sessions, noise control and limit marks
	seen           *seenStore   // known devices and addresses per account
	health         *healthStore // the health check's result and what admins were told
	gotify         *gotifyClient
	silo           *siloClient
	fetch          *http.Client // poster downloads
	// handled records the events already sent or permanently rejected.
	handled *eventLog
	// pending holds new episodes waiting to be sent as one group; flushMu keeps
	// one flush pass at a time in this process and flushEvery paces them.
	pending    *pendingStore
	flushMu    sync.Mutex
	flushEvery time.Duration

	cfgMu sync.RWMutex
	cfg   adminConfig
	// configured is set once Silo has called Configure, so the plugin knows
	// the stored config before it writes it back.
	configured atomic.Bool
	// adminKeyWarned is set once a refused read-only key has been logged, so
	// admin alerts log it once rather than on every event.
	adminKeyWarned atomic.Bool

	// keys creates and revokes Silo API keys during a key setup.
	keys *keyAdmin
	// commands stops streams with the stream-control key.
	commands *sessionCommander
	// keySetupMu lets one key setup run at a time in this process.
	keySetupMu sync.Mutex
	// setGlobalConfigFn rewrites the plugin's own config row (the host's
	// SetGlobalConfigEntry). It replaces the whole row and doesn't call
	// Configure, so callers update p.cfg themselves.
	setGlobalConfigFn func(ctx context.Context, key string, value map[string]any) error

	hostInfoFn func(ctx context.Context) (*runtimehost.HostInfo, error)
	hostMu     sync.Mutex
	hostInfo   *runtimehost.HostInfo
	hostAt     time.Time

	// resolveImagesFn turns catalog image paths into URLs (the host's
	// ResolveCatalogImageURLs).
	resolveImagesFn func(ctx context.Context, paths []string, variant string) (map[string]string, error)

	// listLibrariesFn lists Silo's libraries (the host's ListLibraries).
	listLibrariesFn func(ctx context.Context) ([]siloLibrary, error)
	libsMu          sync.Mutex
	libsList        []siloLibrary
	libsAt          time.Time

	// eventTimeout bounds one HandleEvent call. Silo gives each attempt 10 s.
	eventTimeout time.Duration
	// posterTimeout bounds the poster lookup and download for one message.
	posterTimeout time.Duration
	// taskTimeout bounds one scheduled task run; taskNow is its clock.
	taskTimeout time.Duration
	taskNow     func() time.Time
}

const (
	defaultEventTimeout  = 8 * time.Second
	defaultPosterTimeout = 3 * time.Second
)

func newPlugin(logger *slog.Logger, state stateBackend) *Plugin {
	gotify := newGotifyClient()
	return &Plugin{
		logger:            logger,
		catalogMediaFn:    sdkCatalogMedia,
		store:             newProfileStore(state),
		admins:            newAdminStore(state),
		watch:             newWatchStore(state),
		seen:              newSeenStore(state),
		health:            newHealthStore(state),
		gotify:            gotify,
		silo:              newSiloClient(),
		keys:              newKeyAdmin(),
		commands:          newSessionCommander(),
		fetch:             &http.Client{Timeout: defaultPosterTimeout},
		handled:           newEventLog(state),
		pending:           newPendingStore(state, logger),
		flushEvery:        flushEvery,
		hostInfoFn:        sdkHostInfo,
		resolveImagesFn:   sdkResolveImages,
		listLibrariesFn:   sdkListLibraries,
		setGlobalConfigFn: sdkSetGlobalConfig,
		eventTimeout:      defaultEventTimeout,
		posterTimeout:     defaultPosterTimeout,
		taskTimeout:       taskTimeout,
		taskNow:           time.Now,
	}
}

func (p *Plugin) config() adminConfig {
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	return p.cfg
}

func (p *Plugin) setConfig(cfg adminConfig) {
	p.cfgMu.Lock()
	p.cfg = cfg
	p.cfgMu.Unlock()
	p.silo.Invalidate()
	p.adminKeyWarned.Store(false)
}

// host returns the cached host info, refreshing it every minute. Public URLs
// can change with a Silo config reload.
func (p *Plugin) host(ctx context.Context) (*runtimehost.HostInfo, error) {
	p.hostMu.Lock()
	defer p.hostMu.Unlock()
	if p.hostInfo != nil && time.Since(p.hostAt) < time.Minute {
		return p.hostInfo, nil
	}
	info, err := p.hostInfoFn(ctx)
	if err != nil {
		if p.hostInfo != nil {
			return p.hostInfo, nil
		}
		return nil, err
	}
	p.hostInfo, p.hostAt = info, time.Now()
	return info, nil
}

func sdkHostInfo(ctx context.Context) (*runtimehost.HostInfo, error) {
	h := sdkruntime.Host()
	if h == nil {
		return nil, errHostUnavailable
	}
	return h.GetHostInfo(ctx)
}

func sdkResolveImages(ctx context.Context, paths []string, variant string) (map[string]string, error) {
	h := sdkruntime.Host()
	if h == nil {
		return nil, errHostUnavailable
	}
	return h.ResolveCatalogImageURLs(ctx, paths, variant)
}

func sdkListLibraries(ctx context.Context) ([]siloLibrary, error) {
	h := sdkruntime.Host()
	if h == nil {
		return nil, errHostUnavailable
	}
	libs, err := h.ListLibraries(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]siloLibrary, 0, len(libs))
	for _, l := range libs {
		if l.GetId() != "" {
			out = append(out, siloLibrary{ID: l.GetId(), Name: l.GetName()})
		}
	}
	return out, nil
}

func sdkSetGlobalConfig(ctx context.Context, key string, value map[string]any) error {
	h := sdkruntime.Host()
	if h == nil {
		return errHostUnavailable
	}
	return h.SetGlobalConfigEntry(ctx, key, value)
}

// saveConfig writes cfg as the plugin's stored config and, once that worked,
// makes it the running config. Other API servers' plugin processes keep their
// copy until Silo next calls their Configure (a restart or an admin save).
func (p *Plugin) saveConfig(ctx context.Context, cfg adminConfig) error {
	if err := p.setGlobalConfigFn(ctx, adminConfigKey, cfg.storedValue()); err != nil {
		return err
	}
	p.setConfig(cfg)
	return nil
}

// configure takes the saved settings Silo hands the plugin when it starts.
func (p *Plugin) configure(_ context.Context, entries []*pluginv1.ConfigEntry) error {
	cfg := parseAdminConfig(entries)
	p.setConfig(cfg)
	p.configured.Store(true)
	if cfg.LegacyKeyStored {
		// Only the fact, never the value.
		p.logger.Info("the unscoped Silo API key saved by version 0.2.0 is still stored with this plugin's settings; it is ignored. Replace it from the admin section of the Gotify page, or revoke it under Admin > API Keys.")
	}
	return nil
}

func main() {
	manifest, err := publicmanifest.LoadWithChecksum(manifestJSON, version)
	if err != nil {
		panic(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("plugin", manifest.GetPluginId())
	plugin := newPlugin(logger, hostStateBackend{})
	// Sends grouped new-episode notifications when their window ends: once now,
	// then on a timer. Until the host connection is bound a pass just waits.
	go plugin.runFlusher(context.Background())

	// ServeManifestWithOptions answers GetManifest and BindHostBroker itself
	// and, because the manifest declares configuration_check, refuses to start
	// without the ConfigurationChecks server.
	sdkruntime.ServeManifestWithOptions(manifestJSON, version, sdkruntime.CapabilityServers{
		EventConsumer: &eventServer{plugin: plugin},
		HttpRoutes:    &routesServer{plugin: plugin},
		ScheduledTask: &taskServer{plugin: plugin},
	},
		sdkruntime.WithConfigure(plugin.configure),
		sdkruntime.WithConfigurationChecks(&checkServer{plugin: plugin}),
	)
}

func sdkCatalogMedia(ctx context.Context, req runtimehost.ListLibraryMediaRequest) (*runtimehost.ListLibraryMediaResponse, error) {
	h := sdkruntime.Host()
	if h == nil {
		return nil, errHostUnavailable
	}
	return h.ListLibraryMedia(ctx, req)
}
