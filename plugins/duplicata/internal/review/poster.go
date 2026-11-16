package review
import (
 "context"
 "errors"
 "io"
 "net/http"
 "net/url"
 "strings"
 pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)
func(h *Handler) poster(ctx context.Context,req *pluginv1.HandleHTTPRequest)(*pluginv1.HandleHTTPResponse,error){
 id:="";if req.Query!=nil{if v:=req.Query.Fields["group"];v!=nil{id=v.GetStringValue()}}
 h.mu.Lock();selected:=h.selectedScanLocked(queryString(req,"scan_id"))
 if selected==nil{h.mu.Unlock();return failure(404,errors.New("No image")),nil}
 item,library:="",selected.Library
 for _,g:=range selected.Groups{if g.ID==id{item=g.ItemID;break}};h.mu.Unlock()
 if item==""{return failure(404,errors.New("No image")),nil}
 c,err:=h.connect(ctx);if err!=nil{return failure(502,err),nil}
 // Fetch fresh signed artwork only for visible cards, avoiding expired scan URLs.
 var detail struct{Poster string `json:"poster_url"`}
 if err=c.get(ctx,"/catalog/items/"+url.PathEscape(item),url.Values{"library_id":{library}},&detail);err!=nil{return failure(502,err),nil}
 base,_:=url.Parse(c.base+"/");imageURL,err:=base.Parse(detail.Poster)
 if err!=nil||detail.Poster==""||imageURL.User!=nil||imageURL.Host!=base.Host||imageURL.Scheme!=base.Scheme{
  return failure(404,errors.New("Image unavailable on this server")),nil
 }
 if err=c.pace(ctx);err!=nil{return failure(502,err),nil}
 request,err:=http.NewRequestWithContext(ctx,"GET",imageURL.String(),nil);if err!=nil{return failure(502,errors.New("Invalid image")),nil}
 // Signed image URLs do not need the administrative API key.
 res,err:=c.http.Do(request);if err!=nil{return failure(502,errors.New("Could not load the poster")),nil};defer res.Body.Close()
 typ:=strings.Split(res.Header.Get("Content-Type"),";")[0]
 if res.StatusCode!=200||(typ!="image/jpeg"&&typ!="image/png"&&typ!="image/webp"&&typ!="image/avif"){return failure(404,errors.New("No poster")),nil}
 data,err:=io.ReadAll(io.LimitReader(res.Body,2*1024*1024+1))
 if err!=nil||len(data)>2*1024*1024{return failure(413,errors.New("Image too large")),nil}
 return response(200,typ,data),nil
}
