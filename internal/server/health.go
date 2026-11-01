// Provider reachability probing. The daemon is the only party that
// can answer "is the inference engine up?", so it checks each
// provider's OpenAI-compatible /models endpoint and tells the panels.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Teejer/ghostty-ai/internal/config"
)

var healthClient = &http.Client{Timeout: 3 * time.Second}

// providerReachable reports whether the endpoint answered at all.
// A 401 still counts: the server is up, the key is just missing or
// absent (many local servers need none). Only transport failures and
// 5xx mean "not usable".
func providerReachable(baseURL, apiKey, protocol string) bool {
	var req *http.Request
	var err error
	if protocol == "anthropic" {
		// No /models endpoint; an intentionally invalid tiny POST is
		// fine: any HTTP answer (even 4xx) proves the endpoint lives.
		url := strings.TrimSuffix(baseURL, "/")
		url = strings.TrimSuffix(url, "/v1") + "/v1/messages"
		req, err = http.NewRequest(http.MethodPost, url,
			strings.NewReader("{}"))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("anthropic-version", "2023-06-01")
			if apiKey != "" {
				req.Header.Set("x-api-key", apiKey)
			}
		}
	} else {
		req, err = http.NewRequest(http.MethodGet,
			strings.TrimSuffix(baseURL, "/")+"/models", nil)
		if err == nil && apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}
	if err != nil {
		return false
	}
	resp, err := healthClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}

// healthLoop fills the cache, then re-probes every 10 seconds,
// broadcasting provider_status whenever a provider flips.
func (s *server) healthLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	s.checkHealth()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkHealth()
		}
	}
}

func (s *server) checkHealth() {
	var flips []outbound
	for name, prov := range s.cfg.Providers {
		ok := providerReachable(prov.BaseURL, prov.APIKey, prov.Protocol)
		s.mu.Lock()
		prev, seen := s.health[name]
		s.health[name] = ok
		s.mu.Unlock()
		if seen && prev != ok {
			slog.Info("provider health changed", "provider", name, "ok", ok)
			flips = append(flips, outbound{
				Type: "provider_status", Provider: name, OK: ok,
			})
		}
	}
	for _, m := range flips {
		s.broadcast(m)
	}
}

// broadcast sends one message to every currently attached panel.
func (s *server) broadcast(m outbound) {
	s.mu.Lock()
	list := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		list = append(list, sess)
	}
	s.mu.Unlock()
	for _, sess := range list {
		sess.write(m)
	}
}

// infos returns the provider list stamped with live health, for the
// ready message.
func (s *server) infos() []config.ProviderInfo {
	infos := s.cfg.ProviderInfos()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range infos {
		infos[i].OK = s.health[infos[i].Name]
	}
	return infos
}
