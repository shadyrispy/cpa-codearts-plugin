package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// storageKey is the field name under which the credential is persisted inside
// the auth record's StorageJSON. The host round-trips this opaque blob back to
// the plugin on every executor call.
const storageKey = "codearts_provider_credential"

// loginSession tracks one interactive browser login flow. The official IDE
// extension starts a loopback HTTP server, opens the CodeArts web console with
// a ticket id and a callback URL, then polls the ticket endpoint with the
// secret the console echoes back to that callback. The plugin reproduces that
// flow, including the extension's tolerance for a callback that carries no
// secret: the console binds the secret to the ticket, and the ticket exchange is
// what actually proves the login.
type loginSession struct {
	state    string
	ticketID string
	secret   string

	mu       sync.Mutex
	listener net.Listener
	server   *http.Server
	received bool
	expires  time.Time
	cancel   context.CancelFunc

	// callbackURL is the URL handed to the CodeArts console; bindAddr is the
	// local socket it is served from. They differ when the deployment advertises
	// a different host (login_callback_base) or binds a non-loopback interface.
	callbackURL string
	bindAddr    string

	// progress is the observable state of the flow, so a page can always tell
	// whether it is waiting for the browser or for the ticket exchange.
	callbackAt   time.Time
	attempts     int
	lastStatus   int
	lastMessage  string
	closedListen bool

	credential *credential
	err        string
}

var (
	loginMu       sync.Mutex
	loginSessions = map[string]*loginSession{}
)

// authParse recognises an auth JSON file for this provider and maps it onto a
// CLIProxyAPI auth record. Files are matched on the `type` field so users can
// drop a credential file into the auth directory:
//
//	{"type":"codearts-provider","access_key_id":"...","secret_access_key":"...",
//	 "security_token":"...","domain_id":"..."}
func authParse(request []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode auth parse request: %w", errUnmarshal)
	}
	if normalizeProvider(req.Provider) != providerID {
		// Not ours: report unhandled so other parsers can try.
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	parsed, errCredential := credentialFromStorage(req.RawJSON)
	if errCredential != nil || !parsed.valid() {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	cred := *parsed
	if !cred.valid() {
		logWarn("auth file is missing an access key or secret key", map[string]any{"file": req.FileName})
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}

	storage, errMarshal := json.Marshal(map[string]any{storageKey: cred})
	if errMarshal != nil {
		return nil, errMarshal
	}
	label := cred.UserName
	if label == "" {
		label = cred.UserID
	}
	if label == "" {
		label = req.FileName
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth: pluginapi.AuthData{
			Provider:    providerID,
			FileName:    req.FileName,
			Label:       label,
			StorageJSON: storage,
			Metadata: map[string]any{
				"type":       providerID,
				"user_name":  cred.UserName,
				"user_id":    cred.UserID,
				"domain_id":  cred.DomainID,
				"login_type": cred.LoginType,
			},
			NextRefreshAfter: refreshDeadline(&cred),
		},
	})
}

