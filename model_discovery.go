package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Older tenants still expose the Act and Plan catalogs. They are only queried
// as a fallback when the current account's agent list is unavailable or empty.
var agentModeCatalogIDs = []string{"0a31170db80141e3b4119c2680f42af0", "c497c5d68d5a4d8fb7d58ef84df5f685"}

// modelCatalogResult is shared by the host's account model list and the plugin
// panel. Warnings retain failures of one source even if another source works.
type modelCatalogResult struct {
	Models    []ModelConfig `json:"models"`
	Warnings  []string      `json:"warnings,omitempty"`
	Source    string        `json:"source"`
	FetchedAt time.Time     `json:"fetched_at,omitempty"`
}

type modelCacheEntry struct {
	catalog modelCatalogResult
	expires time.Time
}

var discoveredModels = struct {
	sync.Mutex
	entries map[string]modelCacheEntry
}{entries: make(map[string]modelCacheEntry)}

// benefitCatalogTimeout keeps an optional/slow benefit gateway from blocking
// the account's already available Agent Center models until CPA abandons the
// whole model.for_auth call. The request continues in its buffered goroutine and
// a short cache TTL retries the missing source later.
var benefitCatalogTimeout = 8 * time.Second

type benefitCatalogResult struct {
	models []ModelConfig
	err    error
}

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
	cred, _ := credentialFromStorage(req.StorageJSON)
	if cred != nil && cred.valid() {
		if refreshed, errRefresh := prepareCredentialForUse(req.AuthID, cred); errRefresh == nil {
			cred = refreshed
		} else {
			logWarn("expired credential could not be refreshed before model discovery", map[string]any{
				"auth_id": req.AuthID,
				"error":   errRefresh.Error(),
			})
		}
	}
	catalog := accountModelCatalog(config(), cred, req.HostCallbackID)
	if len(catalog.Warnings) > 0 {
		logWarn("account model discovery incomplete", map[string]any{"auth_id": req.AuthID, "source": catalog.Source, "warnings": catalog.Warnings})
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: providerID, Models: infosForModels(catalog.Models)})
}

func accountModelCatalog(cfg *Config, cred *credential, callbackID string) modelCatalogResult {
	result := modelCatalogResult{Models: []ModelConfig{}, Source: "unavailable"}
	if cfg == nil {
		result.Warnings = []string{"Model discovery configuration is unavailable"}
		return result
	}
	if cfg.DiscoverModels {
		if cred.valid() {
			result = discoverModelCatalog(cfg, cred, callbackID)
		} else {
			result.Warnings = []string{"Sign in to discover the models available to this account"}
		}
	}
	if len(result.Models) == 0 {
		// Only the operator's explicit configuration may be used as a fallback;
		// an unsuccessful request must not invent a supposedly available model.
		if len(cfg.Models) > 0 {
			result.Models = append([]ModelConfig(nil), cfg.Models...)
			result.Source = "configured"
		}
		return result
	}
	// Keep explicit aliases only when their target was discovered for this
	// account. Carry the target's route so aliases can invoke benefit models.
	byID := make(map[string]ModelConfig, len(result.Models))
	for _, model := range result.Models {
		byID[model.ID] = model
	}
	for _, model := range cfg.Models {
		target, ok := byID[cfg.ModelMap[model.ID]]
		if _, exists := byID[model.ID]; exists || !ok {
			continue
		}
		model.Source = target.Source
		result.Models = append(result.Models, model)
		byID[model.ID] = model
	}
	return result
}

// discoverAccountModels remains available for callers that only need models.
// Management callers should use accountModelCatalog to expose partial failures.
func discoverAccountModels(cfg *Config, cred *credential, callbackID string) ([]ModelConfig, error) {
	if cfg == nil || !cred.valid() {
		return nil, fmt.Errorf("model discovery requires configuration and an account credential")
	}
	result := discoverModelCatalog(cfg, cred, callbackID)
	if len(result.Models) == 0 {
		return nil, fmt.Errorf("%s", strings.Join(result.Warnings, "; "))
	}
	return result.Models, nil
}

// Cache entries are isolated by the full discovery configuration and credential,
// including rotated tokens. Only hashes are retained as keys.
func modelCatalogCacheKey(cfg *Config, cred *credential) string {
	data, _ := json.Marshal(struct {
		Config     *Config
		Credential *credential
	}{cfg, cred})
	return sha256Hex(data)
}

