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

type modelCatalogFlight struct {
	mu   sync.Mutex
	refs int
}

var modelCatalogFlights = struct {
	sync.Mutex
	entries map[string]*modelCatalogFlight
}{entries: make(map[string]*modelCatalogFlight)}

// benefitCatalogueSnapshot is the last benefit model list the plugin confirmed
// for one account, kept inside the plugin-owned part of the credential.
//
// CPA asks for plugin models on its cold-start registration path, and the
// benefit gateway must not be contacted there: a slow answer makes the host drop
// the whole registration, which is worse than losing the optional models. That
// leaves the time-limited benefit models invisible after every restart until an
// operator opens the management route by hand. Replaying the previous list needs
// no network at all, so the cold path stays bounded.
type benefitCatalogueSnapshot struct {
	FetchedAt time.Time     `json:"fetched_at"`
	Models    []ModelConfig `json:"models"`
}

// benefitCatalogueRevalidate bounds how long a confirmed list is treated as
// current before another background read is attempted.
var benefitCatalogueRevalidate = 6 * time.Hour

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
	// CPA calls this on its cold-start registration path. Keep that path free of
	// the optional benefit gateway: a slow request there must never hide working
	// Agent Center models. Operator-confirmed benefit_models and the list the
	// scheduled account refresh left on this credential are still merged by
	// accountAgentModelCatalog without network I/O.
	catalog := accountAgentModelCatalog(config(), cred, req.HostCallbackID)
	if len(catalog.Warnings) > 0 {
		logWarn("account model discovery incomplete", map[string]any{"auth_id": req.AuthID, "source": catalog.Source, "warnings": catalog.Warnings})
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: providerID, Models: infosForModels(catalog.Models)})
}

func accountModelCatalog(cfg *Config, cred *credential, callbackID string) modelCatalogResult {
	return accountModelCatalogScoped(cfg, cred, callbackID, true)
}

// accountAgentModelCatalog is the bounded catalogue used by CPA registration
// and request routing. It performs no live benefit-gateway I/O; that optional
// source is queried only by an explicit management request.
func accountAgentModelCatalog(cfg *Config, cred *credential, callbackID string) modelCatalogResult {
	return accountModelCatalogScoped(cfg, cred, callbackID, false)
}

func accountModelCatalogScoped(cfg *Config, cred *credential, callbackID string, includeLiveBenefit bool) modelCatalogResult {
	result := modelCatalogResult{Models: []ModelConfig{}, Source: "unavailable"}
	if cfg == nil {
		result.Warnings = []string{"Model discovery configuration is unavailable"}
		return result
	}
	if cfg.DiscoverModels {
		if cred.valid() {
			if includeLiveBenefit {
				result = discoverModelCatalog(cfg, cred, callbackID)
			} else {
				result = discoverAgentModelCatalog(cfg, cred, callbackID)
				// Replay benefit entries that are already known: either this process
				// fetched them (an explicit management refresh or a background warm
				// filled the full cache), or a previous process confirmed them and
				// left the list on the credential. Neither source performs benefit
				// network I/O on this critical path.
				result = mergeBenefitModels(result, cachedBenefitModels(cfg, cred))
				if cred.BenefitCatalogue != nil {
					result = mergeBenefitModels(result, cred.BenefitCatalogue.Models)
				}
			}
		} else {
			result.Warnings = []string{"Sign in to discover the models available to this account"}
		}
	}
	if len(result.Models) == 0 {
		// Only the operator's explicit configuration may be used as a fallback;
		// an unsuccessful request must not invent a supposedly available model.
		if len(cfg.Models) == 0 && len(cfg.BenefitModels) == 0 {
			return result
		}
		result.Models = append([]ModelConfig(nil), cfg.Models...)
		result.Source = "configured"
	}
	// Keep explicit aliases only when their target was discovered for this
	// account. Carry the target's route so aliases can invoke benefit models.
	byID := make(map[string]int, len(result.Models)+len(cfg.BenefitModels))
	for index, model := range result.Models {
		if model.ID != "" {
			byID[model.ID] = index
		}
	}
	// benefit_models are an explicit last-known-good catalogue. They are safe to
	// merge on the cold-start path because doing so performs no network request.
	// If an ID appears in both configured lists, the explicit benefit route wins.
	for _, model := range cfg.BenefitModels {
		if model.ID == "" {
			continue
		}
		if index, exists := byID[model.ID]; exists {
			result.Models[index].Source = "benefit"
			continue
		}
		model.Source = "benefit"
		result.Models = append(result.Models, model)
		byID[model.ID] = len(result.Models) - 1
	}
	// Models present because they are explicit fallbacks may themselves be
	// aliases. Resolve their source after benefit entries have been merged.
	for index := range result.Models {
		targetIndex, ok := byID[cfg.ModelMap[result.Models[index].ID]]
		if ok {
			result.Models[index].Source = result.Models[targetIndex].Source
		}
	}
	for _, model := range cfg.Models {
		targetIndex, ok := byID[cfg.ModelMap[model.ID]]
		if _, exists := byID[model.ID]; exists || !ok {
			continue
		}
		model.Source = result.Models[targetIndex].Source
		result.Models = append(result.Models, model)
		byID[model.ID] = len(result.Models) - 1
	}
	return result
}

