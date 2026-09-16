package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// managementBasePath must match the prefix the host reserves for plugin
// Management API routes. Paths returned from management.register are joined onto
// this prefix, so they are declared relative to it.
const managementBasePath = "/v0/management"

// managementRoutes returns the plugin-owned Management API surface.
//
// Routes under /v0/management/ are admin-key authenticated and may therefore
// touch credentials. The browser panel itself is served from the
// unauthenticated resource namespace and calls these routes with the admin key
// the operator has already stored in the management UI.
func managementRoutes() []map[string]any {
	return []map[string]any{
		{
			"Method":      http.MethodGet,
			"Path":        "/codearts-provider/accounts",
			"Description": "List CodeArts Doer accounts with subscription, quota and credential expiry.",
		},
		{
			"Method":      http.MethodPost,
			"Path":        "/codearts-provider/quota/refresh",
			"Description": "Refresh the cached quota snapshot for one or all accounts (body: {auth_index}).",
		},
		{
			"Method":      http.MethodGet,
			"Path":        "/codearts-provider/quota",
			"Description": "Return the normalised quota/subscription view for one or all accounts.",
		},
		{
			"Method":      http.MethodGet,
			"Path":        "/codearts-provider/schedule",
			"Description": "List configured cron tasks with next/last run state.",
		},
		{
			"Method":      http.MethodPost,
			"Path":        "/codearts-provider/schedule/run",
			"Description": "Run one scheduled task immediately (body: {task}).",
		},
		{
			"Method":      http.MethodPost,
			"Path":        "/codearts-provider/checkin",
			"Description": "Claim the daily benefit now (runs the checkin task). Optional body: {task} to pick a specific checkin task.",
		},
		{
			"Method":      http.MethodGet,
			"Path":        "/codearts-provider/usage",
			"Description": "Token usage rollup from host usage records: totals, per-model, per-account and recent requests.",
		},
		{
			"Method":      http.MethodGet,
			"Path":        "/codearts-provider/benefits",
			"Description": "Explain the daily benefit/check-in situation: whether it is configured, the claim URL, and the last outcome.",
		},
		{
			"Method":      http.MethodPost,
			"Path":        "/codearts-provider/schedule/config",
			"Description": "Enable or disable the scheduler and toggle individual tasks (body: {enabled, tasks:[{id,enabled}]}).",
		},
		{
			"Method":      http.MethodPost,
			"Path":        "/codearts-provider/import",
			"Description": "Import a CodeArts Doer credential JSON (body: {access_key_id, secret_access_key, security_token, domain_id, user_name, name}).",
		},
		{
			"Method":      http.MethodGet,
			"Path":        "/codearts-provider/export",
			"Description": "Export all CodeArts Doer credentials as a re-importable JSON document.",
		},
		{
			"Method":      http.MethodPost,
			"Path":        "/codearts-provider/delete",
			"Description": "Delete one CodeArts Doer account auth file (body: {auth_index}).",
		},
	}
}

// managementRegistration declares the plugin's routes and browser resources.
//
// The result is returned as an explicit lowercase map because the host decodes
// it into its own rpcManagementRegistrationResponse, whose fields are tagged
// `routes` and `resources`. Marshalling pluginapi.ManagementRegistrationResponse
// directly would emit "Routes"/"Resources" and be silently ignored.
func managementRegistration() map[string]any {
	return map[string]any{
		"routes": managementRoutes(),
		"resources": []map[string]any{
			{
				"Path":        "/panel",
				"Menu":        "CodeArts",
				"Description": "CodeArts Doer dashboard: subscription, quota, accounts and scheduled tasks.",
			},
			{
				"Path":        "/status",
				"Menu":        "",
				"Description": "Machine-readable provider status as JSON.",
			},
		},
	}
}

