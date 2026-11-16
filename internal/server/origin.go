package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"time"
)

// TunnelOrigin asks the public site (notify.siteUrl) for /api/status and
// reports whether that request reached this app from loopback, i.e. whether
// cloudflared on this PC sends the site's traffic to localhost. Only then can
// the app stop listening on the network without cutting the tunnel.
func (s *Server) TunnelOrigin(ctx context.Context) (loopback bool, err error) {
	site := s.Store.Get().Notify.SiteURL
	if site == "" {
		return false, errors.New("no public site address in the settings")
	}
	b := make([]byte, 16)
	rand.Read(b)
	nonce := hex.EncodeToString(b)
	seen := make(chan bool, 1)
	s.probeMu.Lock()
	s.probeNonce, s.probeSeen = nonce, seen
	s.probeMu.Unlock()
	defer func() {
		s.probeMu.Lock()
		s.probeNonce, s.probeSeen = "", nil
		s.probeMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, site+"/api/status?origin-check="+nonce, nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	select {
	case lb := <-seen:
		return lb, nil
	default:
		return false, errors.New("the public site answered, but the request didn't reach this app")
	}
}

// noteProbe records where a TunnelOrigin request came from.
func (s *Server) noteProbe(r *http.Request) {
	q := r.URL.Query().Get("origin-check")
	if q == "" {
		return
	}
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if q != s.probeNonce || s.probeSeen == nil {
		return
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	select {
	case s.probeSeen <- ip != nil && ip.IsLoopback():
	default:
	}
}
