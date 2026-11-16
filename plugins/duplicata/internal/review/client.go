package review

import (
 "context"
 "encoding/json"
 "errors"
 "fmt"
 "io"
 "net/http"
 "net/url"
 "strconv"
 "strings"
 "sync"
 "time"
)

type siloHTTPError struct { Status int; Path string }
func(e *siloHTTPError) Error() string { return fmt.Sprintf("Silo returned HTTP %d for %s",e.Status,e.Path) }

type Client struct {
 base,key,profile string
 profileName,profileSelector string
 http *http.Client
 mu sync.Mutex
 next time.Time
 interval time.Duration
}
func NewClient(base string,cfg Config) (*Client,error) {
 if err:=cfg.Validate();err!=nil{return nil,err}
 if err:=validateBase(base);err!=nil{return nil,err}
 return &Client{base:strings.TrimRight(base,"/"),key:cfg.APIKey,profileSelector:strings.TrimSpace(cfg.ProfileID),
 interval:time.Duration(float64(time.Second)/cfg.RequestsPerSecond),
 http:&http.Client{Timeout:30*time.Second,CheckRedirect:func(*http.Request,[]*http.Request)error{return http.ErrUseLastResponse}}},nil
}
func (c *Client) pace(ctx context.Context) error {
 for {
  c.mu.Lock()
  delay:=time.Until(c.next)
  if delay<=0 {c.next=time.Now().Add(c.interval);c.mu.Unlock();return nil}
  c.mu.Unlock()
  timer:=time.NewTimer(delay)
  select {case <-ctx.Done():timer.Stop();return ctx.Err();case <-timer.C:}
 }
}
func (c *Client) coolDown(delay time.Duration) {
 c.mu.Lock();defer c.mu.Unlock()
 until:=time.Now().Add(delay);if until.After(c.next){c.next=until}
}
func retryAfter(value string,attempt int) time.Duration {
 if seconds,err:=strconv.Atoi(value);err==nil && seconds>=0{return time.Duration(seconds)*time.Second}
 if when,err:=http.ParseTime(value);err==nil {if d:=time.Until(when);d>0{return d}}
 return time.Duration(1<<attempt)*time.Second
}
func (c *Client) get(ctx context.Context,path string,q url.Values,out any) error {
 for attempt:=0;attempt<4;attempt++ {
  if err:=c.pace(ctx);err!=nil{return err}
  req,err:=http.NewRequestWithContext(ctx,"GET",c.base+"/api/v2"+path+"?"+q.Encode(),nil)
  if err!=nil{return errors.New("Could not build the Silo request")}
  req.Header.Set("Authorization","Bearer "+c.key)
  req.Header.Set("Accept","application/json")
  if c.profile!="" {req.Header.Set("X-Profile-Id",c.profile)}
  resp,err:=c.http.Do(req)
  if err!=nil {if ctx.Err()!=nil{return ctx.Err()};return errors.New("Could not contact Silo; check the URL and TLS")}
  body,readErr:=io.ReadAll(io.LimitReader(resp.Body,8*1024*1024+1));resp.Body.Close()
  if readErr!=nil{return errors.New("Incomplete Silo response")}
  if len(body)>8*1024*1024{return errors.New("Response exceeds the 8 MiB limit")}
  if resp.StatusCode==429 {
   d:=retryAfter(resp.Header.Get("Retry-After"),attempt)
   if d<time.Second{d=time.Second}
   if d>5*time.Minute{return errors.New("Silo requested a long wait; try again later")}
   c.coolDown(d)
   if attempt==3{return errors.New("Silo is still rate limiting requests; lower the request rate")}
   continue
  }
  switch resp.StatusCode {
  case 401:return errors.New("Silo rejected the API key")
  case 403:return errors.New("The API key lacks permission for this request")
  case 422:return fmt.Errorf("Silo rejected parameters for %s (HTTP 422)",path)
  }
  if resp.StatusCode!=200{return &siloHTTPError{Status:resp.StatusCode,Path:path}}
  if err:=json.Unmarshal(body,out);err!=nil{return errors.New("Silo did not return the expected JSON format")}
  return nil
 }
 return errors.New("Could not complete the request")
}