// managementHandle dispatches one management or resource request.
//
// req.Path is the full path the host received, so routes are matched on their
// suffix after the plugin's own namespace.
func managementHandle(request []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, fmt.Errorf("decode management request: %w", errUnmarshal)
		}
	}
	route := managementRouteSuffix(req.Path)
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}

	switch {
	case route == "/" || route == "/status":
		return okEnvelope(statusPage(config()))
	case route == "/panel":
		return okEnvelope(htmlResponse(panelHTML()))
	case route == "/accounts" && method == http.MethodGet:
		return okEnvelope(handleAccounts())
	case route == "/quota" && method == http.MethodGet:
		return okEnvelope(handleQuotaGet(req.Query))
	case route == "/quota/refresh" && method == http.MethodPost:
		return okEnvelope(handleQuotaRefresh(req))
	case route == "/schedule" && method == http.MethodGet:
		return okEnvelope(handleScheduleGet())
	case route == "/schedule/run" && method == http.MethodPost:
		return okEnvelope(handleScheduleRun(req))
	case route == "/checkin" && method == http.MethodPost:
		return okEnvelope(handleCheckin(req))
	case route == "/benefits" && method == http.MethodGet:
		return okEnvelope(handleBenefits())
	case route == "/usage" && method == http.MethodGet:
		return okEnvelope(handleUsage())
	case route == "/schedule/config" && method == http.MethodPost:
		return okEnvelope(handleScheduleConfig(req))
	case route == "/import" && method == http.MethodPost:
		return okEnvelope(handleImport(req))
	case route == "/export" && method == http.MethodGet:
		return okEnvelope(handleExport())
	case route == "/delete" && method == http.MethodPost:
		return okEnvelope(handleDelete(req))
	}

	body, _ := json.Marshal(map[string]any{
		"error":  "unknown management path",
		"path":   req.Path,
		"method": method,
	})
	return okEnvelope(jsonResponse(http.StatusNotFound, body))
}

