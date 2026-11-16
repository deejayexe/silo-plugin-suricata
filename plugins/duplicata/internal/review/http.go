package review

import (
 "context"
 "crypto/rand"
 "embed"
 "encoding/hex"
 "encoding/json"
 "errors"
 "fmt"
 "mime"
 "strconv"
 "strings"
 "sync"
 "time"

 pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

//go:embed web/*
var web embed.FS

type Handler struct {
 pluginv1.UnimplementedHttpRoutesServer
 ResolveBase func(context.Context)(string,error)
 mu sync.Mutex
 clientMu sync.Mutex
 cfg Config
 client *Client
 scan Scan
 cancel context.CancelFunc
 pending deletePlan
 history []historyEntry
 csrf string
}
func NewHandler()*Handler{
 b:=make([]byte,32);if _,err:=rand.Read(b);err!=nil{panic(err)}
 return &Handler{cfg:DefaultConfig(),csrf:hex.EncodeToString(b),scan:Scan{Status:"idle",Groups:[]Group{},Errors:[]string{}}}
}
func(h *Handler) Configure(cfg Config){
 h.clientMu.Lock();defer h.clientMu.Unlock()
 h.mu.Lock();defer h.mu.Unlock()
 if h.cancel!=nil {h.cancel();h.cancel=nil}
 h.archiveScanLocked()
 h.pending=deletePlan{}
 h.cfg=cfg;h.client=nil;h.scan=Scan{Status:"idle",Groups:[]Group{},Errors:[]string{}}
}
func(h *Handler) connect(ctx context.Context)(*Client,error){return h.connection(ctx,false)}
func(h *Handler) connection(ctx context.Context,refresh bool)(*Client,error){
 h.clientMu.Lock();defer h.clientMu.Unlock()
 if refresh{h.client=nil}
 if h.client!=nil{return h.client,nil}
 h.mu.Lock();cfg:=h.cfg;h.mu.Unlock()
 if err:=cfg.Validate();err!=nil{return nil,err}
 base:=cfg.BaseURL
 if base=="" {
  if h.ResolveBase==nil{return nil,ErrHostUnavailable}
  var err error;base,err=h.ResolveBase(ctx);if err!=nil{return nil,err}
 }
 client,err:=NewClient(base,cfg);if err!=nil{return nil,err}
 if err=client.Connect(ctx);err!=nil{return nil,err}
 h.client=client;return client,nil
}
func header(req *pluginv1.HandleHTTPRequest,name string)string{
 for k,v:=range req.GetHeaders(){if strings.EqualFold(k,name){return v}}
 return ""
}
func response(status int,contentType string,body []byte)*pluginv1.HandleHTTPResponse{
 return &pluginv1.HandleHTTPResponse{StatusCode:int32(status),Body:body,Headers:map[string]string{
  "Content-Type":contentType,"Cache-Control":"no-store","X-Content-Type-Options":"nosniff",
  "Content-Security-Policy":"default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'self'; form-action 'self'",
 }}
}
func jsonResponse(status int,value any)*pluginv1.HandleHTTPResponse{
 data,err:=json.Marshal(value);if err!=nil{return response(500,"application/json",[]byte(`{"error":"Internal error"}`))}
 if len(data)>3*1024*1024{return jsonResponse(413,map[string]string{"error":"Response too large; reduce the number of results"})}
 return response(status,"application/json; charset=utf-8",data)
}
func failure(status int,err error)*pluginv1.HandleHTTPResponse{return jsonResponse(status,map[string]string{"error":err.Error()})}
func(h *Handler) Handle(ctx context.Context,req *pluginv1.HandleHTTPRequest)(*pluginv1.HandleHTTPResponse,error){
 // The Silo proxy supplies these headers; there is no listener outside that proxy.
 if header(req,"X-Silo-User-Role")!="admin" || header(req,"X-Silo-User-Id")==""{
  return failure(403,errors.New("Administrators only")),nil
 }
 method,path:=req.GetMethod(),req.GetPath()
 if method=="POST" {
  if len(req.GetBody())>4096{return failure(413,errors.New("Request too large")),nil}
  typ,_,_:=mime.ParseMediaType(header(req,"Content-Type"))
  if typ!="application/json"{return failure(415,errors.New("Request must use JSON")),nil}
  // Silo forwards an allowlist of headers, excluding custom CSRF headers.
  // Keep the token in the JSON body, never in URLs or query strings.
  var envelope struct{CSRF string `json:"csrf"`}
  if err:=json.Unmarshal(req.GetBody(),&envelope);err!=nil{return failure(400,errors.New("Invalid JSON request")),nil}
  if envelope.CSRF!=h.csrf {
   return failure(403,errors.New("Page session expired; reload the page")),nil
  }
 }
 switch {
 case method=="GET" && (path=="/"||path==""):
  data,_:=web.ReadFile("web/index.html");return response(200,"text/html; charset=utf-8",data),nil
 case method=="GET"&&path=="/assets/logo.png":
  data,_:=web.ReadFile("web/logo.png");return response(200,"image/png",data),nil
 case method=="GET"&&(path=="/assets/app.js"||path=="/assets/app.css"):
  name:="app.js";typ:="text/javascript; charset=utf-8"
  if strings.HasSuffix(path,".css"){name="app.css";typ="text/css; charset=utf-8"}
  data,_:=web.ReadFile("web/"+name);return response(200,typ,data),nil
 case method=="GET"&&path=="/status":
  h.mu.Lock();defer h.mu.Unlock()
  // Return summaries on polls. Result pages are fetched separately.
  return jsonResponse(200,map[string]any{"version":"0.1.0","read_only":!h.cfg.DeleteEnabled,"csrf":h.csrf,
   "id":h.scan.ID,"status":h.scan.Status,"total":h.scan.Total,"processed":h.scan.Processed,
   "group_count":len(h.scan.Groups),"errors":h.scan.Errors}),nil
 case method=="GET"&&path=="/history":
  return h.historyList(),nil
 case method=="POST"&&path=="/history/restore":
  return h.restoreHistory(req),nil
 case method=="POST"&&path=="/history/remove":
  return h.removeHistory(req),nil
 case method=="GET"&&path=="/results":
  offset:=0
  if req.GetQuery()!=nil {
   if v:=req.GetQuery().GetFields()["offset"];v!=nil {offset,_=strconv.Atoi(v.GetStringValue())}
  }
  h.mu.Lock();defer h.mu.Unlock()
  selected:=h.selectedScanLocked(queryString(req,"scan_id"))
  if selected==nil{return failure(404,errors.New("Analysis is no longer in memory")),nil}
  if offset<0||offset>len(selected.Groups){return failure(400,errors.New("Invalid page")),nil}
  end:=min(offset+20,len(selected.Groups))
  return jsonResponse(200,map[string]any{"groups":selected.Groups[offset:end],"total":len(selected.Groups),"offset":offset,"scan_id":selected.ID,"status":selected.Status,"historical":selected.ID!=h.scan.ID,"library_name":selected.LibraryName,"errors":selected.Errors,"started_at":selected.StartedAt,"finished_at":selected.FinishedAt,"space":deletionSpace(selected.Groups)}),nil
 case method=="GET"&&(path=="/check"||path=="/libraries"):
  client,err:=h.connection(ctx,true);if err!=nil{return failure(502,err),nil}
  libraries,err:=client.Libraries(ctx);if err!=nil{return failure(502,err),nil}
  h.mu.Lock();enabled:=h.cfg.DeleteEnabled;h.mu.Unlock()
  return jsonResponse(200,map[string]any{"items":libraries,"profile_id":client.profile,"profile_name":client.profileName,"read_only":!enabled,"delete_enabled":enabled}),nil
 case method=="POST"&&(path=="/delete/preview"||path=="/delete/confirm"):
  return h.deletion(ctx,req,path=="/delete/confirm")
 case method=="GET"&&path=="/poster":
  return h.poster(ctx,req)
 case method=="POST"&&path=="/scan":
  var input struct{Library string `json:"library_id"`}
  if err:=json.Unmarshal(req.GetBody(),&input);err!=nil||input.Library==""{return failure(400,errors.New("Select a library")),nil}
  h.mu.Lock();running:=h.scan.Status=="running";h.mu.Unlock()
  if running{return failure(409,errors.New("A scan is already running")),nil}
  client,err:=h.connect(ctx);if err!=nil{return failure(502,err),nil}
  libraries,err:=client.Libraries(ctx);if err!=nil{return failure(502,err),nil}
  found:=false;for _,l:=range libraries{if l.ID==input.Library{found=true}}
  if !found{return failure(403,errors.New("Library unavailable for this profile")),nil}
  h.mu.Lock();defer h.mu.Unlock()
  if h.scan.Status=="running"{return failure(409,errors.New("A scan is already running")),nil}
  id:=fmt.Sprintf("%d",time.Now().UnixNano())
  h.archiveScanLocked()
  h.pending=deletePlan{}
  libraryName:="";for _,l:=range libraries{if l.ID==input.Library{libraryName=l.Name}}
  h.scan=Scan{ID:id,Library:input.Library,LibraryName:libraryName,StartedAt:time.Now().UTC(),Status:"running",Groups:[]Group{},Errors:[]string{}}
  scanCtx,cancel:=context.WithTimeout(context.Background(),scanTimeout);h.cancel=cancel
  workers:=h.cfg.Concurrency
  roots:=h.cfg.MediaRoots
  go func(){
   defer cancel()
   runScan(scanCtx,client,input.Library,workers,func(processed,total int){
    h.mu.Lock();defer h.mu.Unlock();if h.scan.ID==id{h.scan.Processed=processed;h.scan.Total=total}
   },func(groups []Group,errs []string,status string){
    h.mu.Lock();defer h.mu.Unlock()
    if h.scan.ID==id{captureFiles(groups,roots);h.scan.Groups=groups;h.scan.Errors=errs;h.scan.Status=status;h.scan.FinishedAt=time.Now().UTC();h.cancel=nil}
   })
  }()
  return jsonResponse(202,map[string]string{"id":id}),nil
 case method=="POST"&&path=="/cancel":
  h.mu.Lock();defer h.mu.Unlock()
  if h.cancel!=nil{h.cancel()}
  return jsonResponse(202,map[string]bool{"ok":true}),nil
 case method=="POST"&&path=="/clear":
  h.mu.Lock();defer h.mu.Unlock()
  if h.scan.Status=="running"{return failure(409,errors.New("Stop the scan first")),nil}
  h.archiveScanLocked()
  h.pending=deletePlan{}
  h.scan=Scan{Status:"idle",Groups:[]Group{},Errors:[]string{}}
  return jsonResponse(200,map[string]bool{"ok":true}),nil
 default:return failure(404,errors.New("Route unavailable")),nil
 }
}
