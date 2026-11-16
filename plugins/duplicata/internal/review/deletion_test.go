package review
import (
 "context"
 "encoding/json"
 "net/http"
 "net/http/httptest"
 "os"
 "path/filepath"
 "testing"
 pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)
func TestFileBoundaryAndChanges(t *testing.T){
 root:=t.TempDir();a:=filepath.Join(root,"a.mkv");b:=filepath.Join(root,"b.mkv")
 os.WriteFile(a,[]byte("a"),0600);os.WriteFile(b,[]byte("b"),0600)
 if _,e:=holdFile(a,root+"2");e==nil{t.Fatal("prefix bypass")}
 if _,e:=holdFile(a,"/");e==nil{t.Fatal("root allowed")}
 link:=filepath.Join(root,"link");os.Symlink(a,link)
 if f,e:=holdFile(link,root);e!=nil{t.Fatal(e)}else{f.close()}
 dirlink:=filepath.Join(root,"dir");os.Symlink(root,dirlink)
 if f,e:=holdFile(filepath.Join(dirlink,"a.mkv"),root);e!=nil{t.Fatal(e)}else{f.close()}
 hard:=filepath.Join(root,"hard");os.Link(a,hard)
 if f,e:=holdFile(a,root);e!=nil{t.Fatal(e)}else{f.close()};os.Remove(hard)
 target,e:=holdFile(a,root);if e!=nil{t.Fatal(e)};defer target.close()
 keeper,e:=holdFile(b,root);if e!=nil{t.Fatal(e)};defer keeper.close()
 os.WriteFile(a,[]byte("changed"),0600)
 if e=safeRemove(target,keeper);e==nil{t.Fatal("changed file removed")}
 if _,e=os.Stat(a);e!=nil{t.Fatal(e)}
}
func TestMissingKeeperBlocksRemoval(t *testing.T){
 root:=t.TempDir();a:=filepath.Join(root,"a");b:=filepath.Join(root,"b")
 os.WriteFile(a,[]byte("a"),0600);os.WriteFile(b,[]byte("b"),0600)
 target,_:=holdFile(a,root);defer target.close();keeper,_:=holdFile(b,root);defer keeper.close()
 os.Remove(b);if safeRemove(target,keeper)==nil{t.Fatal("removed last copy")}
 if _,e:=os.Stat(a);e!=nil{t.Fatal(e)}
}
func TestDeleteFlowPreservesLastCopyAndRejectsReplay(t *testing.T){
 root:=t.TempDir();a:=filepath.Join(root,"a.mkv");b:=filepath.Join(root,"b.mkv")
 os.WriteFile(a,[]byte("a"),0600);os.WriteFile(b,[]byte("b"),0600)
 versions:=[]Version{{ID:"a",ItemID:"movie",Path:a,Size:1},{ID:"b",ItemID:"movie",Path:b,Size:1}}
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  var result any
  switch r.URL.Path{
  case "/api/v2/user/libraries":result=map[string]any{"items":[]Library{{ID:"lib"}}}
  case "/api/v2/admin/items/movie/files":result=map[string]any{"items":[]member{{ID:"a",Library:"lib",Path:a},{ID:"b",Library:"lib",Path:b}}}
  case "/api/v2/catalog/items/movie/versions":result=map[string]any{"items":versions}
  default:http.NotFound(w,r);return
  };json.NewEncoder(w).Encode(result)
 }));defer server.Close()
 cfg:=DefaultConfig();cfg.APIKey="test";cfg.DeleteEnabled=true;cfg.MediaRoots=root
 client,_:=NewClient(server.URL,cfg);client.interval=0
 h:=NewHandler();h.cfg=cfg;h.client=client
 h.scan=Scan{ID:"scan",Library:"lib",Status:"complete",Groups:[]Group{{ID:"group",Versions:versions}}}
 captureFiles(h.scan.Groups,root)
 call:=func(path,file,token string)*pluginv1.HandleHTTPResponse{
  data,_:=json.Marshal(map[string]string{"csrf":h.csrf,"scan_id":"scan","group_id":"group","file_id":file,"token":token})
  res,_:=h.Handle(context.Background(),&pluginv1.HandleHTTPRequest{Method:"POST",Path:path,Body:data,Headers:map[string]string{"Content-Type":"application/json","X-Silo-User-Role":"admin","X-Silo-User-Id":"1"}});return res
 }
 h.cfg.DeleteEnabled=false
 if call("/delete/preview","a","").StatusCode!=403{t.Fatal("disabled accepted")};h.cfg.DeleteEnabled=true
 h.scan.Status="partial";if call("/delete/preview","a","").StatusCode!=409{t.Fatal("partial accepted")};h.scan.Status="complete"
 preview:=call("/delete/preview","a","");if preview.StatusCode!=200{t.Fatal(string(preview.Body))}
 if _,e:=os.Stat(a);e!=nil{t.Fatal("preview removed file")}
 var plan struct{Token string `json:"token"`};json.Unmarshal(preview.Body,&plan)
 result:=call("/delete/confirm","a",plan.Token);if result.StatusCode!=200{t.Fatal(string(result.Body))}
 var deletedResponse struct{Space *SpaceEstimate `json:"space_released"`};json.Unmarshal(result.Body,&deletedResponse)
 if deletedResponse.Space==nil||deletedResponse.Space.Bytes<=0{t.Fatal("deletion response lacks space estimate")}
 if _,e:=os.Stat(a);!os.IsNotExist(e){t.Fatal("target still exists")}
 if _,e:=os.Stat(b);e!=nil{t.Fatal("keeper gone")}
 if !h.scan.Groups[0].Versions[0].Deleted{t.Fatal("not marked")}
 if call("/delete/confirm","a",plan.Token).StatusCode!=409{t.Fatal("replay accepted")}
 if call("/delete/preview","b","").StatusCode!=409{t.Fatal("last copy allowed")}
 if h.scan.Groups[0].Versions[0].DeletedAt==""||h.scan.Groups[0].Versions[0].KeptPath!=b{t.Fatal("deletion audit missing")}
 h.archiveScanLocked();h.scan=Scan{Status:"idle",Groups:[]Group{}}
 if h.history[0].Scan.Groups[0].Versions[0].SpaceReleased==nil{t.Fatal("space estimate missing from archived deletion")}
 if !h.history[0].Scan.Groups[0].Versions[0].Deleted{t.Fatal("deletion not preserved in history")}
 if call("/delete/preview","a","").StatusCode!=409{t.Fatal("archived scan allowed deletion")}
 if _,e:=os.Stat(b);e!=nil{t.Fatal("history operation deleted keeper")}

}

