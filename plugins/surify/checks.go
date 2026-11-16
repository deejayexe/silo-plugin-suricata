package main

import (
	"context"
	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"net/url"
)

type checkServer struct {
	pluginv1.UnimplementedConfigurationChecksServer
	plugin *Plugin
}

func (s *checkServer) TestConfiguration(_ context.Context, req *pluginv1.TestConfigurationRequest) (*pluginv1.TestConfigurationResponse, error) {
	cfg := parseAdminConfig(req.GetConfig())
	if cfg.PublicURL != "" {
		u, e := url.Parse(cfg.PublicURL)
		if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return &pluginv1.TestConfigurationResponse{Message: "Enter a valid Silo public URL."}, nil
		}
	}
	if cfg.DefaultAppToken != "" && (!validServerURL(cfg.DefaultServerURL) || !validApplicationToken(cfg.DefaultAppToken)) {
		return &pluginv1.TestConfigurationResponse{Message: "Enter a valid shared Gotify server URL and application token."}, nil
	}
	return &pluginv1.TestConfigurationResponse{Ok: true, Message: "Settings format is valid. Check the Gotify server and send a test notification from Admin > Surify to verify delivery."}, nil
}