func cloneModelCatalog(catalog modelCatalogResult) modelCatalogResult {
	catalog.Models = append([]ModelConfig{}, catalog.Models...)
	catalog.Warnings = append([]string(nil), catalog.Warnings...)
	return catalog
}

func discoverModelCatalog(cfg *Config, cred *credential, callbackID string) modelCatalogResult {
	key := modelCatalogCacheKey(cfg, cred)
	discoveredModels.Lock()
	entry, ok := discoveredModels.entries[key]
	discoveredModels.Unlock()
	if ok && time.Now().Before(entry.expires) {
		return cloneModelCatalog(entry.catalog)
	}
	result := modelCatalogResult{Models: []ModelConfig{}, Source: "unavailable", FetchedAt: time.Now().UTC()}
	var benefitResults <-chan benefitCatalogResult
	var benefitTimer *time.Timer
	if cfg.APIMode != "native" && cfg.BenefitGatewayURL != "" {
		results := make(chan benefitCatalogResult, 1)
		benefitResults = results
		benefitTimer = time.NewTimer(benefitCatalogTimeout)
		go func() {
			models, err := discoverBenefitModels(cfg, cred, callbackID)
			results <- benefitCatalogResult{models: models, err: err}
		}()
	}
	ids := append([]string(nil), cfg.ModelAgentIDs...)
	if len(ids) == 0 {
		if cfg.APIMode == "native" {
			ids = []string{cfg.AgentID}
		} else {
			var warnings []string
			ids, warnings = discoverAgentCatalogIDs(cfg, cred, callbackID)
			result.Warnings = append(result.Warnings, warnings...)
			if len(ids) == 0 {
				ids = append([]string(nil), agentModeCatalogIDs...)
			}
		}
	}
	seenIDs := make(map[string]bool)
	seenModels := make(map[string]bool)
	appendModels := func(models []ModelConfig) {
		for _, model := range models {
			if !seenModels[model.ID] {
				seenModels[model.ID] = true
				result.Models = append(result.Models, model)
			}
		}
	}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seenIDs[id] {
			continue
		}
		seenIDs[id] = true
		endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/v1/agent-center/agents/detail?agent_id=" + url.QueryEscape(id)
		body, err := requestModelCatalog(cfg, cred, callbackID, endpoint, "AgentCenter")
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("Agent catalog %s: %v", id, err))
			continue
		}
		models, err := parseAgentModelsLanguage(body, cfg.Language)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("Agent catalog %s: %v", id, err))
			continue
		}
		appendModels(models)
	}
	if benefitResults != nil {
		select {
		case benefit := <-benefitResults:
			if benefitTimer != nil {
				benefitTimer.Stop()
			}
			if benefit.err != nil {
				result.Warnings = append(result.Warnings, "Benefit model catalog: "+benefit.err.Error())
			}
			appendModels(benefit.models)
		case <-benefitTimer.C:
			result.Warnings = append(result.Warnings, fmt.Sprintf("Benefit model catalog: timed out after %s", benefitCatalogTimeout))
		}
	}
	// Merged last: appendModels keeps the first entry per id, so ids the agent
	// and benefit catalogues also report keep the route those sources advertise,
	// and the built-in list only contributes models nothing else reports.
	if cfg.APIMode != "native" {
		builtin, errBuiltin := discoverBuiltinModels(cfg, cred, callbackID)
		if errBuiltin != nil {
			result.Warnings = append(result.Warnings, "Builtin model catalog: "+errBuiltin.Error())
		}
		appendModels(builtin)
	}
	if len(result.Models) == 0 {
		result.Warnings = append(result.Warnings, "No enabled models were returned for this account")
		return result
	}
	result.Source = "discovered"
	ttl := 5 * time.Minute
	if len(result.Warnings) > 0 {
		// Retry missing sources promptly without discarding the working models.
		ttl = 30 * time.Second
	}
	now := time.Now()
	discoveredModels.Lock()
	for cacheKey, value := range discoveredModels.entries {
		if !now.Before(value.expires) {
			delete(discoveredModels.entries, cacheKey)
		}
	}
	if len(discoveredModels.entries) < 512 {
		discoveredModels.entries[key] = modelCacheEntry{catalog: cloneModelCatalog(result), expires: now.Add(ttl)}
	}
	discoveredModels.Unlock()
	return result
}