// discoverAccountModels remains available for callers that only need models.
// Management callers should use accountModelCatalog to expose partial failures.
func discoverAccountModels(cfg *Config, cred *credential, callbackID string) ([]ModelConfig, error) {
	if cfg == nil || !cred.valid() {
		return nil, fmt.Errorf("model discovery requires configuration and an account credential")
	}
	result := discoverAgentModelCatalog(cfg, cred, callbackID)
	if len(result.Models) == 0 {
		return nil, fmt.Errorf("%s", strings.Join(result.Warnings, "; "))
	}
	return result.Models, nil
}

// Cache entries are isolated by the full discovery configuration and credential,
// including rotated tokens. Only hashes are retained as keys.
func modelCatalogCacheKey(cfg *Config, cred *credential, scope string) string {
	// The replayed benefit list is not part of what a catalogue describes, so a
	// credential whose list was just refreshed must keep hitting the cached agent
	// catalogues instead of refetching them.
	target := cred
	if cred != nil {
		plain := *cred
		plain.BenefitCatalogue = nil
		target = &plain
	}
	data, _ := json.Marshal(struct {
		Config     *Config
		Credential *credential
		Scope      string
	}{cfg, target, scope})
	return sha256Hex(data)
}

func cloneModelCatalog(catalog modelCatalogResult) modelCatalogResult {
	catalog.Models = append([]ModelConfig{}, catalog.Models...)
	catalog.Warnings = append([]string(nil), catalog.Warnings...)
	return catalog
}

func cachedModelCatalog(key string) (modelCatalogResult, bool) {
	discoveredModels.Lock()
	entry, ok := discoveredModels.entries[key]
	discoveredModels.Unlock()
	if ok && time.Now().Before(entry.expires) {
		return cloneModelCatalog(entry.catalog), true
	}
	return modelCatalogResult{}, false
}

