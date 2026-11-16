package review

import (
 "context"
 "crypto/rand"
 "encoding/hex"
 "encoding/json"
 "errors"
 "time"
 pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)
type deletePlan struct{Token,User,Scan,Group,File string; Expires time.Time}
func token()string{b:=make([]byte,32);if _,e:=rand.Read(b);e!=nil{panic(e)};return hex.EncodeToString(b)}
// All operations are serialized with scan/configuration changes. Each request
// rechecks membership and both filesystem fingerprints before an unlink.
func(h *Handler) deletion(ctx context.Context,req *pluginv1.HandleHTTPRequest,commit bool)(*pluginv1.HandleHTTPResponse,error){
 var in struct{Scan string `json:"scan_id"`;Group string `json:"group_id"`;File string `json:"file_id"`;Token string `json:"token"`}
 if json.Unmarshal(req.Body,&in)!=nil{return failure(400,errors.New("Invalid request")),nil}
 client,err:=h.connect(ctx);if err!=nil{return failure(502,err),nil}
 h.mu.Lock();defer h.mu.Unlock()
 if !h.cfg.DeleteEnabled{return failure(403,errors.New("Deletion is disabled in settings")),nil}
 if h.scan.Status!="complete"||h.scan.ID!=in.Scan{return failure(409,errors.New("A current, complete scan is required")),nil}
 if commit {
  p:=h.pending
  h.pending=deletePlan{}
  if p.Token==""||p.Token!=in.Token||p.User!=header(req,"X-Silo-User-Id")||p.Scan!=in.Scan||p.Group!=in.Group||p.File!=in.File||time.Now().After(p.Expires){return failure(409,errors.New("Confirmation expired; review the file again")),nil}
 }
 var group *Group
 for i:=range h.scan.Groups{if h.scan.Groups[i].ID==in.Group{group=&h.scan.Groups[i];break}}
 if group==nil{return failure(404,errors.New("Group not found")),nil}
 var target *Version
 for i:=range group.Versions{if group.Versions[i].ID==in.File{target=&group.Versions[i];break}}
 if target==nil||target.Deleted||!target.Ready{return failure(409,errors.New("File unavailable for deletion")),nil}
 libraries,err:=client.Libraries(ctx);if err!=nil{return failure(502,err),nil}
 allowed:=false;for _,l:=range libraries{if l.ID==h.scan.Library{allowed=true}}
 if !allowed{return failure(403,errors.New("Library is no longer available")),nil}
 verify:=func(v *Version)(*heldFile,error){
  members,e:=client.Membership(ctx,v.ItemID,h.scan.Library);if e!=nil{return nil,e}
  if members[v.ID]!=v.Path{return nil,errors.New("The Silo file reference has changed")}
  versions,e:=client.Versions(ctx,v.ItemID,h.scan.Library);if e!=nil{return nil,e}
  found:=false
  for _,live:=range versions{if live.ID==v.ID&&live.Size==v.Size&&live.Parts<=1&&live.MultiEnd==0&&(live.Kind==""||live.Kind=="single"){found=true}}
  if !found{return nil,errors.New("The Silo version has changed")}
  f,e:=holdFile(v.Path,h.cfg.MediaRoots);if e!=nil{return nil,e}
  if f.stamp!=v.Snapshot||f.path!=v.ResolvedPath{f.close();return nil,errors.New("The file has changed since the scan")}
  return f,nil
 }
 tf,err:=verify(target);if err!=nil{return failure(409,err),nil};defer tf.close()
 if err=tf.writable();err!=nil{return failure(409,err),nil}
 var keeper *heldFile;var keepPath string
 for i:=range group.Versions{
  v:=&group.Versions[i]
  if v.ID==target.ID||v.Deleted||!v.Ready{continue}
  f,e:=verify(v);if e==nil{if f.path==tf.path{f.close();continue};keeper=f;keepPath=f.path;break}
 }
 if keeper==nil{return failure(409,errors.New("Another verified file path must remain; the last copy cannot be deleted")),nil};defer keeper.close()
 if !commit{
  h.pending=deletePlan{token(),header(req,"X-Silo-User-Id"),in.Scan,in.Group,in.File,time.Now().Add(60*time.Second)}
  return jsonResponse(200,map[string]any{"token":h.pending.Token,"path":tf.path,"library_path":target.Path,"keep_path":keepPath,"size":target.Size}),nil
 }
 spaceReleased:=tf.releasedSpaceEstimate()
 if err=safeRemove(tf,keeper);err!=nil{return failure(409,err),nil}
 removedDirectories:=[]string{}
 if group.Type=="episode"{removedDirectories=cleanupEmptyParents(tf)}
 // Mark every occurrence of this physical path without invalidating other groups.
 for gi:=range h.scan.Groups{for vi:=range h.scan.Groups[gi].Versions{
  v:=&h.scan.Groups[gi].Versions[vi];if v.Path==target.Path||v.ResolvedPath==tf.path{v.Deleted=true;v.Ready=false;v.SpaceReleased=spaceReleased;v.DeletedAt=time.Now().UTC().Format(time.RFC3339);v.KeptPath=keepPath;v.RemovedDirectories=removedDirectories}
 }}
 return jsonResponse(200,map[string]any{"deleted":true,"space_released":spaceReleased,"path":tf.path,"removed_directories":removedDirectories,"message":"File deleted. Silo will reflect the change after its next library scan."}),nil
}