// managementRouteSuffix strips the management or resource prefix and the plugin
// namespace, yielding a path like "/quota" or "/panel".
//
// Both namespaces are matched against the full path because the host passes the
// path it received in full: authenticated routes arrive as
// /v0/management/<pluginID>/..., browser resources as
// /v0/resource/plugins/<pluginID>/....
func managementRouteSuffix(path string) string {
	trimmed := strings.TrimSpace(path)
	for _, prefix := range []string{
		managementBasePath + "/" + providerID,
		"/v0/resource/plugins/" + providerID,
	} {
		if strings.HasPrefix(trimmed, prefix) {
			trimmed = strings.TrimPrefix(trimmed, prefix)
			break
		}
	}
	if trimmed == "" {
		return "/"
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	return strings.TrimRight(trimmed, "/")
}

// ---------------------------------------------------------------------------
// account + quota handlers
// ---------------------------------------------------------------------------

// accountView is one account row for the panel and the accounts route.
type accountView struct {
	AuthIndex string `json:"auth_index"`
	AuthID    string `json:"auth_id"`
	Name      string `json:"name"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	Disabled  bool   `json:"disabled"`
	UserName  string `json:"user_name"`
	UserID    string `json:"user_id"`
	DomainID  string `json:"domain_id"`
	LoginType string `json:"login_type"`
	ExpiresAt string `json:"expires_at"`

	Plan                   string  `json:"plan,omitempty"`
	PlanName               string  `json:"plan_name,omitempty"`
	PlanURL                string  `json:"plan_url,omitempty"`
	ResetDate              string  `json:"reset_date,omitempty"`
	CodeCompletionsPercent float64 `json:"code_completions_percent"`
	ChatMessagesPercent    float64 `json:"chat_messages_percent"`
	Features               any     `json:"features,omitempty"`
	QuotaFetchedAt         string  `json:"quota_fetched_at,omitempty"`
	QuotaError             string  `json:"quota_error,omitempty"`

	LastRefresh string `json:"last_refresh,omitempty"`
	NextRefresh string `json:"next_refresh,omitempty"`
}

// codeartsAccounts reads every CodeArts Doer credential from the host store and
// merges the cached quota snapshot.
func codeartsAccounts() ([]accountView, error) {
	files, errList := hostAuthList()
	if errList != nil {
		return nil, fmt.Errorf("list auth files: %w", errList)
	}
	cached := quotas.all()

	views := make([]accountView, 0, len(files))
	for _, file := range files {
		if normalizeProvider(file.Provider) != providerID && normalizeProvider(file.Type) != providerID {
			continue
		}
		view := accountView{
			AuthIndex: file.AuthIndex,
			AuthID:    file.ID,
			Name:      file.Name,
			Label:     firstNonEmptyString(file.Label, file.Account, file.Email),
			Status:    file.Status,
			Disabled:  file.Disabled,
		}
		if !file.LastRefresh.IsZero() {
			view.LastRefresh = file.LastRefresh.Format(time.RFC3339)
		}
		if !file.NextRetryAfter.IsZero() {
			view.NextRefresh = file.NextRetryAfter.Format(time.RFC3339)
		}

		// Credential identity comes from the plugin-owned storage blob.
		if storage, errGet := hostAuthGet(file.AuthIndex); errGet == nil {
			if cred, errCred := credentialFromStorage(storage); errCred == nil && cred != nil {
				view.UserName = cred.UserName
				view.UserID = cred.UserID
				view.DomainID = cred.DomainID
				view.LoginType = cred.LoginType
				view.ExpiresAt = cred.ExpiresAt
			}
		}

		if snapshot, ok := cached[file.AuthIndex]; ok {
			view.Plan = snapshot.Plan
			view.PlanName = snapshot.PlanName
			view.PlanURL = snapshot.PlanURL
			view.ResetDate = snapshot.ResetDate
			view.CodeCompletionsPercent = snapshot.CodeCompletionsPercent
			view.ChatMessagesPercent = snapshot.ChatMessagesPercent
			view.Features = snapshot.Features
			view.QuotaError = snapshot.Error
			if !snapshot.FetchedAt.IsZero() {
				view.QuotaFetchedAt = snapshot.FetchedAt.Format(time.RFC3339)
			}
		}
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].AuthIndex < views[j].AuthIndex })
	return views, nil
}

func handleAccounts() pluginapi.ManagementResponse {
	views, errAccounts := codeartsAccounts()
	if errAccounts != nil {
		return errorJSON(http.StatusBadGateway, errAccounts.Error())
	}
	body, _ := json.Marshal(map[string]any{
		"provider": providerID,
		"count":    len(views),
		"accounts": views,
	})
	return jsonResponse(http.StatusOK, body)
}

func handleQuotaGet(query map[string][]string) pluginapi.ManagementResponse {
	authIndex := firstQueryValue(query, "auth_index")
	if authIndex != "" {
		views, errAccounts := codeartsAccounts()
		if errAccounts != nil {
			return errorJSON(http.StatusBadGateway, errAccounts.Error())
		}
		for _, view := range views {
			if view.AuthIndex == authIndex {
				body, _ := json.Marshal(view)
				return jsonResponse(http.StatusOK, body)
			}
		}
		return errorJSON(http.StatusNotFound, "unknown auth_index")
	}
	cached := quotas.all()
	body, _ := json.Marshal(map[string]any{
		"provider":  providerID,
		"count":     len(cached),
		"snapshots": cached,
	})
	return jsonResponse(http.StatusOK, body)
}

// handleQuotaRefresh refreshes one or all accounts, forcing an upstream read.
func handleQuotaRefresh(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	authIndex := ""
	if len(req.Body) > 0 {
		var body struct {
			AuthIndex string `json:"auth_index"`
		}
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal == nil {
			authIndex = strings.TrimSpace(body.AuthIndex)
		}
	}
	if authIndex == "" {
		authIndex = firstQueryValue(req.Query, "auth_index")
	}

	files, errList := hostAuthList()
	if errList != nil {
		return errorJSON(http.StatusBadGateway, errList.Error())
	}
	refreshed, failed := 0, 0
	var errorsSeen []string
	for _, file := range files {
		if normalizeProvider(file.Provider) != providerID && normalizeProvider(file.Type) != providerID {
			continue
		}
		if authIndex != "" && file.AuthIndex != authIndex {
			continue
		}
		storage, errGet := hostAuthGet(file.AuthIndex)
		if errGet != nil {
			failed++
			errorsSeen = append(errorsSeen, file.AuthIndex+": "+errGet.Error())
			continue
		}
		cred, errCred := credentialFromStorage(storage)
		if errCred != nil || !cred.valid() {
			failed++
			errorsSeen = append(errorsSeen, file.AuthIndex+": credential is incomplete")
			continue
		}
		if _, errFetch := fetchQuotaSnapshot(file.AuthIndex, cred); errFetch != nil {
			failed++
			errorsSeen = append(errorsSeen, file.AuthIndex+": "+errFetch.Error())
			continue
		}
		refreshed++
	}
	body, _ := json.Marshal(map[string]any{
		"success":   failed == 0,
		"refreshed": refreshed,
		"failed":    failed,
		"errors":    errorsSeen,
	})
	return jsonResponse(http.StatusOK, body)
}

// ---------------------------------------------------------------------------
// schedule handlers
// ---------------------------------------------------------------------------

func handleScheduleGet() pluginapi.ManagementResponse {
	cfg := config()
	body, _ := json.Marshal(map[string]any{
		"enabled":  cfg.Schedule.Enabled,
		"timezone": firstNonEmptyString(cfg.Schedule.Timezone, time.Local.String()),
		"tasks":    describeTasks(cfg),
	})
	return jsonResponse(http.StatusOK, body)
}

func handleScheduleRun(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		Task string `json:"task"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	taskID := strings.TrimSpace(body.Task)
	if taskID == "" {
		taskID = firstQueryValue(req.Query, "task")
	}
	if taskID == "" {
		return errorJSON(http.StatusBadRequest, "task is required")
	}
	if errTrigger := triggerTask(taskID); errTrigger != nil {
		return errorJSON(http.StatusNotFound, errTrigger.Error())
	}
	out, _ := json.Marshal(map[string]any{
		"success": true,
		"task":    taskID,
		"note":    "the task was started in the background; poll /schedule for the outcome",
	})
	return jsonResponse(http.StatusOK, out)
}

// handleScheduleConfig toggles the scheduler and individual tasks at runtime.
//
// The change is applied in memory and by restarting the cron runner. It is not
// written back to config.yaml: the host owns that file, and silently rewriting
// operator configuration is not this plugin's business. The response says so.
func handleScheduleConfig(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		Enabled *bool `json:"enabled"`
		Tasks   []struct {
			ID      string `json:"id"`
			Enabled *bool  `json:"enabled"`
		} `json:"tasks"`
	}
	if len(req.Body) > 0 {
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			return errorJSON(http.StatusBadRequest, "invalid body: "+errUnmarshal.Error())
		}
	}

	cfg := config()
	// Work on a copy so a partial failure cannot leave a half-mutated config.
	updated := *cfg
	updated.Schedule = cfg.Schedule
	updated.Schedule.Tasks = append([]ScheduleTask(nil), cfg.Schedule.Tasks...)

	if body.Enabled != nil {
		updated.Schedule.Enabled = *body.Enabled
	}
	for _, change := range body.Tasks {
		id := strings.TrimSpace(change.ID)
		if id == "" {
			continue
		}
		found := false
		for index := range updated.Schedule.Tasks {
			if updated.Schedule.Tasks[index].ID != id {
				continue
			}
			found = true
			if change.Enabled != nil {
				value := *change.Enabled
				updated.Schedule.Tasks[index].Enabled = &value
			}
			break
		}
		if !found {
			return errorJSON(http.StatusNotFound, "unknown task "+id)
		}
	}

	currentConfig.Store(&updated)
	startScheduler(&updated)

	out, _ := json.Marshal(map[string]any{
		"success":    true,
		"enabled":    updated.Schedule.Enabled,
		"tasks":      describeTasks(&updated),
		"persistent": false,
		"note": "runtime change only; edit plugins.configs." + providerID +
			".schedule in config.yaml to persist it",
	})
	return jsonResponse(http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// credential import / export / delete
// ---------------------------------------------------------------------------

// handleImport accepts a credential JSON and writes it to the host auth store.
func handleImport(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		SecurityToken   string `json:"security_token"`
		DomainID        string `json:"domain_id"`
		UserName        string `json:"user_name"`
		UserID          string `json:"user_id"`
		ExpiresAt       string `json:"expires_at"`
		LoginType       string `json:"login_type"`
		Name            string `json:"name"`
	}
	if len(req.Body) == 0 {
		return errorJSON(http.StatusBadRequest, "request body is required")
	}
	if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
		return errorJSON(http.StatusBadRequest, "invalid body: "+errUnmarshal.Error())
	}
	cred := credential{
		AccessKeyID:     strings.TrimSpace(body.AccessKeyID),
		SecretAccessKey: strings.TrimSpace(body.SecretAccessKey),
		SecurityToken:   strings.TrimSpace(body.SecurityToken),
		DomainID:        strings.TrimSpace(body.DomainID),
		UserName:        strings.TrimSpace(body.UserName),
		UserID:          strings.TrimSpace(body.UserID),
		ExpiresAt:       strings.TrimSpace(body.ExpiresAt),
		LoginType:       firstNonEmptyString(strings.TrimSpace(body.LoginType), "AKSK"),
	}
	if !cred.valid() {
		return errorJSON(http.StatusBadRequest, "access_key_id and secret_access_key are both required")
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		label := firstNonEmptyString(cred.UserName, cred.UserID, cred.AccessKeyID)
		name = providerLoginFile(label)
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		name += ".json"
	}

	payload, errBuild := buildAuthFileDocument(cred)
	if errBuild != nil {
		return errorJSON(http.StatusInternalServerError, errBuild.Error())
	}
	if errSave := hostAuthSave(name, payload); errSave != nil {
		return errorJSON(http.StatusBadGateway, "host.auth.save: "+errSave.Error())
	}
	logInfo("imported credential through the management API", map[string]any{"file": name})

	out, _ := json.Marshal(map[string]any{
		"success":    true,
		"name":       name,
		"user":       firstNonEmptyString(cred.UserName, cred.UserID),
		"login_type": cred.LoginType,
	})
	return jsonResponse(http.StatusOK, out)
}

