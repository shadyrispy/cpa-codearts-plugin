package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// These are the Act and Plan agents used by extension 26.3.6's
// setSpecialAgent/getActAndPlantModel. Model IDs are model_alias, not model_id.
var agentModeCatalogIDs = []string{"0a31170db80141e3b4119c2680f42af0", "c497c5d68d5a4d8fb7d58ef84df5f685"}

type modelCacheEntry struct {
	models  []ModelConfig
	expires time.Time
}

var discoveredModels = struct {
	sync.Mutex
	entries map[string]modelCacheEntry
}{entries: make(map[string]modelCacheEntry)}

func modelsForAuth(raw []byte) ([]byte, error) {
	var req struct {
		pluginapi.AuthModelRequest
		HostCallbackID string `json:"host_callback_id"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if req.AuthProvider != "" && normalizeProvider(req.AuthProvider) != providerID {
		return okEnvelope(pluginapi.ModelResponse{})
	}
	cfg := config()
	models := cfg.Models
	cred, err := credentialFromStorage(req.StorageJSON)
	if cfg.DiscoverModels && err == nil && cred.valid() {
		found, err := discoverAccountModels(cfg, cred, req.HostCallbackID)
		if err == nil {
			models = found
			// Explicit configured aliases remain usable alongside discovered models.
			seen := map[string]bool{}
			for _, m := range models {
				seen[m.ID] = true
			}
			for _, m := range cfg.Models {
				if target := cfg.ModelMap[m.ID]; target != "" && seen[target] && !seen[m.ID] {
					models = append(models, m)
					seen[m.ID] = true
				}
			}
		} else {
			logWarn("model discovery failed; using configured models", map[string]any{"auth_id": req.AuthID, "error": err.Error()})
		}
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: providerID, Models: infosForModels(models)})
}

func discoverAccountModels(cfg *Config, cred *credential, callbackID string) ([]ModelConfig, error) {
	ids := cfg.ModelAgentIDs
	if len(ids) == 0 {
		ids = agentModeCatalogIDs
		if cfg.APIMode == "native" {
			ids = []string{cfg.AgentID}
		}
	}
	key := sha256Hex([]byte(cfg.BaseURL + "\x00" + cred.AccessKeyID + "\x00" + cred.DomainID + "\x00" + strings.Join(ids, ",")))
	discoveredModels.Lock()
	entry, ok := discoveredModels.entries[key]
	discoveredModels.Unlock()
	if ok && time.Now().Before(entry.expires) {
		return append([]ModelConfig(nil), entry.models...), nil
	}
	var models []ModelConfig
	seen := map[string]bool{}
	var lastErr error
	for _, id := range ids {
		endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/v1/agent-center/agents/detail?agent_id=" + url.QueryEscape(id)
		headers := baseUpstreamHeaders(cfg, executorRequest{})
		headers["Agent-Type"] = "AgentCenter"
		headers["Accept"] = "application/json"
		signed, err := signRequest(http.MethodGet, endpoint, headers, nil, cred, cfg.SignHost)
		if err != nil {
			return nil, err
		}
		response, err := hostHTTPDoContext(callbackID, http.MethodGet, endpoint, signed, nil)
		if err != nil {
			lastErr = err
			continue
		}
		if response.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("Agent Center returned HTTP %d", response.StatusCode)
			continue
		}
		found, err := parseAgentModels(response.Body)
		if err != nil {
			lastErr = err
			continue
		}
		for _, m := range found {
			if !seen[m.ID] {
				seen[m.ID] = true
				models = append(models, m)
			}
		}
	}
	if len(models) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("Agent Center returned no enabled models")
	}
	discoveredModels.Lock()
	// Keep account cache bounded and avoid retaining expired credentials' entries.
	for k, v := range discoveredModels.entries {
		if time.Now().After(v.expires) {
			delete(discoveredModels.entries, k)
		}
	}
	if len(discoveredModels.entries) < 512 {
		discoveredModels.entries[key] = modelCacheEntry{models: append([]ModelConfig(nil), models...), expires: time.Now().Add(5 * time.Minute)}
	}
	discoveredModels.Unlock()
	return models, nil
}

func parseAgentModels(body []byte) ([]ModelConfig, error) {
	var response struct {
		GPTs struct {
			Models []struct {
				Alias      string `json:"model_alias"`
				ID         string `json:"model_id"`
				Name       string `json:"model_name"`
				Parameters struct {
					Enabled        *bool `json:"enabled"`
					ContextWindow  int64 `json:"context_window"`
					MaxTokens      int64 `json:"max_tokens"`
					SupportsImages bool  `json:"supports_images"`
				} `json:"model_parameters"`
			} `json:"models"`
		} `json:"gpts"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("invalid Agent Center response: %w", err)
	}
	var models []ModelConfig
	for _, m := range response.GPTs.Models {
		if m.Parameters.Enabled != nil && !*m.Parameters.Enabled {
			continue
		}
		id := firstNonEmptyString(m.Alias, m.ID)
		if id == "" {
			continue
		}
		models = append(models, ModelConfig{ID: id, Name: id, DisplayName: firstNonEmptyString(m.Name, id),
			ContextLength: m.Parameters.ContextWindow, MaxOutputTokens: m.Parameters.MaxTokens, SupportsImages: m.Parameters.SupportsImages})
	}
	return models, nil
}
