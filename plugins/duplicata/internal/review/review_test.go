package review

import (
 "context"
 "encoding/json"
 "net/http"
 "net/http/httptest"
 "net/url"
 "strings"
 "testing"
 "time"

 pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

func TestConfiguration(t *testing.T){
 c:=DefaultConfig()
 if c.Validate()==nil{t.Fatal("empty API key accepted")}
 c.APIKey="test"
 if err:=c.Validate();err!=nil{t.Fatal(err)}
 for _,raw:=range []string{"file:///etc/passwd","http://u:p@host","https://host?token=x"}{
  c.BaseURL=raw;if c.Validate()==nil{t.Fatal(raw)}
 }
}
func TestIdentityNeverUsesTitleAlone(t *testing.T){
 a:=Item{ID:"a",Title:"Same",Type:"movie"};b:=Item{ID:"b",Title:"Same",Type:"movie"}
 if identity("1",a)==identity("1",b){t.Fatal("title-only grouping")}
 a.TMDB=json.RawMessage("123");b.TMDB=json.RawMessage("123")
 if identity("1",a)!=identity("1",b){t.Fatal("provider identity ignored")}
 if identity("1",a)==identity("2",b){t.Fatal("libraries crossed")}
}
func TestMembershipFilteringAndPathDeduplication(t *testing.T){
 groups:=map[string]*Group{}
 item:=Item{ID:"a",Title:"Movie",Type:"movie"}
 addGroup(groups,"lib",item,[]Version{{ID:"1"},{ID:"2"},{ID:"3"}},map[string]string{"1":"/one","2":"/one"})
 if len(finalize(groups))!=0{t.Fatal("one physical path treated as duplicate")}
 addGroup(groups,"lib",item,[]Version{{ID:"3"}},map[string]string{"3":"/two"})
 if len(finalize(groups))!=1{t.Fatal("missing duplicate")}
}
func TestAdminAndCSRFRequired(t *testing.T){
 h:=NewHandler()
 req:=&pluginv1.HandleHTTPRequest{Method:"GET",Path:"/status"}
 r,_:=h.Handle(context.Background(),req);if r.StatusCode!=403{t.Fatal(r)}
 req.Headers=map[string]string{"X-Silo-User-Role":"admin","X-Silo-User-Id":"1"}
 r,_=h.Handle(context.Background(),req);if r.StatusCode!=200{t.Fatal(r)}
 req.Method="POST";req.Path="/clear";req.Body=[]byte("{}");req.Headers["Content-Type"]="application/json"
 r,_=h.Handle(context.Background(),req);if r.StatusCode!=403{t.Fatal("CSRF accepted")}
 req.Headers["X-Duplicate-Review-CSRF"]=h.csrf
 r,_=h.Handle(context.Background(),req);if r.StatusCode!=403{t.Fatal("header alone accepted")}
 delete(req.Headers,"X-Duplicate-Review-CSRF")
 req.Body=[]byte(`{"csrf":"wrong"}`)
 r,_=h.Handle(context.Background(),req);if r.StatusCode!=403{t.Fatal("wrong token accepted")}
 req.Body=[]byte(`{"csrf":"`+h.csrf+`"}`)
 r,_=h.Handle(context.Background(),req);if r.StatusCode!=200{t.Fatal(r)}
 req.Path="/delete"
 r,_=h.Handle(context.Background(),req);if r.StatusCode!=404{t.Fatal("deletion route exists")}
}
func TestClientRefusesRedirectAndRedactsError(t *testing.T){
 target:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){t.Error("redirect followed")}))
 defer target.Close()
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){http.Redirect(w,r,target.URL,302)}))
 defer server.Close()
 cfg:=DefaultConfig();cfg.APIKey="PRIVATE-KEY"
 c,err:=NewClient(server.URL,cfg);if err!=nil{t.Fatal(err)}
 var out any
 err=c.get(context.Background(),"/test",url.Values{},&out)
 if err==nil||strings.Contains(err.Error(),cfg.APIKey){t.Fatal(err)}
}
func TestPaginationRejectsRepeatedCursor(t *testing.T){
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  w.Header().Set("Content-Type","application/json")
  w.Write([]byte(`{"items":[],"page":{"has_more":true,"next_cursor":"repeat"}}`))
 }))
 defer server.Close()
 cfg:=DefaultConfig();cfg.APIKey="test"
 c,_:=NewClient(server.URL,cfg)
 if _,err:=c.Libraries(context.Background());err==nil{t.Fatal("repeated cursor accepted")}
}
func TestCancelledPacingReturnsPromptly(t *testing.T){
 c:=&Client{next:time.Now().Add(time.Hour),interval:time.Second}
 ctx,cancel:=context.WithCancel(context.Background());cancel()
 start:=time.Now()
 if c.pace(ctx)==nil{t.Fatal("cancellation ignored")}
 if time.Since(start)>time.Second{t.Fatal("cancellation delayed")}
}
type fakeSource struct{ block bool }
func(f fakeSource)Items(ctx context.Context,lib,kind string)([]Item,error){
 if kind=="episode"{return []Item{},nil}
 return []Item{{ID:"a",Title:"A",Type:"movie"},{ID:"b",Title:"B",Type:"movie"}},nil
}
func(f fakeSource)Versions(ctx context.Context,id,lib string)([]Version,error){
 if f.block{<-ctx.Done();return nil,ctx.Err()}
 return []Version{{ID:id+"1"},{ID:id+"2"}},nil
}
func(f fakeSource)Membership(ctx context.Context,id,lib string)(map[string]string,error){
 return map[string]string{id+"1":"/"+id+"1",id+"2":"/"+id+"2"},nil
}
func TestScanCompletes(t *testing.T){
 runScan(context.Background(),fakeSource{},"lib",2,func(int,int){},func(g []Group,errs []string,status string){
  if status!="complete"||len(g)!=2||len(errs)!=0{t.Fatalf("%s %v",status,errs)}
 })
}
func TestScanCancelsWorkers(t *testing.T){
 ctx,cancel:=context.WithTimeout(context.Background(),20*time.Millisecond);defer cancel()
 runScan(ctx,fakeSource{block:true},"lib",2,func(int,int){},func(g []Group,errs []string,status string){
  if status!="cancelled"{t.Fatalf("status %s",status)}
 })
}

