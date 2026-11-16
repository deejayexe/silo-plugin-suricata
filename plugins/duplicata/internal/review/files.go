package review

import (
 "errors"
 "path/filepath"
 "strings"
 "golang.org/x/sys/unix"
)

type fingerprint struct {Dev uint64; Ino uint64; Size int64; Sec int64; Nsec int64}
type heldFile struct {parent int; name string; stamp fingerprint; source, path, root string}
func (f *heldFile) close(){unix.Close(f.parent)}
func stamp(s *unix.Stat_t) fingerprint{return fingerprint{uint64(s.Dev),s.Ino,s.Size,s.Mtim.Sec,s.Mtim.Nsec}}
func (f *heldFile) check()error{
 resolved,err:=filepath.EvalSymlinks(f.source)
 if err!=nil||resolved!=f.path{return errors.New("The library path has changed")}
 var s unix.Stat_t
 if err:=unix.Fstatat(f.parent,f.name,&s,unix.AT_SYMLINK_NOFOLLOW);err!=nil{return err}
 if s.Mode&unix.S_IFMT!=unix.S_IFREG || stamp(&s)!=f.stamp{return errors.New("The file has changed or is not a regular file")}
 // Check that the pathname still leads to the held parent directory.
 fresh,err:=openDirectory(filepath.Dir(f.path));if err!=nil{return err};defer unix.Close(fresh)
 var oldDir,newDir unix.Stat_t
 if err=unix.Fstat(f.parent,&oldDir);err!=nil{return err}
 if err=unix.Fstat(fresh,&newDir);err!=nil{return err}
 if oldDir.Dev!=newDir.Dev||oldDir.Ino!=newDir.Ino{return errors.New("The parent directory has changed")}
 return nil
}
func openDirectory(path string)(int,error){
 fd,err:=unix.Open("/",unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC,0);if err!=nil{return -1,err}
 for _,part:=range strings.Split(strings.TrimPrefix(path,"/"),"/"){
  if part==""{continue}
  next,e:=unix.Openat(fd,part,unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC,0)
  unix.Close(fd);if e!=nil{return -1,errors.New("Directory inaccessible or changed to a symbolic link")};fd=next
 }
 return fd,nil
}
func within(root,path string)bool{
 rel,err:=filepath.Rel(root,path)
 return err==nil&&rel!="."&&rel!=".."&&!strings.HasPrefix(rel,"../")
}
// Resolve library aliases, then traverse the authorized real path without following links.
// The browser never supplies a filesystem path.
func holdFile(path,roots string)(*heldFile,error){
 if !filepath.IsAbs(path)||filepath.Clean(path)!=path{return nil,errors.New("Invalid path")}
 resolved,err:=filepath.EvalSymlinks(path);if err!=nil{return nil,err}
 boundary:=""
 for _,root:=range strings.Split(roots,"\n"){
  root=strings.TrimSpace(root)
  if root==""||root=="/"||!filepath.IsAbs(root)||filepath.Clean(root)!=root{continue}
  if within(root,resolved)&&len(root)>len(boundary){boundary=root}
 }
 if boundary==""{return nil,errors.New("File outside the authorized folders")}
 fd,err:=openDirectory(filepath.Dir(resolved));if err!=nil{return nil,err}
 var s unix.Stat_t
 name:=filepath.Base(resolved)
 if err=unix.Fstatat(fd,name,&s,unix.AT_SYMLINK_NOFOLLOW);err!=nil{unix.Close(fd);return nil,err}
 if s.Mode&unix.S_IFMT!=unix.S_IFREG{unix.Close(fd);return nil,errors.New("Directories and special files are not supported")}
 return &heldFile{parent:fd,name:name,stamp:stamp(&s),source:path,path:resolved,root:boundary},nil
}
func (f *heldFile) writable()error{
 var fs unix.Statfs_t
 if err:=unix.Fstatfs(f.parent,&fs);err!=nil{return err}
 if fs.Flags&unix.ST_RDONLY!=0{return errors.New("Media mount is read-only")}
 if err:=unix.Faccessat(f.parent,".",unix.W_OK|unix.X_OK,unix.AT_EACCESS);err!=nil{return errors.New("No write/search permission on the parent folder")}
 return nil
}
func safeRemove(target,survivor *heldFile)error{
 if target.path==survivor.path{return errors.New("Both entries refer to the same path")}
 if err:=survivor.check();err!=nil{return errors.New("The retained copy has changed or is missing")}
 if err:=target.check();err!=nil{return err}
 if err:=target.writable();err!=nil{return err}
 return unix.Unlinkat(target.parent,target.name,0)
}
// Remove empty episode directories only. rmdir never removes remaining content.
func cleanupEmptyParents(f *heldFile)[]string{
 removed:=[]string{}
 child,err:=unix.Dup(f.parent);if err!=nil{return removed};defer func(){unix.Close(child)}()
 for path:=filepath.Dir(f.path);within(f.root,path);path=filepath.Dir(path){
  parent,err:=openDirectory(filepath.Dir(path));if err!=nil{break}
  var held,live unix.Stat_t
  err=unix.Fstat(child,&held)
  if err==nil{err=unix.Fstatat(parent,filepath.Base(path),&live,unix.AT_SYMLINK_NOFOLLOW)}
  if err!=nil||live.Mode&unix.S_IFMT!=unix.S_IFDIR||held.Dev!=live.Dev||held.Ino!=live.Ino{unix.Close(parent);break}
  if err=unix.Unlinkat(parent,filepath.Base(path),unix.AT_REMOVEDIR);err!=nil{unix.Close(parent);break}
  removed=append(removed,path);unix.Close(child);child=parent
 }
 return removed
}
func captureFiles(groups []Group,roots string){
 for gi:=range groups{for vi:=range groups[gi].Versions{
  v:=&groups[gi].Versions[vi]
  v.Ready=false;v.Blocked="";v.WriteBlocked=""
  if v.Deleted{continue}
  if v.Parts>1||v.MultiEnd>0||(v.Kind!=""&&v.Kind!="single"){v.Blocked="Multipart or multi-episode content";continue}
  f,err:=holdFile(v.Path,roots)
  if err!=nil{v.Blocked=err.Error();continue}
  if f.stamp.Size!=v.Size{v.Blocked="File size does not match Silo";f.close();continue}
  v.ResolvedPath=f.path
  if err=f.writable();err!=nil{v.WriteBlocked=err.Error()}
  v.Snapshot=f.stamp;v.Ready=true;f.close()
 }}
}