// authLoginStart begins the interactive browser login. It binds a loopback
// listener, builds the CodeArts web console redirect URL carrying the ticket id
// and the callback URL, and returns the URL for the user to open.
func authLoginStart(request []byte) ([]byte, error) {
	var req pluginapi.AuthLoginStartRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode login start request: %w", errUnmarshal)
	}
	cfg := config()

	bindAddr := fmt.Sprintf("%s:%d", cfg.LoginCallbackBind, cfg.LoginCallbackPort)
	listener, errListen := net.Listen("tcp", bindAddr)
	if errListen != nil && cfg.LoginCallbackPort != 0 {
		// A pinned port may already be in use; an ephemeral one still lets the
		// flow work for a browser on the same machine.
		logWarn("the configured login callback port is unavailable; using an ephemeral port", map[string]any{
			"bind":  bindAddr,
			"error": errListen.Error(),
		})
		bindAddr = fmt.Sprintf("%s:0", cfg.LoginCallbackBind)
		listener, errListen = net.Listen("tcp", bindAddr)
	}
	if errListen != nil {
		return failEnvelope("login_unavailable", "failed to bind the browser callback listener on "+bindAddr+": "+errListen.Error(), http.StatusInternalServerError)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d/authentication", port)
	if cfg.LoginCallbackBase != "" {
		// An advertised base is what makes a containerised gateway reachable:
		// the browser is sent to the published address instead of the loopback.
		callbackURL = cfg.LoginCallbackBase + "/authentication"
	}

	ticketID := randomUUIDv4()
	state := randomHex(16)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.loginTimeout())

	session := &loginSession{
		state:       state,
		ticketID:    ticketID,
		cancel:      cancel,
		listener:    listener,
		callbackURL: callbackURL,
		bindAddr:    listener.Addr().String(),
		expires:     time.Now().Add(cfg.loginTimeout()),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/authentication", session.handleCallback)
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	session.mu.Lock()
	session.server = server
	session.mu.Unlock()

	go func() {
		if errServe := server.Serve(listener); errServe != nil && errServe != http.ErrServerClosed {
			logWarn("login callback listener stopped", map[string]any{"error": errServe.Error()})
		}
	}()

	loginMu.Lock()
	loginSessions[state] = session
	evictLoginSessionsLocked()
	loginMu.Unlock()

	// Poll the ticket endpoint in the background so the flow completes as soon
	// as the browser hands the secret back to our loopback listener.
	go session.poll(ctx)

	loginURL := buildLoginURL(cfg, ticketID, callbackURL)
	logInfo("started browser login", map[string]any{"port": port})

	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerID,
		URL:       loginURL,
		State:     state,
		ExpiresAt: session.expires,
		Metadata:  map[string]any{"callback_url": callbackURL, "ticket_id": ticketID},
	})
}

// maxLoginSessions bounds the live login flows. Each one holds a loopback
// listener, a server goroutine and a poll goroutine for the whole login timeout,
// so an unbounded table would let a caller that never finishes a login exhaust
// sockets. Callers hold loginMu.
const maxLoginSessions = 8

// evictLoginSessionsLocked drops expired sessions and, when the table is still
// full, the session closest to expiry. Evicting the oldest keeps the newest
// attempt - the one the operator just started - working.
func evictLoginSessionsLocked() {
	now := time.Now()
	for key, item := range loginSessions {
		if now.After(item.expires) {
			item.stop()
			delete(loginSessions, key)
		}
	}
	for len(loginSessions) > maxLoginSessions {
		var oldestKey string
		var oldestExpiry time.Time
		for key, item := range loginSessions {
			if oldestKey == "" || item.expires.Before(oldestExpiry) {
				oldestKey, oldestExpiry = key, item.expires
			}
		}
		if oldestKey == "" {
			return
		}
		logWarn("dropping an unfinished login flow to make room for a new one", map[string]any{"state": oldestKey})
		loginSessions[oldestKey].stop()
		delete(loginSessions, oldestKey)
	}
}