func storeModelCatalog(key string, result modelCatalogResult) {
	ttl := 5 * time.Minute
	if len(result.Warnings) > 0 {
		// Retry missing sources promptly without discarding working models.
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
}

// cachedBenefitModels returns the benefit entries a live read already fetched for
// this account, or nothing when that read has not happened or has expired.
func cachedBenefitModels(cfg *Config, cred *credential) []ModelConfig {
	full, ok := cachedModelCatalog(modelCatalogCacheKey(cfg, cred, "full"))
	if !ok {
		return nil
	}
	models := make([]ModelConfig, 0, len(full.Models))
	for _, model := range full.Models {
		if model.Source == "benefit" {
			models = append(models, model)
		}
	}
	return models
}

// mergeBenefitModels adds the benefit entries the agent catalogues do not carry,
// and keeps the benefit route for names both sides list.
func mergeBenefitModels(result modelCatalogResult, benefit []ModelConfig) modelCatalogResult {
	seen := make(map[string]bool, len(result.Models)+len(benefit))
	for _, model := range result.Models {
		seen[model.ID] = true
	}
	for _, model := range benefit {
		if model.ID == "" || seen[model.ID] {
			continue
		}
		seen[model.ID] = true
		model.Source = "benefit"
		result.Models = append(result.Models, model)
	}
	return result
}

// refreshAccountBenefitCatalogue confirms the benefit list while an account is
// being refreshed — a place where the optional gateway cannot delay anything a
// client waits for. A failure is logged only: the refresh itself succeeded.
func refreshAccountBenefitCatalogue(authIndex string, cred *credential) {
	if errConfirm := confirmBenefitCatalogue(config(), authIndex, cred, time.Now()); errConfirm != nil {
		logWarn("benefit catalogue refresh failed", map[string]any{
			"auth_index": authIndex, "error": errConfirm.Error(),
		})
	}
}

// benefitCatalogueStale reports whether this account should confirm its benefit
// list again. Live benefit discovery has to be switched on to know anything, so
// every gate that guards the live read also guards the refresh.
func benefitCatalogueStale(cfg *Config, cred *credential, now time.Time) bool {
	if cfg == nil || !cfg.DiscoverModels || cfg.APIMode == "native" || strings.TrimSpace(cfg.BenefitGatewayURL) == "" {
		return false
	}
	if !cred.valid() {
		return false
	}
	if cred.BenefitCatalogue == nil || len(cred.BenefitCatalogue.Models) == 0 {
		return true
	}
	return now.Sub(cred.BenefitCatalogue.FetchedAt) >= benefitCatalogueRevalidate
}

// confirmBenefitCatalogue reads the live benefit list for one account and leaves
// it on the credential, so the next registration (and every restart after that)
// can advertise those models without ever putting a benefit request on a
// critical path. It runs from the scheduled account refresh, never from a
// registration reply. An unsuccessful read keeps the previous list.
func confirmBenefitCatalogue(cfg *Config, authIndex string, cred *credential, now time.Time) error {
	if !benefitCatalogueStale(cfg, cred, now) {
		return nil
	}
	full := discoverModelCatalog(cfg, cred, "")
	benefit := make([]ModelConfig, 0, len(full.Models))
	for _, model := range full.Models {
		if model.Source == "benefit" {
			benefit = append(benefit, model)
		}
	}
	if len(benefit) == 0 {
		if len(full.Warnings) > 0 {
			return fmt.Errorf("no benefit catalogue to remember: %s", strings.Join(full.Warnings, "; "))
		}
		return nil
	}
	if benefitCatalogueUnchanged(cred.BenefitCatalogue, benefit) {
		return nil
	}
	return persistBenefitCatalogue(authIndex, benefit, now)
}

// benefitCatalogueUnchanged ignores arrival order and the stored timestamp, so a
// stable list does not rewrite the credential on every refresh.
func benefitCatalogueUnchanged(stored *benefitCatalogueSnapshot, fresh []ModelConfig) bool {
	if stored == nil || len(stored.Models) != len(fresh) {
		return false
	}
	byID := make(map[string]ModelConfig, len(stored.Models))
	for _, model := range stored.Models {
		byID[model.ID] = model
	}
	for _, model := range fresh {
		known, exists := byID[model.ID]
		if !exists || known.ContextLength != model.ContextLength || known.MaxOutputTokens != model.MaxOutputTokens {
			return false
		}
	}
	return true
}

// persistBenefitCatalogue stores the confirmed benefit list in the plugin-owned
// part of one credential. It re-reads what the host holds right now and adds
// only this field, so a token rotated while the list was being read is never
// rolled back, and host-owned keys (priority, note, weight, ...) survive.
func persistBenefitCatalogue(authRef string, models []ModelConfig, fetchedAt time.Time) error {
	files, errList := hostAuthList()
	if errList != nil {
		return errList
	}
	for _, file := range files {
		if normalizeProvider(file.Provider) != providerID && normalizeProvider(file.Type) != providerID {
			continue
		}
		if authRef != "" && file.Name != authRef && file.ID != authRef && file.AuthIndex != authRef {
			continue
		}
		storage, errGet := hostAuthGet(file.AuthIndex)
		if errGet != nil {
			return errGet
		}
		current, errCred := credentialFromStorage(storage)
		if errCred != nil || !current.valid() {
			return fmt.Errorf("the stored credential is unreadable")
		}
		catalogue := make([]ModelConfig, 0, len(models))
		for _, model := range models {
			model.Source = "benefit"
			catalogue = append(catalogue, model)
		}
		current.BenefitCatalogue = &benefitCatalogueSnapshot{FetchedAt: fetchedAt, Models: catalogue}
		updated, errMarshal := json.Marshal(map[string]any{storageKey: *current})
		if errMarshal != nil {
			return errMarshal
		}
		wrap, errMerge := mergeCredentialFile(storage, updated)
		if errMerge != nil {
			return errMerge
		}
		if file.Name == "" {
			return fmt.Errorf("credential %s has no file name to save to", file.AuthIndex)
		}
		return hostAuthSave(file.Name, wrap)
	}
	return fmt.Errorf("no stored credential matches %s", authRef)
}

// acquireModelCatalogFlight coalesces concurrent cache misses for the same
// account/configuration. The reference count lets entries be removed safely:
// a waiter increments refs before the current owner can delete the lock.
func acquireModelCatalogFlight(key string) func() {
	modelCatalogFlights.Lock()
	flight := modelCatalogFlights.entries[key]
	if flight == nil {
		flight = &modelCatalogFlight{}
		modelCatalogFlights.entries[key] = flight
	}
	flight.refs++
	modelCatalogFlights.Unlock()
	flight.mu.Lock()
	return func() {
		flight.mu.Unlock()
		modelCatalogFlights.Lock()
		flight.refs--
		if flight.refs == 0 && modelCatalogFlights.entries[key] == flight {
			delete(modelCatalogFlights.entries, key)
		}
		modelCatalogFlights.Unlock()
	}
}

// discoverAgentModelCatalog is the only live discovery routine used by CPA's
// registration and executor paths. It deliberately contains no benefit request
// and therefore cannot leave an optional detached request behind.
func discoverAgentModelCatalog(cfg *Config, cred *credential, callbackID string) modelCatalogResult {
	key := modelCatalogCacheKey(cfg, cred, "agent")
	if cached, ok := cachedModelCatalog(key); ok {
		return cached
	}
	release := acquireModelCatalogFlight(key)
	defer release()
	if cached, ok := cachedModelCatalog(key); ok {
		return cached
	}
	result := modelCatalogResult{Models: []ModelConfig{}, Source: "unavailable", FetchedAt: time.Now().UTC()}
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
	// Merged last: appendModels keeps the first entry per id, so ids the agent
	// catalogue also reports keep the Agent route, and the built-in list only
	// contributes models Agent Center did not report.
	if cfg.APIMode != "native" && cfg.DiscoverBuiltinModels {
		builtin, errBuiltin := discoverBuiltinModels(cfg, cred, callbackID)
		if errBuiltin != nil {
			result.Warnings = append(result.Warnings, "Builtin model catalog: "+errBuiltin.Error())
		}
		appendModels(builtin)
	}
	if len(result.Models) == 0 {
		result.Warnings = append(result.Warnings, "No enabled models were returned for this account")
		storeModelCatalog(key, result)
		return result
	}
	result.Source = "discovered"
	storeModelCatalog(key, result)
	return result
}

// discoverModelCatalog is the explicit full refresh used by management. Agent
// models are obtained and cached first; the benefit request then runs
// synchronously under the management callback context so cancellation reaches
// host.http.do and no goroutine survives the call.
func discoverModelCatalog(cfg *Config, cred *credential, callbackID string) modelCatalogResult {
	key := modelCatalogCacheKey(cfg, cred, "full")
	if cached, ok := cachedModelCatalog(key); ok {
		return cached
	}
	release := acquireModelCatalogFlight(key)
	defer release()
	if cached, ok := cachedModelCatalog(key); ok {
		return cached
	}
	result := discoverAgentModelCatalog(cfg, cred, callbackID)
	if cfg.APIMode != "native" && cfg.BenefitGatewayURL != "" {
		models, err := discoverBenefitModels(cfg, cred, callbackID)
		if err != nil {
			result.Warnings = append(result.Warnings, "Benefit model catalog: "+err.Error())
		} else {
			seen := make(map[string]bool, len(result.Models))
			for _, model := range result.Models {
				seen[model.ID] = true
			}
			for _, model := range models {
				if !seen[model.ID] {
					seen[model.ID] = true
					result.Models = append(result.Models, model)
				}
			}
		}
	}
	if len(result.Models) == 0 {
		result.Source = "unavailable"
		if len(result.Warnings) == 0 {
			result.Warnings = append(result.Warnings, "No enabled models were returned for this account")
		}
		storeModelCatalog(key, result)
		return result
	}
	result.Source = "discovered"
	storeModelCatalog(key, result)
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
