package review

import (
 "errors"
 "math"
 "net/url"
 "strings"
 "time"
)

var ErrHostUnavailable = errors.New("Silo has not supplied its internal address; wait or configure the server URL")

type Config struct {
 DeleteEnabled bool `json:"delete_enabled"`
 MediaRoots string `json:"media_roots"`
 APIKey string `json:"api_key"`
 BaseURL string `json:"base_url"`
 ProfileID string `json:"profile_id"`
 RequestsPerSecond float64 `json:"requests_per_second"`
 Concurrency int `json:"concurrency"`
}
const maxQueryItems = 100000
const scanTimeout = 2 * time.Hour

func DefaultConfig() Config { return Config{RequestsPerSecond:100,Concurrency:16} }
func (c Config) Validate() error {
 if strings.TrimSpace(c.APIKey)=="" { return errors.New("Configure an administrator API key in plugin settings") }
 if c.RequestsPerSecond < 1 || c.RequestsPerSecond > 100 || math.IsNaN(c.RequestsPerSecond) || math.IsInf(c.RequestsPerSecond,0) {
  return errors.New("Rate must be between 1 and 100 requests/s")
 }
 if c.Concurrency < 1 || c.Concurrency > 16 { return errors.New("Concurrency must be between 1 and 16") }
 if c.BaseURL!="" { return validateBase(c.BaseURL) }
 return nil
}
func validateBase(raw string) error {
 u,err:=url.Parse(raw)
 if err!=nil || (u.Scheme!="http" && u.Scheme!="https") || u.Host=="" || u.User!=nil || u.RawQuery!="" || u.Fragment!="" {
  return errors.New("Invalid Silo URL: use HTTP(S) without credentials or query parameters")
 }
 return nil
}
