package main

import (
 "testing"
 "encoding/json"
 pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
)

func TestManifestMatchesSDK(t *testing.T){
 m,err:=manifest.Load(manifestJSON)
 if err!=nil{t.Fatal(err)}
 if m.GetPluginId()!="community.duplicata-versions"{t.Fatal("unexpected plugin ID")}
 for _,route:=range m.GetHttpRoutes(){
  if route.GetAccess()!="admin"{t.Fatal("non-admin route")}
 }
}

// Silo's repository loader uses encoding/json, not protojson. Named enums
// accepted by the SDK manifest loader are invalid in repository.json.
func TestManifestCompatibleWithCatalogDecoder(t *testing.T){
 var m pluginv1.PluginManifest
 if err:=json.Unmarshal(manifestJSON,&m);err!=nil{t.Fatal(err)}
 if len(m.GetGlobalConfigSchema())!=3{t.Fatal("missing configuration sections")}
}