func TestHardlinkDeletionPreservesOtherLinks(t *testing.T){
 root:=t.TempDir();a:=filepath.Join(root,"a");b:=filepath.Join(root,"b");external:=filepath.Join(t.TempDir(),"other")
 if err:=os.WriteFile(a,[]byte("media"),0600);err!=nil{t.Fatal(err)}
 for _,path:=range []string{b,external}{if err:=os.Link(a,path);err!=nil{t.Fatal(err)}}
 target,err:=holdFile(a,root);if err!=nil{t.Fatal(err)};defer target.close()
 keeper,err:=holdFile(b,root);if err!=nil{t.Fatal(err)};defer keeper.close()
 if err=safeRemove(target,keeper);err!=nil{t.Fatal(err)}
 if _,err=os.Stat(a);!os.IsNotExist(err){t.Fatal("selected path remains")}
 for _,path:=range []string{b,external}{data,err:=os.ReadFile(path);if err!=nil||string(data)!="media"{t.Fatal("other link lost",path,err)}}
}
func TestSymlinkBoundaryAndSameResolvedPath(t *testing.T){
 root:=t.TempDir();outside:=t.TempDir();a:=filepath.Join(root,"a");link:=filepath.Join(outside,"library")
 os.WriteFile(a,[]byte("media"),0600);os.Symlink(root,link)
 target,err:=holdFile(filepath.Join(link,"a"),root);if err!=nil{t.Fatal(err)};defer target.close()
 keeper,err:=holdFile(a,root);if err!=nil{t.Fatal(err)};defer keeper.close()
 if safeRemove(target,keeper)==nil{t.Fatal("same resolved path used as keeper")}
 os.WriteFile(filepath.Join(outside,"escape"),[]byte("x"),0600);os.Symlink(filepath.Join(outside,"escape"),filepath.Join(root,"escape"))
 if f,err:=holdFile(filepath.Join(root,"escape"),root);err==nil{f.close();t.Fatal("escaped root")}
 os.Remove(link);os.Symlink(outside,link)
 if target.check()==nil{t.Fatal("changed alias accepted")}
}
func TestEmptyEpisodeCleanup(t *testing.T){
 for _,companion:=range []bool{false,true}{t.Run(map[bool]string{false:"empty",true:"companion"}[companion],func(t *testing.T){
  root:=t.TempDir();season:=filepath.Join(root,"Series","Season 1");os.MkdirAll(season,0700)
  a:=filepath.Join(season,"episode.mkv");b:=filepath.Join(root,"keeper.mkv");os.WriteFile(a,[]byte("a"),0600);os.WriteFile(b,[]byte("b"),0600)
  if companion{os.WriteFile(filepath.Join(season,"episode.srt"),[]byte("subtitle"),0600)}
  target,err:=holdFile(a,root);if err!=nil{t.Fatal(err)};defer target.close();keeper,err:=holdFile(b,root);if err!=nil{t.Fatal(err)};defer keeper.close()
  if err=safeRemove(target,keeper);err!=nil{t.Fatal(err)}
  removed:=cleanupEmptyParents(target)
  if companion{if len(removed)!=0{t.Fatal("removed folder with content")};if _,err=os.Stat(filepath.Join(season,"episode.srt"));err!=nil{t.Fatal(err)}}else{if len(removed)!=2{t.Fatal(removed)};if _,err=os.Stat(filepath.Join(root,"Series"));!os.IsNotExist(err){t.Fatal("empty series remains")}}
  if _,err=os.Stat(root);err!=nil{t.Fatal("authorized root removed")}
 })}
}
func TestCleanupStopsAtNestedAuthorizedRoot(t *testing.T){
 root:=t.TempDir();nested:=filepath.Join(root,"Series");season:=filepath.Join(nested,"Season");os.MkdirAll(season,0700)
 a:=filepath.Join(season,"episode");os.WriteFile(a,[]byte("a"),0600)
 f,err:=holdFile(a,root+"\n"+nested);if err!=nil{t.Fatal(err)};defer f.close();os.Remove(a)
 if got:=cleanupEmptyParents(f);len(got)!=1{t.Fatal(got)}
 if _,err=os.Stat(nested);err!=nil{t.Fatal("nested authorized root removed")}
}