type page[T any] struct {
 Items []T `json:"items"`
 Page struct { HasMore bool `json:"has_more"`; NextCursor string `json:"next_cursor"` } `json:"page"`
}
func collection[T any](ctx context.Context,c *Client,path string,q url.Values)([]T,error){
 var all []T
 cursors:=map[string]bool{}
 for n:=0;n<maxQueryItems;n++ {
  var p page[T]
  if err:=c.get(ctx,path,q,&p);err!=nil{return nil,err}
  if p.Items==nil{return nil,errors.New("Collection incompatible with API v2")}
  all=append(all,p.Items...)
  if len(all)>maxQueryItems{return nil,errors.New("Up to 100000 items per query are supported")}
  if !p.Page.HasMore{return all,nil}
  if p.Page.NextCursor=="" || cursors[p.Page.NextCursor]{return nil,errors.New("Incomplete or repeated pagination")}
  cursors[p.Page.NextCursor]=true;q.Set("cursor",p.Page.NextCursor)
 }
 return nil,errors.New("Too many pages")
}
type Library struct { ID string `json:"id"`;Name string `json:"name"`;Type string `json:"type"` }
type Profile struct {ID string `json:"id"`;Name string `json:"name"`;Primary bool `json:"is_primary"` }
func(c *Client) Connect(ctx context.Context)error {
 var account struct{Role string `json:"role"`}
 if err:=c.get(ctx,"/account/me",url.Values{},&account);err!=nil{return err}
 if account.Role!="admin"{return errors.New("The API key must belong to an administrator account")}
 profiles,err:=collection[Profile](ctx,c,"/profiles",url.Values{});if err!=nil{return err}
 if c.profileSelector!="" {
  var matches []Profile
  for _,p:=range profiles {if p.ID==c.profileSelector {c.profile=p.ID;c.profileName=p.Name;return nil};if strings.EqualFold(p.Name,c.profileSelector){matches=append(matches,p)}}
  if len(matches)==1{c.profile=matches[0].ID;c.profileName=matches[0].Name;return nil}
  if len(matches)>1{return errors.New("Multiple profiles have that name; use a unique name")}
  return errors.New("Profile not found on the API key account; leave blank to use the primary profile")
 }
 for _,p:=range profiles{if p.Primary{c.profile=p.ID;c.profileName=p.Name;return nil}}
 return errors.New("The account has no primary profile; specify a profile name")

}
func(c *Client) Libraries(ctx context.Context)([]Library,error){
 return collection[Library](ctx,c,"/user/libraries",url.Values{})
}
func(c *Client) Items(ctx context.Context,library,kind string)([]Item,error){
 return collection[Item](ctx,c,"/catalog",url.Values{"library_id":{library},"type":{kind},"limit":{"200"}})
}
func(c *Client) Versions(ctx context.Context,id,library string)([]Version,error){
 return collection[Version](ctx,c,"/catalog/items/"+url.PathEscape(id)+"/versions",url.Values{"library_id":{library}})
}
type member struct{ID string `json:"id"`;Library string `json:"library_id"`;Path string `json:"file_path"`}
func(c *Client) Membership(ctx context.Context,id,library string)(map[string]string,error){
 rows,err:=collection[member](ctx,c,"/admin/items/"+url.PathEscape(id)+"/files",url.Values{"limit":{"200"}})
	if err != nil {
		var status *siloHTTPError
		if !errors.As(err, &status) || status.Status != http.StatusNotFound {
			return nil, err
		}
		// Administrative file lists belong to media items (movies/series), not episodes.
		var item struct {
			Type     string `json:"type"`
			SeriesID string `json:"series_id"`
		}
		if err = c.get(ctx, "/catalog/items/"+url.PathEscape(id), url.Values{"library_id": {library}}, &item); err != nil {
			return nil, err
		}
		if item.Type != "episode" || item.SeriesID == "" {
			return nil, status
		}
		rows, err = collection[member](ctx, c, "/admin/items/"+url.PathEscape(item.SeriesID)+"/files", url.Values{"limit": {"200"}})
		if err != nil {
			return nil, err
		}
	}
 out:=map[string]string{};for _,r:=range rows{if r.Library==library{out[r.ID]=r.Path}}
 return out,nil
}