func TestConnectProfileAndStrictQueryContract(t *testing.T) {
 for _,selector:=range []string{"","Administrador","primary-id"} {
  t.Run(selector,func(t *testing.T){
   server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
    switch r.URL.Path {
    case "/api/v2/account/me":
     if r.Header.Get("X-Profile-Id")!=""{t.Error("profile sent before resolution")}
     w.Write([]byte(`{"role":"admin"}`))
    case "/api/v2/profiles":
     if r.URL.RawQuery!=""{http.Error(w,"unknown query",422);return}
     w.Write([]byte(`{"items":[{"id":"primary-id","name":"Administrador","is_primary":true}]}`))
    case "/api/v2/user/libraries":
     if r.URL.RawQuery!="" || r.Header.Get("X-Profile-Id")!="primary-id"{http.Error(w,"invalid query or profile",422);return}
     w.Write([]byte(`{"items":[]}`))
    case "/api/v2/catalog/items/movie/versions":
     if r.URL.Query().Get("limit")!=""{http.Error(w,"unknown limit",422);return}
     w.Write([]byte(`{"items":[]}`))
    default: http.NotFound(w,r)
    }
   }));defer server.Close()
   cfg:=DefaultConfig();cfg.APIKey="test";cfg.ProfileID=selector
   c,err:=NewClient(server.URL,cfg);if err!=nil{t.Fatal(err)};c.interval=0
   if err=c.Connect(context.Background());err!=nil{t.Fatal(err)}
   if c.profileName!="Administrador"{t.Fatal("profile name not resolved")}
   if _,err=c.Libraries(context.Background());err!=nil{t.Fatal(err)}
   if _,err=c.Versions(context.Background(),"movie","library");err!=nil{t.Fatal(err)}
  })
 }
}

func TestConnectionCheckRevalidatesAccount(t *testing.T){
 calls:=0
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  switch r.URL.Path{
  case "/api/v2/account/me":calls++;role:="admin";if calls>1{role="user"};json.NewEncoder(w).Encode(map[string]string{"role":role})
  case "/api/v2/profiles":w.Write([]byte(`{"items":[{"id":"p","name":"Primary","is_primary":true}]}`))
  case "/api/v2/user/libraries":w.Write([]byte(`{"items":[]}`))
  default:http.NotFound(w,r)
  }
 }));defer server.Close()
 h:=NewHandler();cfg:=DefaultConfig();cfg.APIKey="test";cfg.BaseURL=server.URL;cfg.DeleteEnabled=true;cfg.RequestsPerSecond=80;h.Configure(cfg)
 req:=&pluginv1.HandleHTTPRequest{Method:"GET",Path:"/check",Headers:map[string]string{"X-Silo-User-Role":"admin","X-Silo-User-Id":"1"}}
 res,_:=h.Handle(context.Background(),req);if res.StatusCode!=200{t.Fatal(string(res.Body))}
 var status struct{ReadOnly bool `json:"read_only"`;Enabled bool `json:"delete_enabled"`};json.Unmarshal(res.Body,&status)
 if status.ReadOnly||!status.Enabled{t.Fatal("wrong deletion setting")}
 res,_=h.Handle(context.Background(),req);if res.StatusCode!=502||calls!=2{t.Fatal("cached account accepted",string(res.Body))}
}

