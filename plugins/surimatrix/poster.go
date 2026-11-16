package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Posters carry no Silo or Matrix authorization headers. Validate DNS at dial
// time and every redirect, rather than trusting an artwork URL from metadata.
func publicAddress(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast()
}
func downloadPoster(ctx context.Context, raw string) ([]byte, string, error) {
	if e := httpsURL(raw); e != nil {
		return nil, "", e
	}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		ips, e := net.DefaultResolver.LookupIPAddr(ctx, host)
		if e != nil {
			return nil, e
		}
		if len(ips) == 0 {
			return nil, errors.New("DNS returned no addresses")
		}
		for _, ip := range ips {
			if !publicAddress(ip.IP) {
				return nil, errors.New("The poster points to a private network")
			}
		}
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}}
	defer transport.CloseIdleConnections()
	cli := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return errors.New("Too many redirects")
		}
		return httpsURL(req.URL.String())
	}}
	u, e := url.Parse(raw)
	if e != nil {
		return nil, "", e
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if e != nil {
		return nil, "", e
	}
	r, e := cli.Do(req)
	if e != nil {
		return nil, "", e
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return nil, "", errors.New("Unable to download the poster")
	}
	b, e := io.ReadAll(io.LimitReader(r.Body, (10<<20)+1))
	if e != nil {
		return nil, "", e
	}
	if len(b) > 10<<20 {
		return nil, "", errors.New("The poster exceeds 10 MiB")
	}
	mime := http.DetectContentType(b)
	switch mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return b, mime, nil
	}
	return nil, "", errors.New("The file is not a supported image")
}