// buildAuthFileDocument renders the auth file the host persists. The credential
// lives under the plugin-owned key so host-managed keys survive a round trip.
func buildAuthFileDocument(cred credential) ([]byte, error) {
	doc := map[string]any{
		"type":      providerID,
		storageKey:  cred,
		"user_name": cred.UserName,
		"user_id":   cred.UserID,
	}
	if cred.DomainID != "" {
		doc["domain_id"] = cred.DomainID
	}
	if cred.LoginType != "" {
		doc["login_type"] = cred.LoginType
	}
	if cred.ExpiresAt != "" {
		doc["expires_at"] = cred.ExpiresAt
	}
	return json.MarshalIndent(doc, "", "  ")
}

// handleExport dumps every credential in a re-importable form. The response is
// served from an admin-authenticated route, so returning key material is
// acceptable here; the unauthenticated panel never calls it without the key.
func handleExport() pluginapi.ManagementResponse {
	views, errAccounts := codeartsAccounts()
	if errAccounts != nil {
		return errorJSON(http.StatusBadGateway, errAccounts.Error())
	}
	type exported struct {
		Name string     `json:"name"`
		Cred credential `json:"credential"`
	}
	items := make([]exported, 0, len(views))
	for _, view := range views {
		storage, errGet := hostAuthGet(view.AuthIndex)
		if errGet != nil {
			continue
		}
		cred, errCred := credentialFromStorage(storage)
		if errCred != nil || cred == nil || !cred.valid() {
			continue
		}
		items = append(items, exported{Name: view.Name, Cred: *cred})
	}
	body, _ := json.Marshal(map[string]any{
		"provider":    providerID,
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"count":       len(items),
		"accounts":    items,
		"warning":     "this document contains live credentials; store it accordingly",
	})
	return jsonResponse(http.StatusOK, body)
}