func TestRestoredAnalysisDeletionRechecksLiveState(t *testing.T) {
 for _,scenario:=range []string{"unchanged","changed-target","missing-keeper","changed-reference","partial"} {
  t.Run(scenario,func(t *testing.T){
   root:=t.TempDir();a:=filepath.Join(root,"a.mkv");b:=filepath.Join(root,"b.mkv")
   os.WriteFile(a,[]byte("a"),0600);os.WriteFile(b,[]byte("b"),0600)
   versions:=[]Version{{ID:"a",ItemID:"movie",Path:a,Size:1},{ID:"b",ItemID:"movie",Path:b,Size:1}}
   server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
    var result any
    switch r.URL.Path {
    case "/api/v2/user/libraries":result=map[string]any{"items":[]Library{{ID:"lib"}}}
    case "/api/v2/admin/items/movie/files":
     path:=a;if scenario=="changed-reference"{path=filepath.Join(root,"replacement")}
     result=map[string]any{"items":[]member{{ID:"a",Library:"lib",Path:path},{ID:"b",Library:"lib",Path:b}}}
    case "/api/v2/catalog/items/movie/versions":result=map[string]any{"items":versions}
    default:t.Errorf("unexpected request %s (restore must not rescan)",r.URL.Path);http.NotFound(w,r);return
    };json.NewEncoder(w).Encode(result)
   }));defer server.Close()
   cfg:=DefaultConfig();cfg.APIKey="test";cfg.DeleteEnabled=true;cfg.MediaRoots=root
   client,_:=NewClient(server.URL,cfg);client.interval=0
   h:=NewHandler();h.cfg=cfg;h.client=client
   h.scan=Scan{ID:"saved",Library:"lib",Status:"complete",Groups:[]Group{{ID:"group",Versions:versions}}}
   captureFiles(h.scan.Groups,root)
   if scenario=="partial"{h.scan.Status="partial"}
   h.archiveScanLocked();h.scan=Scan{Status:"idle",Groups:[]Group{}}
   if scenario=="changed-target"{os.WriteFile(a,[]byte("changed media"),0600)}
   if scenario=="missing-keeper"{os.Remove(b)}
   if res:=historyRequest(h,"POST","/history/restore",nil,map[string]string{"scan_id":"saved","csrf":h.csrf});res.StatusCode!=200{t.Fatalf("restore: %s",res.Body)}
   request:=map[string]string{"scan_id":"saved","group_id":"group","file_id":"a","csrf":h.csrf}
   preview:=historyRequest(h,"POST","/delete/preview",nil,request)
   if scenario!="unchanged" {
    if preview.StatusCode!=409{t.Fatalf("unsafe restored result accepted: %s",preview.Body)}
    if _,err:=os.Stat(a);err!=nil{t.Fatal("blocked operation removed target")};return
   }
   if preview.StatusCode!=200{t.Fatalf("preview: %s",preview.Body)}
   var plan struct{Token string `json:"token"`};json.Unmarshal(preview.Body,&plan);request["token"]=plan.Token
   result:=historyRequest(h,"POST","/delete/confirm",nil,request)
   if result.StatusCode!=200{t.Fatalf("deletion from restored analysis: %s",result.Body)}
   if _,err:=os.Stat(a);!os.IsNotExist(err){t.Fatal("selected file not removed")}
   if _,err:=os.Stat(b);err!=nil{t.Fatal("keeper removed")}
  })
 }
}
