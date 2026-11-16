// Duplicata Versions provides version review and optional file deletion for Silo.
package main

import (
 "context"
 _ "embed"
 "encoding/json"
 "runtime"

 pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
 sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
 "github.com/deejayexe/silo-plugin-duplicata-versions/internal/review"
)

//go:embed manifest.json
var manifestJSON []byte

var version = "0.1.1"

func main() {
 h := review.NewHandler()
 var m map[string]any
 if err := json.Unmarshal(manifestJSON, &m); err != nil { panic(err) }
 m["supported_platforms"] = []map[string]string{{"os":runtime.GOOS,"arch":runtime.GOARCH}}
 manifest, err := json.Marshal(m)
 if err != nil { panic(err) }
 h.ResolveBase = func(ctx context.Context) (string,error) {
  host := sdkruntime.Host()
  if host == nil { return "", review.ErrHostUnavailable }
  info, err := host.GetHostInfo(ctx)
  if err != nil { return "", review.ErrHostUnavailable }
  if info.InternalBaseURL != "" { return info.InternalBaseURL,nil }
  return "", review.ErrHostUnavailable
 }
 sdkruntime.ServeManifestWithOptions(manifest, version,
  sdkruntime.CapabilityServers{HttpRoutes:h},
  sdkruntime.WithConfigure(func(ctx context.Context, entries []*pluginv1.ConfigEntry) error {
   cfg := review.DefaultConfig()
   for _, entry := range entries {
    if entry == nil || entry.GetValue() == nil { continue }
    data, err := json.Marshal(entry.GetValue().AsMap())
    if err != nil { return err }
    if entry.GetKey() == "connection" || entry.GetKey() == "analysis" || entry.GetKey() == "deletion" {
     if err := json.Unmarshal(data,&cfg); err != nil { return err }
    }
   }
   // An incomplete configuration must not prevent plugin installation/startup.
   h.Configure(cfg)
   return nil
  }))
}