// handleDelete removes one credential from the host store.
//
// The strict ownership check matters: deleting an auth file is destructive, so
// the target must be a credential this plugin owns, identified by auth_index.
func handleDelete(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		authIndex = firstQueryValue(req.Query, "auth_index")
	}
	if authIndex == "" {
		return errorJSON(http.StatusBadRequest, "auth_index is required")
	}

	files, errList := hostAuthList()
	if errList != nil {
		return errorJSON(http.StatusBadGateway, errList.Error())
	}
	for _, file := range files {
		if file.AuthIndex != authIndex {
			continue
		}
		// Refuse to delete anything that is not a CodeArts Doer credential.
		if normalizeProvider(file.Provider) != providerID && normalizeProvider(file.Type) != providerID {
			return errorJSON(http.StatusForbidden, "auth_index does not belong to the "+providerID+" provider")
		}
		path := strings.TrimSpace(file.Path)
		if path == "" {
			return errorJSON(http.StatusConflict, "credential has no backing auth file to delete")
		}
		// The plugin ABI has no host.auth.delete, so the file is removed
		// directly. That is destructive, so the path is constrained to a plain
		// .json file inside the auth directory before anything is touched.
		if errDelete := deleteAuthFileSafely(path); errDelete != nil {
			return errorJSON(http.StatusBadGateway, errDelete.Error())
		}
		quotas.forget(authIndex)
		logInfo("deleted credential through the management API", map[string]any{
			"account": authIndex,
			"file":    file.Name,
		})
		out, _ := json.Marshal(map[string]any{"success": true, "auth_index": authIndex, "name": file.Name})
		return jsonResponse(http.StatusOK, out)
	}
	return errorJSON(http.StatusNotFound, "unknown auth_index")
}