func TestEpisodeScanUsesSeriesFileMembership(t *testing.T) {
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  switch r.URL.Path {
  case "/api/v2/catalog":
   if r.URL.Query().Get("type")=="episode" {w.Write([]byte(`{"items":[{"content_id":"episode-1","type":"episode","title":"Pilot","series_title":"Show"}]}`))} else {w.Write([]byte(`{"items":[]}`))}
  case "/api/v2/catalog/items/episode-1/versions":
   w.Write([]byte(`{"items":[{"file_id":"1"},{"file_id":"2"},{"file_id":"3"}]}`))
  case "/api/v2/admin/items/episode-1/files":http.NotFound(w,r)
  case "/api/v2/catalog/items/episode-1":
   if r.URL.Query().Get("library_id")!="lib" {t.Error("missing library scope")}
   w.Write([]byte(`{"type":"episode","series_id":"series-1"}`))
  case "/api/v2/admin/items/series-1/files":
   w.Write([]byte(`{"items":[{"id":"1","library_id":"lib","file_path":"/one.mkv"},{"id":"2","library_id":"lib","file_path":"/two.mkv"},{"id":"3","library_id":"other","file_path":"/other.mkv"},{"id":"4","library_id":"lib","file_path":"/another-episode.mkv"}]}`))
  default:t.Errorf("unexpected request %s",r.URL);http.NotFound(w,r)
  }
 }));defer server.Close()
 cfg:=DefaultConfig();cfg.APIKey="test"
 c,err:=NewClient(server.URL,cfg);if err!=nil{t.Fatal(err)};c.interval=0
 runScan(context.Background(),c,"lib",2,func(int,int){},func(groups []Group,errs []string,status string){
  if status!="complete"||len(errs)!=0||len(groups)!=1 {t.Fatalf("status=%s errors=%v groups=%v",status,errs,groups)}
  if len(groups[0].Versions)!=2 {t.Fatalf("wrong episode/library membership: %+v",groups[0].Versions)}
 })
}

func TestMembershipDoesNotFallbackOnPermissionFailure(t *testing.T) {
 calls:=0
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){calls++;w.WriteHeader(403)}));defer server.Close()
 cfg:=DefaultConfig();cfg.APIKey="test";c,_:=NewClient(server.URL,cfg);c.interval=0
 if _,err:=c.Membership(context.Background(),"episode","lib");err==nil||calls!=1{t.Fatalf("permission error bypassed: %v, calls=%d",err,calls)}
}

func TestElevatedScanLimits(t *testing.T) {
 c:=DefaultConfig();c.APIKey="test";c.RequestsPerSecond=100;c.Concurrency=16
 if err:=c.Validate();err!=nil{t.Fatal(err)}
 c.RequestsPerSecond=101
 if c.Validate()==nil{t.Fatal("rate above maximum accepted")}
 c.RequestsPerSecond=100;c.Concurrency=17
 if c.Validate()==nil{t.Fatal("concurrency above maximum accepted")}
}

func TestLogoServedFromEmbeddedAsset(t *testing.T) {
 h:=NewHandler()
 req:=&pluginv1.HandleHTTPRequest{Method:"GET",Path:"/assets/logo.png",Headers:map[string]string{"X-Silo-User-Role":"admin","X-Silo-User-Id":"1"}}
 res,err:=h.Handle(context.Background(),req)
 if err!=nil||res.StatusCode!=200||res.Headers["Content-Type"]!="image/png"||len(res.Body)<8||string(res.Body[:8])!="\x89PNG\r\n\x1a\n"{t.Fatal("embedded logo unavailable")}
 req.Headers=nil;res,_=h.Handle(context.Background(),req)
 if res.StatusCode!=403{t.Fatal("asset bypassed administrator access")}
}