func requestModelCatalog(cfg *Config, cred *credential, callbackID, endpoint, agentType string) ([]byte, error) {
	headers := baseUpstreamHeaders(cfg, executorRequest{})
	headers["Agent-Type"] = agentType
	headers["Accept"] = "application/json"
	signed, err := signRequest(http.MethodGet, endpoint, headers, nil, cred, cfg.SignHost)
	if err != nil {
		return nil, err
	}
	response, err := hostHTTPDoContext(callbackID, http.MethodGet, endpoint, signed, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return response.Body, nil
}

// discoverBuiltinModels reads the built-in model catalogue. The official client
// asks for it with Agent-Type: PromptCenter; with any other agent type the
// gateway answers with that agent's restricted view and the list comes back
// empty, which is indistinguishable from "this account has no models".
func discoverBuiltinModels(cfg *Config, cred *credential, callbackID string) ([]ModelConfig, error) {
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/v1/model/builtin"
	body, err := requestModelCatalog(cfg, cred, callbackID, endpoint, "PromptCenter")
	if err != nil {
		return nil, err
	}
	return parseBuiltinModels(body)
}

func parseBuiltinModels(body []byte) ([]ModelConfig, error) {
	var catalogue struct {
		BuiltinModels []struct {
			ModelID       string `json:"model_id"`
			ModelName     string `json:"model_name"`
			Enable        *bool  `json:"enable"`
			ContextWindow int64  `json:"context_window"`
			MaxTokens     int64  `json:"max_tokens"`
			SupportImages bool   `json:"supports_images"`
			Description   string `json:"model_desc"`
		} `json:"builtinModels"`
	}
	if err := json.Unmarshal(body, &catalogue); err != nil {
		return nil, fmt.Errorf("invalid builtin model response: %w", err)
	}
	var models []ModelConfig
	for _, entry := range catalogue.BuiltinModels {
		if entry.Enable != nil && !*entry.Enable {
			continue
		}
		id := firstNonEmptyString(entry.ModelID, entry.ModelName)
		if id == "" {
			continue
		}
		models = append(models, ModelConfig{
			ID:              id,
			Name:            firstNonEmptyString(entry.ModelName, id),
			DisplayName:     firstNonEmptyString(entry.ModelName, id),
			Description:     entry.Description,
			ContextLength:   entry.ContextWindow,
			MaxOutputTokens: entry.MaxTokens,
			SupportsImages:  entry.SupportImages,
			Source:          "builtin",
		})
	}
	return models, nil
}

func discoverAgentCatalogIDs(cfg *Config, cred *credential, callbackID string) ([]string, []string) {
	const pageSize = 100
	const maxPages = 100
	var ids []string
	seen := make(map[string]bool)
	for page := 0; page < maxPages; page++ {
		query := url.Values{
			"offset":                        {strconv.Itoa(page * pageSize)},
			"limit":                         {strconv.Itoa(pageSize)},
			"is_primary_agent":              {"true"},
			"supported_client":              {"VSCODE"},
			"min_compatible_plugin_version": {cfg.PluginVersion},
		}
		endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/v1/agent-center/agents/useragents?" + query.Encode()
		body, err := requestModelCatalog(cfg, cred, callbackID, endpoint, "AgentCenter")
		if err != nil {
			return ids, []string{fmt.Sprintf("Account agent list: %v", err)}
		}
		var response struct {
			Total  *int `json:"total"`
			Agents *[]struct {
				ID         string `json:"agent_id"`
				OriginalID string `json:"original_id"`
			} `json:"agents"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return ids, []string{fmt.Sprintf("Invalid account agent list: %v", err)}
		}
		if response.Agents == nil {
			return ids, []string{"Account agent list response has no agents array"}
		}
		for _, agent := range *response.Agents {
			// original_id is the system role; customized agents are queried by
			// their real agent_id, just as the extension's getGptInfo does.
			id := strings.TrimSpace(firstNonEmptyString(agent.ID, agent.OriginalID))
			if id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		if len(*response.Agents) < pageSize || (response.Total != nil && (page+1)*pageSize >= *response.Total) {
			return ids, nil
		}
	}
	return ids, []string{"Account agent list exceeded the 100-page discovery limit"}
}

func discoverBenefitModels(cfg *Config, cred *credential, callbackID string) ([]ModelConfig, error) {
	gateURL := strings.TrimRight(cfg.BaseURL, "/") + "/v1/benefit-gateway-config"
	body, err := requestModelCatalog(cfg, cred, callbackID, gateURL, "PromptCenter")
	if err != nil {
		return nil, fmt.Errorf("availability check: %w", err)
	}
	var gate struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(body, &gate); err != nil {
		return nil, fmt.Errorf("invalid availability response: %w", err)
	}
	if gate.Enabled == nil {
		return nil, fmt.Errorf("availability response has no enabled flag")
	}
	if !*gate.Enabled {
		return nil, nil
	}
	endpoint := strings.TrimRight(cfg.BenefitGatewayURL, "/") + "/api/v1/gateway/config"
	// The benefit gateway signs only host, date and STS token. It uses the
	// same temporary AK/SK as the regional API, without its domain header.
	gatewayCredential := *cred
	gatewayCredential.DomainID = ""
	headers, err := signRequest(http.MethodGet, endpoint, nil, nil, &gatewayCredential, true)
	if err != nil {
		return nil, err
	}
	response, err := hostHTTPDoContext(callbackID, http.MethodGet, endpoint, headers, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway returned HTTP %d", response.StatusCode)
	}
	return parseBenefitModels(response.Body)
}

func parseBenefitModels(body []byte) ([]ModelConfig, error) {
	var response struct {
		ErrorCode string `json:"error_code"`
		Result    *struct {
			Models []struct {
				ID            string `json:"model_id"`
				Name          string `json:"model_name"`
				ContextWindow int64  `json:"context_window"`
				MaxTokens     int64  `json:"max_tokens"`
				Sort          int    `json:"sort"`
			} `json:"models"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("invalid gateway response: %w", err)
	}
	if response.ErrorCode != "0000" || response.Result == nil {
		return nil, fmt.Errorf("gateway catalog was not successful (error_code=%q)", response.ErrorCode)
	}
	sort.SliceStable(response.Result.Models, func(i, j int) bool {
		return response.Result.Models[i].Sort < response.Result.Models[j].Sort
	})
	var models []ModelConfig
	for _, model := range response.Result.Models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		models = append(models, ModelConfig{ID: id, Name: id, DisplayName: firstNonEmptyString(model.Name, id),
			ContextLength: model.ContextWindow, MaxOutputTokens: model.MaxTokens, Source: "benefit"})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("gateway returned no models")
	}
	return models, nil
}

func parseAgentModels(body []byte) ([]ModelConfig, error) {
	return parseAgentModelsLanguage(body, "")
}

func parseAgentModelsLanguage(body []byte, language string) ([]ModelConfig, error) {
	var response struct {
		GPTs *struct {
			Models []struct {
				Alias      string `json:"model_alias"`
				ID         string `json:"model_id"`
				Name       string `json:"model_name"`
				Parameters struct {
					ID             string `json:"model_id"`
					Enabled        *bool  `json:"enabled"`
					DisplayEnabled *bool  `json:"display_enabled"`
					ContextWindow  int64  `json:"context_window"`
					MaxTokens      int64  `json:"max_tokens"`
					SupportsImages bool   `json:"supports_images"`
					Description    string `json:"model_desc"`
					DescriptionEN  string `json:"model_desc_en"`
				} `json:"model_parameters"`
			} `json:"models"`
		} `json:"gpts"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("invalid Agent Center response: %w", err)
	}
	if response.GPTs == nil {
		return nil, fmt.Errorf("Agent Center response has no gpts catalog")
	}
	var models []ModelConfig
	for _, model := range response.GPTs.Models {
		if (model.Parameters.Enabled != nil && !*model.Parameters.Enabled) ||
			model.Parameters.DisplayEnabled == nil || !*model.Parameters.DisplayEnabled {
			continue
		}
		// The extension spreads model_parameters over model_alias, so its
		// nested model_id takes precedence when the upstream supplies one.
		id := strings.TrimSpace(firstNonEmptyString(model.Parameters.ID, model.Alias, model.ID))
		if id == "" {
			continue
		}
		description := firstNonEmptyString(model.Parameters.Description, model.Parameters.DescriptionEN)
		if strings.HasPrefix(strings.ToLower(language), "en") {
			description = firstNonEmptyString(model.Parameters.DescriptionEN, model.Parameters.Description)
		}
		models = append(models, ModelConfig{ID: id, Name: id, DisplayName: firstNonEmptyString(model.Name, id),
			Description: description, ContextLength: model.Parameters.ContextWindow, MaxOutputTokens: model.Parameters.MaxTokens,
			SupportsImages: model.Parameters.SupportsImages, Source: "agent"})
	}
	return models, nil
}