// authLoginPoll reports the current state of an interactive login flow.
func authLoginPoll(request []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode login poll request: %w", errUnmarshal)
	}
	loginMu.Lock()
	session := loginSessions[strings.TrimSpace(req.State)]
	loginMu.Unlock()
	if session == nil {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "unknown or expired login state; restart the login flow",
		})
	}
	if time.Now().After(session.expires) {
		session.stop()
		loginMu.Lock()
		delete(loginSessions, session.state)
		loginMu.Unlock()
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "the login flow expired before it completed",
		})
	}

	session.mu.Lock()
	cred := session.credential
	errMessage := session.err
	session.mu.Unlock()

	if errMessage != "" {
		session.stop()
		loginMu.Lock()
		delete(loginSessions, session.state)
		loginMu.Unlock()
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: errMessage,
		})
	}
	if cred == nil {
		// Pending, but never silent: the message says whether the plugin is still
		// waiting for the browser or already exchanging the ticket.
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: session.progress(),
		})
	}

	// A browser authorization is a WEB credential; stamping it on the stored
	// credential matches the extension's transferToUserInfo(..., "WEB") and lets
	// the account view report how the account signed in.
	stamped := *cred
	stamped.LoginType = firstNonEmptyString(stamped.LoginType, "WEB")
	storage, errMarshal := json.Marshal(map[string]any{storageKey: stamped})
	if errMarshal != nil {
		return nil, errMarshal
	}
	session.stop()
	loginMu.Lock()
	delete(loginSessions, session.state)
	loginMu.Unlock()

	label := cred.UserName
	if label == "" {
		label = providerID
	}
	logInfo("browser login completed", map[string]any{"user": label})
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "signed in to CodeArts Doer",
		Auth: pluginapi.AuthData{
			Provider:    providerID,
			Label:       label,
			FileName:    credentialFileName(&stamped),
			StorageJSON: storage,
			Metadata: map[string]any{
				"type":       providerID,
				"user_name":  stamped.UserName,
				"user_id":    stamped.UserID,
				"domain_id":  stamped.DomainID,
				"login_type": stamped.LoginType,
			},
			Attributes:       map[string]string{"provider_type": providerID},
			NextRefreshAfter: refreshDeadline(cred),
		},
	})
}

// authRefresh renews the temporary AK/SK pair through the CodeArts token renew
// endpoint, which the official extension calls every hour.
func authRefresh(request []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode auth refresh request: %w", errUnmarshal)
	}
	cred, errCred := credentialFromStorage(req.StorageJSON)
	if errCred != nil {
		return failEnvelope("invalid_credential", "stored credential could not be decoded: "+errCred.Error(), http.StatusUnauthorized)
	}
	if !cred.valid() {
		return failEnvelope("invalid_credential", "stored credential is incomplete", http.StatusUnauthorized)
	}
	cfg := config()

	// A long-lived AK/SK pair has nothing to renew: the endpoint requires a
	// security token. Hand the credential back unchanged rather than failing.
	if strings.TrimSpace(cred.SecurityToken) == "" {
		logInfo("credential has no security token; treating it as a permanent key pair", nil)
		storage, errMarshal := json.Marshal(map[string]any{storageKey: *cred})
		if errMarshal != nil {
			return nil, errMarshal
		}
		return okEnvelope(pluginapi.AuthRefreshResponse{
			Auth: pluginapi.AuthData{
				Provider:    providerID,
				Label:       cred.UserName,
				StorageJSON: storage,
				Metadata: map[string]any{
					"type":       providerID,
					"user_name":  cred.UserName,
					"user_id":    cred.UserID,
					"domain_id":  cred.DomainID,
					"login_type": firstNonEmptyString(cred.LoginType, "AKSK"),
				},
				NextRefreshAfter: refreshDeadline(cred),
			},
			NextRefreshAfter: refreshDeadline(cred),
		})
	}

	// token/renew accepts a security-token authenticated request and returns a
	// fresh credential triple.
	renewBody, errMarshal := json.Marshal(map[string]any{
		"access":           cred.AccessKeyID,
		"securitytoken":    cred.SecurityToken,
		"duration_seconds": 24 * 60 * 60,
	})
	if errMarshal != nil {
		return nil, errMarshal
	}
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/snap-manager/v1/token/renew"
	headers, errSign := signRequest(http.MethodPost, endpoint, map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json",
	}, renewBody, cred, cfg.SignHost)
	if errSign != nil {
		return failEnvelope("sign_failed", errSign.Error(), http.StatusInternalServerError)
	}

	response, errDo := hostHTTPDo(http.MethodPost, endpoint, headers, renewBody)
	if errDo != nil {
		return failEnvelope("upstream_unreachable", "token renew request failed: "+errDo.Error(), http.StatusBadGateway)
	}
	if response.StatusCode != http.StatusOK {
		return failEnvelope("refresh_rejected", fmt.Sprintf("token renew returned HTTP %d: %s", response.StatusCode, truncate(string(response.Body), 400)), httpStatusFor(response.StatusCode))
	}

	updated, errDecode := decodeCredentialResponse(response.Body)
	if errDecode != nil {
		return failEnvelope("refresh_failed", "token renew response could not be parsed: "+errDecode.Error(), http.StatusBadGateway)
	}
	// Preserve identity fields the renew endpoint does not echo back.
	if updated.DomainID == "" {
		updated.DomainID = cred.DomainID
	}
	if updated.UserName == "" {
		updated.UserName = cred.UserName
	}
	if updated.UserID == "" {
		updated.UserID = cred.UserID
	}
	updated.LoginType = firstNonEmptyString(updated.LoginType, cred.LoginType, "WEB")

	storage, errMarshal2 := json.Marshal(map[string]any{storageKey: updated})
	if errMarshal2 != nil {
		return nil, errMarshal2
	}
	logInfo("refreshed upstream credential", map[string]any{"expires_at": updated.ExpiresAt})

	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider:    providerID,
			Label:       updated.UserName,
			StorageJSON: storage,
			Metadata: map[string]any{
				"type":       providerID,
				"user_name":  updated.UserName,
				"user_id":    updated.UserID,
				"domain_id":  updated.DomainID,
				"login_type": updated.LoginType,
			},
			NextRefreshAfter: refreshDeadline(&updated),
		},
		NextRefreshAfter: refreshDeadline(&updated),
	})
}

