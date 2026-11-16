package review

import (
 "context"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "fmt"
 "sort"
 "sync"
 "time"
)

type Item struct {
 ID string `json:"content_id"`
 Title string `json:"title"`
 Type string `json:"type"`
 Year int `json:"year"`
 TMDB json.RawMessage `json:"tmdb_id"`
 IMDB string `json:"imdb_id"`
 Series string `json:"series_title"`
}
type Version struct {
 ItemID string `json:"-"`
 Snapshot fingerprint `json:"-"`
 ResolvedPath string `json:"resolved_path,omitempty"`
 WriteBlocked string `json:"write_blocked,omitempty"`
 Ready bool `json:"can_delete"`
 Deleted bool `json:"deleted"`
 SpaceReleased *SpaceEstimate `json:"space_released"`
 DeletedAt string `json:"deleted_at,omitempty"`
 KeptPath string `json:"kept_path,omitempty"`
 RemovedDirectories []string `json:"removed_directories,omitempty"`
 Blocked string `json:"blocked,omitempty"`
 ID string `json:"file_id"`
 Path string `json:"file_path"`
 Size int64 `json:"file_size"`
 Codec string `json:"codec_video"`
 Audio string `json:"codec_audio"`
 Resolution string `json:"resolution"`
 Duration float64 `json:"duration"`
 Edition string `json:"edition_raw"`
 Kind string `json:"presentation_kind"`
 Parts int `json:"presentation_part_total"`
 MultiEnd int `json:"multi_episode_end"`
 AudioTracks []struct{Language string `json:"language"`} `json:"audio_tracks"`
}
type Group struct {
 ItemID string `json:"-"`
 ID string `json:"id"`
 Title string `json:"title"`
 Year int `json:"year"`
 Type string `json:"type"`
 Series string `json:"series_title"`
 Versions []Version `json:"versions"`
 Warnings []string `json:"warnings"`
}
func identity(library string,item Item)string {
 id:=item.ID
 if item.Type=="movie" {
  if len(item.TMDB)>0 && string(item.TMDB)!="null" && string(item.TMDB)!="0" && string(item.TMDB)!=`""` {
   var numeric json.Number
   var text string
   if json.Unmarshal(item.TMDB,&numeric)==nil {id="tmdb:"+numeric.String()} else if json.Unmarshal(item.TMDB,&text)==nil && text!="" {id="tmdb:"+text}
  } else if item.IMDB!="" {id="imdb:"+item.IMDB}
 }
 sum:=sha256.Sum256([]byte(library+"\x00"+item.Type+"\x00"+id))
 return hex.EncodeToString(sum[:12])
}
func addGroup(groups map[string]*Group,library string,item Item,versions []Version,members map[string]string) {
 key:=identity(library,item)
 g:=groups[key]
 if g==nil {g=&Group{ItemID:item.ID,ID:key,Title:item.Title,Year:item.Year,Type:item.Type,Series:item.Series,Versions:[]Version{},Warnings:[]string{}};groups[key]=g}
 paths:=map[string]bool{};for _,v:=range g.Versions{paths[v.Path]=true}
 for _,v:=range versions {
  path,ok:=members[v.ID]
  if !ok || path=="" || paths[path]{continue}
  v.ItemID=item.ID;v.Path=path;g.Versions=append(g.Versions,v);paths[path]=true
 }
}
func finalize(groups map[string]*Group)[]Group {
 out:=[]Group{}
 for _,g:=range groups {
  if len(g.Versions)<2{continue}
  warnings:=map[string]bool{}
  min,max:=0.0,0.0
  editions:=map[string]bool{}
  for _,v:=range g.Versions {
   editions[v.Edition]=true
   if v.Duration>0 {if min==0||v.Duration<min{min=v.Duration};if v.Duration>max{max=v.Duration}}
   if v.Parts>1 || v.MultiEnd>0 || (v.Kind!=""&&v.Kind!="single") {warnings["Multipart or multi-episode content requires special review."]=true}
  }
  if len(editions)>1 {warnings["Different editions: you may want to retain these versions."]=true}
  if max-min>60 && min>0 {warnings["Duration differs by more than one minute."]=true}
  for w:=range warnings{g.Warnings=append(g.Warnings,w)}
  sort.Strings(g.Warnings)
  out=append(out,*g)
 }
 sort.Slice(out,func(i,j int)bool{return out[i].Title<out[j].Title})
 return out
}
type Source interface {
 Items(context.Context,string,string)([]Item,error)
 Versions(context.Context,string,string)([]Version,error)
 Membership(context.Context,string,string)(map[string]string,error)
}
type Scan struct {
 ID string `json:"id"`
 Library string `json:"library_id"`
 LibraryName string `json:"library_name"`
 StartedAt time.Time `json:"started_at"`
 FinishedAt time.Time `json:"finished_at"`
 Status string `json:"status"`
 Processed int `json:"processed"`
 Total int `json:"total"`
 Groups []Group `json:"groups"`
 Errors []string `json:"errors"`
}

// runScan never performs any filesystem mutation. Callback receives immutable snapshots.
func runScan(ctx context.Context,source Source,library string,workers int,update func(int,int),finish func([]Group,[]string,string)) {
 var items []Item
 for _,kind:=range []string{"movie","episode"} {
  rows,err:=source.Items(ctx,library,kind)
  if err!=nil {status:="failed";if ctx.Err()!=nil{status="cancelled"};finish(nil,[]string{err.Error()},status);return}
  items=append(items,rows...)
 }
 seen:=map[string]bool{};unique:=[]Item{};counts:=map[string]int{}
 for _,item:=range items {
  if item.ID=="" {finish(nil,[]string{"Silo returned an item without an identifier"},"failed");return}
  if seen[item.ID]{continue};seen[item.ID]=true;unique=append(unique,item);counts[identity(library,item)]++
 }
 items=unique;update(0,len(items))
 ctx,cancel:=context.WithCancel(ctx);defer cancel()
 jobs:=make(chan Item)
 groups:=map[string]*Group{}
 var mu sync.Mutex
 var wg sync.WaitGroup
 processed:=0;errs:=[]string{};failed:=false
 for n:=0;n<workers;n++ {
  wg.Add(1)
  go func(){
   defer wg.Done()
   for item:=range jobs {
    if ctx.Err()!=nil{return}
    versions,err:=source.Versions(ctx,item.ID,library)
    var members map[string]string
    if err==nil && (len(versions)>1||counts[identity(library,item)]>1){members,err=source.Membership(ctx,item.ID,library)}
    mu.Lock()
    if err!=nil {
     if ctx.Err()==nil {errs=append(errs,fmt.Sprintf("%s: %s",item.Title,err));failed=true;cancel()}
     mu.Unlock();return
    }
    if members!=nil{addGroup(groups,library,item,versions,members)}
    processed++;update(processed,len(items));mu.Unlock()
   }
  }()
 }
loop:
 for _,item:=range items {select{case jobs<-item:case <-ctx.Done():break loop}}
 close(jobs);wg.Wait()
 status:="complete"
 if failed{status="partial"}else if ctx.Err()!=nil{status="cancelled"}
 finish(finalize(groups),errs,status)
}