// handleUsage serves the token usage rollup collected from host usage records.
func handleUsage() pluginapi.ManagementResponse {
	body, errMarshal := json.Marshal(usage.snapshot())
	if errMarshal != nil {
		return errorJSON(http.StatusInternalServerError, errMarshal.Error())
	}
	return jsonResponse(http.StatusOK, body)
}

// ---------------------------------------------------------------------------
// daily benefit / check-in handlers
// ---------------------------------------------------------------------------

// handleCheckin claims the daily benefit by running a checkin task now.
//
// There is no upstream API documented for this and the extension performs no
// such call itself (it opens a server-hosted activity page), so the claim is
// driven entirely by the configured checkin task. When none is configured the
// response explains exactly how to enable it rather than failing silently.
func handleCheckin(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body struct {
		Task string `json:"task"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	taskID := strings.TrimSpace(body.Task)
	if taskID == "" {
		taskID = firstQueryValue(req.Query, "task")
	}

	cfg := config()
	var candidates []ScheduleTask
	for _, task := range cfg.scheduleTasks() {
		if task.Type == TaskCheckin {
			candidates = append(candidates, task)
		}
	}
	if len(candidates) == 0 {
		out, _ := json.Marshal(map[string]any{
			"success":    false,
			"configured": false,
			"error":      "no checkin task is configured",
			"how_to_enable": []string{
				"The CodeArts Doer extension implements no check-in API; the daily benefit page (the \"Wish Wall\") is a server-hosted web app.",
				"Open the activity page in your browser, use the browser devtools Network tab, click the daily claim, and copy the request URL, method, body and headers.",
				"Add a schedule task of type \"checkin\" with checkin_url (and checkin_body/checkin_headers if the request needs them).",
				"Then call this route again, or let cron claim it automatically.",
			},
		})
		return jsonResponse(http.StatusOK, out)
	}

	selected := candidates[0]
	if taskID != "" {
		found := false
		for _, task := range candidates {
			if task.ID == taskID {
				selected, found = task, true
				break
			}
		}
		if !found {
			return errorJSON(http.StatusNotFound, "unknown checkin task "+taskID)
		}
	}
	if errTrigger := triggerTask(selected.ID); errTrigger != nil {
		return errorJSON(http.StatusNotFound, errTrigger.Error())
	}
	out, _ := json.Marshal(map[string]any{
		"success":    true,
		"configured": true,
		"task":       selected.ID,
		"note":       "claim started in the background; GET /benefits for the outcome",
	})
	return jsonResponse(http.StatusOK, out)
}

// handleBenefits explains the daily check-in situation for the panel.
func handleBenefits() pluginapi.ManagementResponse {
	cfg := config()
	states := scheduler.snapshot()

	type taskView struct {
		ID         string `json:"id"`
		Cron       string `json:"cron"`
		Enabled    bool   `json:"enabled"`
		URL        string `json:"checkin_url,omitempty"`
		Method     string `json:"checkin_method,omitempty"`
		NextRun    string `json:"next_run,omitempty"`
		LastRun    string `json:"last_run,omitempty"`
		LastResult string `json:"last_result,omitempty"`
		LastError  string `json:"last_error,omitempty"`
	}
	next := scheduler.nextRuns()
	var tasks []taskView
	for _, task := range cfg.scheduleTasks() {
		if task.Type != TaskCheckin {
			continue
		}
		view := taskView{
			ID:      task.ID,
			Cron:    task.Cron,
			Enabled: task.isEnabled(),
			URL:     task.CheckinURL,
			Method:  task.CheckinMethod,
		}
		if value, ok := next[task.ID]; ok {
			view.NextRun = value
		}
		if state, ok := states[task.ID]; ok {
			if !state.LastRunAt.IsZero() {
				view.LastRun = state.LastRunAt.Format(time.RFC3339)
			}
			view.LastResult = state.LastResult
			view.LastError = state.LastError
		}
		tasks = append(tasks, view)
	}

	scheduleEnabled := cfg.Schedule.Enabled
	out, _ := json.Marshal(map[string]any{
		"configured":       len(tasks) > 0,
		"schedule_enabled": scheduleEnabled,
		"tasks":            tasks,
		"explanation": "The CodeArts Doer extension contains no check-in call: the daily benefit page " +
			"(the \"Wish Wall\") is a server-hosted web app opened from the extension's wishWallUrl, " +
			"and the claim is performed inside that page. This plugin schedules the claim for you once you " +
			"supply the request, captured from that page.",
		"how_to_capture": []string{
			"Open the daily benefit page in a browser and sign in.",
			"Open devtools -> Network, clear it, then click the daily claim button.",
			"Copy the request as cURL: URL, method, body and any custom headers.",
			"Put them into a checkin task (checkin_url / checkin_method / checkin_body / checkin_headers).",
			"Set checkin_success_marker to text that appears only on a real claim, so a no-op is not reported as success.",
		},
	})
	return jsonResponse(http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// response helpers
// ---------------------------------------------------------------------------

func jsonResponse(status int, body []byte) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}

func htmlResponse(html string) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       []byte(html),
	}
}

func errorJSON(status int, message string) pluginapi.ManagementResponse {
	body, _ := json.Marshal(map[string]any{"success": false, "error": message})
	return jsonResponse(status, body)
}

func firstQueryValue(query map[string][]string, key string) string {
	values, ok := query[key]
	if !ok || len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

// statusPage renders the machine-readable provider status. It deliberately
// contains no credential material because this route also backs the
// unauthenticated browser resource.
func statusPage(cfg *Config) pluginapi.ManagementResponse {
	type modelView struct {
		ID            string `json:"id"`
		DisplayName   string `json:"display_name"`
		UpstreamModel string `json:"upstream_model"`
	}
	models := make([]modelView, 0, len(cfg.Models))
	for _, model := range cfg.Models {
		models = append(models, modelView{
			ID:            model.ID,
			DisplayName:   model.DisplayName,
			UpstreamModel: cfg.upstreamModel(model.ID),
		})
	}

	payload := map[string]any{
		"plugin": map[string]any{
			"id":      providerID,
			"name":    "CodeArts",
			"version": pluginVersion,
		},
		"endpoint": map[string]any{
			"base_url":          cfg.BaseURL,
			"protocol_mode":     cfg.APIMode,
			"chat_path":         chatPathFor(cfg),
			"quota_path":        "/snap-manager/v1/statistics/plugin",
			"web_login_base":    cfg.WebLoginBase,
			"agent_id":          cfg.AgentID,
			"default_model_id":  cfg.DefaultModelID,
			"plugin_name":       cfg.PluginName,
			"plugin_version":    cfg.PluginVersion,
			"language":          cfg.Language,
			"heartbeat":         cfg.Heartbeat,
			"request_timeout_s": cfg.RequestTimeoutSeconds,
		},
		"models": models,
		"schedule": map[string]any{
			"enabled":  cfg.Schedule.Enabled,
			"timezone": firstNonEmptyString(cfg.Schedule.Timezone, time.Local.String()),
			"tasks":    describeTasks(cfg),
		},
		// Derived from the same source as plugin.register so the status page can
		// never advertise a different capability set than the plugin declares.
		"capabilities": declaredCapabilities(),
		"notes": []string{
			"Daily check-in is driven by the configurable checkin task: the claim UI is a server-hosted page and the extension performs no claim request.",
			"Recurring work is driven by the plugin's own cron scheduler because the plugin ABI provides no timer.",
			"Resource pages are not admin-authenticated, so no credential material is shown here.",
			"Sign in with GET /v0/management/" + providerID + "-auth-url (admin key required) and open the returned url.",
		},
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	}

	body, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		body = []byte(`{"error":"failed to encode status"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}

func chatPathFor(cfg *Config) string {
	if cfg.APIMode == "native" {
		return nativeChatPath
	}
	return agentModePath
}