// ---------------------------------------------------------------------------
// login session plumbing
// ---------------------------------------------------------------------------

// handleCallback receives the browser redirect from the CodeArts console.
//
// It deliberately mirrors the official extension's callback server rather than
// validating the request: that server answers every request with the same
// success body, resolves with whatever query the browser sent, and closes the
// listener afterwards. The extension then reads `secret` and falls back to an
// empty string, because the secret is bound to the ticket by the console and it
// is the ticket exchange - not this callback - that establishes the login.
// Rejecting a secret-less or non-GET callback (as an earlier version of this
// plugin did) leaves the flow waiting forever for a second callback that never
// comes, which is exactly the "sign-in hangs" report this fixes.
func (s *loginSession) handleCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Access-Control-Max-Age", "86400")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if time.Now().After(s.expires) {
		w.WriteHeader(http.StatusGone)
		return
	}

	secret := callbackSecret(r)
	s.mu.Lock()
	first := !s.received
	if first {
		s.secret = secret
		s.received = true
		s.callbackAt = time.Now()
	}
	s.mu.Unlock()

	if first {
		if secret == "" {
			logWarn("the browser callback carried no secret; exchanging the ticket anyway", map[string]any{"state": s.state})
		}
		// The extension stops accepting callbacks once it has one; a second
		// browser hit must not be able to replace the bound secret.
		s.closeListener()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"success","message":"Secret received"}`))
}

// callbackSecret extracts the secret from the callback request. The console
// redirects the browser with it as a query parameter; a JSON or form body is
// also accepted so a proxied, scripted or content-type-less callback still
// works. An empty result is not an error: the secret is optional here.
func callbackSecret(r *http.Request) string {
	if secret := strings.TrimSpace(r.URL.Query().Get("secret")); secret != "" {
		return secret
	}
	if r.Body == nil {
		return ""
	}
	raw, errRead := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if errRead != nil || len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var body struct {
		Secret string `json:"secret"`
	}
	if json.Unmarshal(raw, &body) == nil && strings.TrimSpace(body.Secret) != "" {
		return strings.TrimSpace(body.Secret)
	}
	if strings.Contains(r.Header.Get("Content-Type"), "form-urlencoded") {
		if errParse := r.ParseForm(); errParse == nil {
			if secret := strings.TrimSpace(r.PostForm.Get("secret")); secret != "" {
				return secret
			}
		}
	}
	if values, errParse := url.ParseQuery(string(raw)); errParse == nil {
		return strings.TrimSpace(values.Get("secret"))
	}
	return ""
}

// closeListener stops accepting callbacks without ending the login session: the
// ticket exchange still has to run.
func (s *loginSession) closeListener() {
	s.mu.Lock()
	already := s.closedListen
	s.closedListen = true
	server := s.server
	s.mu.Unlock()
	if already || server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

// progress renders the observable state of the flow for the panel.
func (s *loginSession) progress() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.progressLocked()
}

// progressLocked is progress for callers that already hold the session lock.
func (s *loginSession) progressLocked() string {
	switch {
	case s.credential != nil:
		return "授权成功，账号已写入 CPA。"
	case s.err != "":
		return s.err
	case !s.received:
		return "等待浏览器完成授权回调（" + s.callbackURL + "）。"
	case s.attempts == 0:
		return "已收到浏览器回调，正在换取凭证…"
	case s.lastStatus == http.StatusOK:
		return "已收到回调，凭证尚未就绪，正在重试…"
	case s.lastMessage != "":
		return fmt.Sprintf("已收到回调，换取凭证返回 HTTP %d：%s", s.lastStatus, s.lastMessage)
	default:
		return fmt.Sprintf("已收到回调，换取凭证返回 HTTP %d，正在重试…", s.lastStatus)
	}
}

// poll exchanges the ticket for a credential once the secret arrives, retrying
// until the login window closes. Every attempt is recorded so the management
// API can explain a flow that is not progressing instead of leaving it silent.
func (s *loginSession) poll(ctx context.Context) {
	defer s.stop()
	cfg := config()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			if s.credential == nil && s.err == "" {
				if !s.received {
					s.err = "登录超时：浏览器没有把授权结果回传到 " + s.callbackURL +
						"。如果 CPA 运行在容器或远程主机上，请在面板中粘贴浏览器地址栏里的回调地址。"
				} else if s.lastMessage != "" {
					s.err = fmt.Sprintf("登录超时：换取凭证一直返回 HTTP %d（%s）。", s.lastStatus, s.lastMessage)
				} else {
					s.err = "登录超时：未能换取凭证。"
				}
			}
			s.mu.Unlock()
			return
		case <-ticker.C:
		}

		s.mu.Lock()
		received := s.received
		s.mu.Unlock()
		if !received {
			continue
		}
		cred, retry, status, message, errPoll := s.exchangeTicket(cfg)
		s.mu.Lock()
		s.attempts++
		s.lastStatus = status
		if message != "" {
			s.lastMessage = message
		}
		if cred != nil {
			s.credential = cred
		} else if errPoll != nil && !retry {
			s.err = errPoll.Error()
		}
		done := s.credential != nil || s.err != ""
		s.mu.Unlock()
		if done {
			return
		}
	}
}

// exchangeTicket performs one ticket poll. retry reports whether another attempt
// is worthwhile; the status and a short body excerpt are returned for progress
// reporting even when the exchange keeps failing.
func (s *loginSession) exchangeTicket(cfg *Config) (*credential, bool, int, string, error) {
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/snap-manager/v1/login/ticket"
	parsed, errParse := url.Parse(endpoint)
	if errParse != nil {
		return nil, false, 0, "", errParse
	}
	query := parsed.Query()
	query.Set("ticket_id", s.ticketID)
	s.mu.Lock()
	query.Set("secret", s.secret)
	s.mu.Unlock()
	parsed.RawQuery = query.Encode()

	// This request is intentionally unsigned: it is exactly what the extension
	// sends (plugin identity headers and the ticket/secret pair), and the ticket
	// is the credential here, not an AK/SK pair.
	headers := map[string]string{
		"Content-Type":   "application/json;charset=UTF-8",
		"plugin-name":    cfg.PluginName,
		"plugin-version": cfg.PluginVersion,
		"Accept":         "application/json",
	}
	response, errDo := hostHTTPDo(http.MethodGet, parsed.String(), headers, nil)
	if errDo != nil {
		return nil, true, 0, truncate(errDo.Error(), 200), nil
	}
	if response.StatusCode != http.StatusOK {
		// The ticket is not ready yet; keep polling until the deadline, exactly
		// like the extension, and report the last status for the panel.
		return nil, true, response.StatusCode, truncate(string(response.Body), 300), nil
	}
	cred, errDecode := decodeCredentialResponse(response.Body)
	if errDecode != nil {
		return nil, true, response.StatusCode, truncate(errDecode.Error(), 200), nil
	}
	if !cred.valid() {
		return nil, true, response.StatusCode, "凭证字段不完整", nil
	}
	return &cred, false, response.StatusCode, "", nil
}

func (s *loginSession) stop() {
	if s == nil {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.server.Shutdown(ctx)
		cancel()
		return
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// decodeCredentialResponse extracts the credential triple from the CodeArts
// envelope. The upstream nests it under `credential`:
//
//	{"credential":{"access":"..","secret":"..","securitytoken":"..",
//	               "expires_at":".."},"domain_id":"..","user_name":".."}
func decodeCredentialResponse(body []byte) (credential, error) {
	var envelope struct {
		Credential *struct {
			Access        string `json:"access"`
			Secret        string `json:"secret"`
			SecurityToken string `json:"securitytoken"`
			ExpiresAt     string `json:"expires_at"`
		} `json:"credential"`
		DomainID  string `json:"domain_id"`
		UserName  string `json:"user_name"`
		UserID    string `json:"user_id"`
		LoginType string `json:"login_type"`
	}
	if errUnmarshal := json.Unmarshal(body, &envelope); errUnmarshal != nil {
		return credential{}, errUnmarshal
	}
	if envelope.Credential == nil {
		return credential{}, fmt.Errorf("response contains no credential object")
	}
	cred := credential{
		AccessKeyID:     strings.TrimSpace(envelope.Credential.Access),
		SecretAccessKey: strings.TrimSpace(envelope.Credential.Secret),
		SecurityToken:   strings.TrimSpace(envelope.Credential.SecurityToken),
		DomainID:        strings.TrimSpace(envelope.DomainID),
		UserName:        strings.TrimSpace(envelope.UserName),
		UserID:          strings.TrimSpace(envelope.UserID),
		ExpiresAt:       strings.TrimSpace(envelope.Credential.ExpiresAt),
		LoginType:       strings.TrimSpace(envelope.LoginType),
	}
	if !cred.valid() || cred.SecurityToken == "" {
		return credential{}, fmt.Errorf("response contains an incomplete temporary credential")
	}
	return cred, nil
}

// credentialFromStorage pulls the credential out of the opaque storage blob the
// host round-trips to the plugin.
func credentialFromStorage(storage []byte) (*credential, error) {
	if len(storage) == 0 {
		return nil, nil
	}
	// Look the field up by name rather than through a struct tag, so the key
	// exists in exactly one place (storageKey) and cannot drift from what
	// buildAuthFileDocument writes.
	var document map[string]any
	if errUnmarshal := json.Unmarshal(storage, &document); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if raw, ok := document[storageKey]; ok {
		nested, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid credential object")
		}
		document = nested
	}
	return &credential{
		AccessKeyID:     firstString(document, "access_key_id", "accessKeyId", "ak"),
		SecretAccessKey: firstString(document, "secret_access_key", "secretAccessKey", "sk"),
		SecurityToken:   firstString(document, "security_token", "securityToken", "accessToken"),
		DomainID:        firstString(document, "domain_id", "domainId", "X-Domain-Id"),
		UserName:        firstString(document, "user_name", "userName"),
		UserID:          firstString(document, "user_id", "userId"),
		ExpiresAt:       firstString(document, "expires_at", "expiresAt"),
		LoginType:       firstString(document, "login_type", "loginType"),
	}, nil
}

func stopLoginSessions() {
	loginMu.Lock()
	sessions := loginSessions
	loginSessions = map[string]*loginSession{}
	loginMu.Unlock()
	for _, session := range sessions {
		session.stop()
	}
}

// refreshDeadline returns when the host should next renew the credential. The
// extension renews hourly and the temporary credential lasts 24 hours, so an
// hour before expiry is a safe point.
//
// A credential without a security token is a permanent AK/SK pair; the renewal
// endpoint does not apply to it, so it is parked far in the future instead of
// scheduling a refresh that would fail on every attempt.
func refreshDeadline(cred *credential) time.Time {
	if cred == nil || strings.TrimSpace(cred.SecurityToken) == "" {
		return time.Now().Add(30 * 24 * time.Hour)
	}
	if cred.ExpiresAt != "" {
		if parsed, errParse := time.Parse(time.RFC3339, cred.ExpiresAt); errParse == nil {
			if target := parsed.Add(-time.Hour); target.After(time.Now()) {
				return target
			}
			return time.Now().Add(5 * time.Minute)
		}
	}
	return time.Now().Add(time.Hour)
}

// buildLoginURL mirrors the URL the official extension opens:
//
//	${codeartsWebUrl}/doer/redirect?ticket_id=..&IdeaType=vscode&auth_callback_url=..&plugin-name=..&plugin-version=..
//
// Keep the parameter order and literal callback URL byte-for-byte compatible
// with the extension. The CodeArts redirect page does not consistently treat a
// percent-encoded auth_callback_url as equivalent: in the authenticated flow it
// can reject the otherwise valid UUID with "invalid ticketId".
func buildLoginURL(cfg *Config, ticketID, callbackURL string) string {
	base := cfg.WebLoginBase
	if base == "" {
		base = cfg.IDEBaseURL
	}
	if base == "" {
		base = "https://devcloud.cn-north-4.huaweicloud.com"
	}
	base = strings.TrimRight(base, "/") + "/doer/redirect"
	return base +
		"?ticket_id=" + ticketID +
		"&IdeaType=vscode" +
		"&auth_callback_url=" + callbackURL +
		"&plugin-name=" + cfg.PluginName +
		"&plugin-version=" + cfg.PluginVersion
}

func normalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

func providerLoginFile(label string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		label = providerID
	}
	return providerID + "-" + sanitizeFileToken(label) + ".json"
}

func credentialFileName(cred *credential) string {
	identity := cred.DomainID + ":" + cred.UserID
	if cred.UserID == "" {
		identity = cred.AccessKeyID
	}
	return providerLoginFile(firstNonEmptyString(cred.UserName, "account") + "-" + sha256Hex([]byte(identity))[:12])
}

func sanitizeFileToken(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	out := strings.Trim(builder.String(), "-")
	if out == "" {
		return "account"
	}
	return out
}

func firstString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func randomHex(n int) string {
	buffer := make([]byte, n)
	if _, errRead := rand.Read(buffer); errRead != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

// randomUUIDv4 produces the dashed UUID form the CodeArts console expects.
func randomUUIDv4() string {
	buffer := make([]byte, 16)
	if _, errRead := rand.Read(buffer); errRead != nil {
		return randomHex(16)
	}
	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buffer[0:4], buffer[4:6], buffer[6:8], buffer[8:10], buffer[10:16])
}

func httpStatusFor(status int) int {
	if status >= 400 && status < 600 {
		return status
	}
	return http.StatusBadGateway
}
