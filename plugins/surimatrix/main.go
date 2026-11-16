package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	runtime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"google.golang.org/protobuf/types/known/structpb"
)

//go:embed manifest.json
var manifest []byte

var version string

//go:embed web/index.html
var page []byte

//go:embed logo.png
var logo []byte

type config struct {
	Enabled      bool   `json:"enabled"`
	Homeserver   string `json:"homeserver"`
	UserID       string `json:"user_id"`
	AccessToken  string `json:"access_token"`
	DeviceID     string `json:"device_id"`
	RoomID       string `json:"room_id"`
	StateDir     string `json:"state_dir"`
	SiloURL      string `json:"silo_url"`
	SiloKey      string `json:"silo_key"`
	SiloUsername string `json:"silo_username"`
	MessageTitle string `json:"message_title"`
	profileID    string
	Libraries    []string `json:"libraries"`
}
type plugin struct {
	pb.UnimplementedScheduledTaskServer
	pb.UnimplementedHttpRoutesServer
}

var cfgMu sync.RWMutex
var current config

func configure(_ context.Context, entries []*pb.ConfigEntry) error {
	var c config
	for _, e := range entries {
		if e.GetKey() == "matrix" {
			b, _ := json.Marshal(e.GetValue().AsMap())
			if err := json.Unmarshal(b, &c); err != nil {
				return err
			}
		}
	}
	cfgMu.Lock()
	current = c
	cfgMu.Unlock()
	return nil
}
func loadConfig(ctx context.Context) (config, error) {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return current, nil
}
func (p *plugin) Run(ctx context.Context, req *pb.RunScheduledTaskRequest) (*pb.RunScheduledTaskResponse, error) {
	c, err := loadConfig(ctx)
	if err != nil {
		return nil, err
	}
	if !c.Enabled {
		return &pb.RunScheduledTaskResponse{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	err = withMatrix(ctx, c, func(m *matrixClient) error { return scan(ctx, c, m) })
	if err != nil {
		slog.Error("Surimatrix: content task failed", "error", err)
		return nil, err
	}
	out, _ := structpb.NewStruct(map[string]any{"status": "ok"})
	return &pb.RunScheduledTaskResponse{Output: out}, nil
}
func header(req *pb.HandleHTTPRequest, key string) string {
	for k, v := range req.GetHeaders() {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}
func response(code int, typ string, body []byte) *pb.HandleHTTPResponse {
	return &pb.HandleHTTPResponse{StatusCode: int32(code), Headers: map[string]string{"Content-Type": typ, "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff"}, Body: body}
}
func (p *plugin) Handle(ctx context.Context, req *pb.HandleHTTPRequest) (*pb.HandleHTTPResponse, error) {
	uid, e := strconv.Atoi(header(req, "X-Silo-User-Id"))
	if e != nil || uid < 1 || header(req, "X-Silo-User-Role") != "admin" || header(req, "X-Silo-Profile-Primary") != "true" {
		return response(403, "text/plain", []byte("Administrators on their primary profile only")), nil
	}
	if req.GetMethod() == http.MethodGet {
		switch req.GetPath() {
		case "", "/":
			return response(200, "text/html; charset=utf-8", page), nil
		case "/api/libraries":
			return p.handleLibraries(ctx), nil
		case "/logo.png":
			return response(200, "image/png", logo), nil
		}
	}
	if req.GetMethod() == http.MethodPost && (req.GetPath() == "/api/check-silo" || req.GetPath() == "/api/test-poster" || req.GetPath() == "/api/send-latest-movie" || req.GetPath() == "/api/send-latest-episode") {
		return p.handleSiloTest(ctx, req), nil
	}
	if req.GetMethod() != http.MethodPost || req.GetPath() != "/api/test" {
		return response(404, "text/plain", []byte("Not found")), nil
	}
	c, err := loadConfig(ctx)
	if err == nil {
		ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		err = withMatrix(ctx, c, func(m *matrixClient) error {
			return m.send(ctx, "test-"+strconv.FormatInt(time.Now().UnixNano(), 10), notificationTitle(ctx, c)+"\nEncrypted image test\nElement has decrypted the message and attachment if you can see the logo.", logo, "image/png")
		})
	}
	if err != nil {
		slog.Warn("Matrix test failed", "error", err)
		return response(502, "application/json", []byte(`{"error":"Unable to send. Check the connection settings, writable storage and encrypted room membership. See the Silo logs for details."}`)), nil
	}
	return response(200, "application/json", []byte(`{"ok":true}`)), nil
}
func main() {
	p := &plugin{}
	runtime.ServeManifestWithOptions(manifest, version, runtime.CapabilityServers{ScheduledTask: p, HttpRoutes: p}, runtime.WithConfigure(configure))
}
