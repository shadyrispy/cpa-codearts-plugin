package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This test runs an actual, CGO-enabled CPA binary with the built DLL and a
// local upstream fixture. No real subscription or credentials are used.
// CODEARTS_CPA_EXE and CODEARTS_PLUGIN_DLL opt in to the process-level test.
func TestCPAIntegration(t *testing.T) {
	executable := os.Getenv("CODEARTS_CPA_EXE")
	dll := os.Getenv("CODEARTS_PLUGIN_DLL")
	if executable == "" || dll == "" {
		t.Skip("set CODEARTS_CPA_EXE and CODEARTS_PLUGIN_DLL for real-host integration")
	}
	var chatCalls, loginCalls atomic.Int32
	var nextStatus atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/snap-manager/v1/login/ticket" {
			loginCalls.Add(1)
			// The console binds the secret to the ticket, so a callback that
			// carried none still exchanges successfully; the fixture accepts both
			// shapes to exercise that contract.
			secret := r.URL.Query().Get("secret")
			if (secret != "browser-secret" && secret != "") || r.URL.Query().Get("ticket_id") == "" {
				http.Error(w, "wrong ticket exchange", 400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"credential":{"access":"login-ak","secret":"login-sk","securitytoken":"login-sts","expires_at":"2030-01-01T00:00:00Z"},"domain_id":"tenant","user_name":"browser-user","user_id":"browser-id"}`)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "SDK-HMAC-SHA256 Access=") {
			http.Error(w, "unsigned", 401)
			return
		}
		switch r.URL.Path {
		case "/v1/agent-center/agents/detail":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"gpts":{"models":[{"model_alias":"audit-model","model_name":"Audit Model","model_parameters":{"enabled":true,"context_window":128000,"max_tokens":8192,"supports_images":true}}]},"sub_agents":[]}`)
		case agentModePath:
			chatCalls.Add(1)
			if status := nextStatus.Load(); status != 0 {
				http.Error(w, "fixture rate limit", int(status))
				return
			}
			body, _ := io.ReadAll(r.Body)
			var request map[string]json.RawMessage
			if json.Unmarshal(body, &request) != nil || string(request["model"]) != `"audit-model"` {
				http.Error(w, "wrong model", 400)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if bytes.Contains(body, []byte("lookup")) {
				fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"model\":\"audit-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\r\n\r\n")
				fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"model\":\"audit-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\r\n\r\n")
			} else {
				fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"model\":\"audit-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello from fixture\"},\"finish_reason\":null}]}\r\n\r\n")
				w.(http.Flusher).Flush()
				fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"model\":\"audit-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\r\n\r\n")
			}
			fmt.Fprint(w, "data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"model\":\"audit-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\r\n\r\ndata: [DONE]\r\n\r\n")
		default:
			http.Error(w, "unknown fixture endpoint", 404)
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auth")
	pluginDir := filepath.Join(dir, "plugins")
	for _, d := range []string{authDir, pluginDir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	library, err := os.ReadFile(dll)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(pluginDir, "codearts-provider.dll"), library, 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	configPath := filepath.Join(dir, "config.yaml")
	configYAML := fmt.Sprintf("host: 127.0.0.1\nport: %d\nauth-dir: %q\napi-keys: [audit-client]\nremote-management:\n  allow-remote: false\n  secret-key: audit-admin\n  disable-control-panel: true\nrequest-retry: 0\nplugins:\n  enabled: true\n  dir: %q\n  configs:\n    codearts-provider:\n      enabled: true\n      base_url: %q\n      discover_models: true\n", port, filepath.ToSlash(authDir), filepath.ToSlash(pluginDir), upstream.URL)
	if err = os.WriteFile(configPath, []byte(configYAML), 0600); err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 15 * time.Second}
	request := func(method, path, body string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		key := "audit-client"
		if strings.HasPrefix(path, "/v0/management/") {
			key = "audit-admin"
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, b
	}
	var process *exec.Cmd
	stop := func() {
		if process != nil {
			process.Process.Kill()
			process.Wait()
			process = nil
		}
	}
	defer stop()
	start := func() {
		t.Helper()
		process = exec.Command(executable, "-config", configPath)
		process.Dir = dir
		log, err := os.OpenFile(filepath.Join(dir, "cpa.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		process.Stdout = log
		process.Stderr = log
		if err = process.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}
		log.Close()
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := client.Get(base + "/")
			if err == nil {
				resp.Body.Close()
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		logBody, _ := os.ReadFile(filepath.Join(dir, "cpa.log"))
		t.Fatalf("CPA did not start: %s", logBody)
	}
	defer func() {
		if t.Failed() {
			stop()
			logBody, _ := os.ReadFile(filepath.Join(dir, "cpa.log"))
			t.Logf("CPA log:\n%s", logBody)
		}
	}()
	start()
	status, body := request("GET", "/v0/management/plugins", "")
	if status != 200 || !bytes.Contains(body, []byte(`"registered":true`)) || !bytes.Contains(body, []byte(`"effective_enabled":true`)) {
		t.Fatalf("plugin not enabled: %d %s", status, body)
	}
	t.Log("actual CPA loaded and enabled the DLL")
	status, body = request("POST", "/v0/management/codearts-provider/import", `{"access_key_id":"import-ak","secret_access_key":"import-sk","user_name":"imported","name":"codearts-provider-integration.json"}`)
	if status != 200 {
		t.Fatalf("import failed: %d %s", status, body)
	}
	assertModel := func() {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			status, body = request("GET", "/v1/models", "")
			if status == 200 && bytes.Contains(body, []byte(`"audit-model"`)) {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("discovered model missing: %d %s", status, body)
	}
	assertModel()
	t.Log("imported nested credential and discovered account models")
	for _, stream := range []bool{false, true} {
		status, body = request("POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"audit-model","messages":[{"role":"user","content":"hello"}],"stream":%t,"stream_options":{"include_usage":true}}`, stream))
		if status != 200 || !bytes.Contains(body, []byte("hello from fixture")) || !bytes.Contains(body, []byte(`"total_tokens":18`)) {
			t.Fatalf("chat stream=%v failed: %d %s", stream, status, body)
		}
		if stream && bytes.Count(body, []byte("[DONE]")) != 1 {
			t.Fatalf("incorrect stream termination: %s", body)
		}
	}
	status, body = request("POST", "/v1/chat/completions", `{"model":"audit-model","messages":[{"role":"user","content":"lookup"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	if status != 200 || !bytes.Contains(body, []byte(`"tool_calls"`)) || !bytes.Contains(body, []byte(`"call1"`)) {
		t.Fatalf("tool call failed: %d %s", status, body)
	}
	t.Log("streaming, non-streaming, usage and tool calls passed through CPA")
	for _, route := range []string{"/v1/messages", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			payload := fmt.Sprintf(`{"model":"audit-model","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"stream":%t}`, stream)
			if route == "/v1/responses" {
				payload = fmt.Sprintf(`{"model":"audit-model","input":"hello","stream":%t}`, stream)
			}
			status, body = request("POST", route, payload)
			if status != 200 || !bytes.Contains(body, []byte("hello from fixture")) {
				t.Fatalf("CPA format conversion %s stream=%v failed: %d %s", route, stream, status, body)
			}
		}
	}
	t.Log("CPA translated Anthropic Messages and OpenAI Responses in both modes")
	// The Anthropic route must receive complete Anthropic events, not bare JSON
	// chunks: CPA writes plugin frames for this protocol unchanged, so a missing
	// event frame silently truncates the client stream.
	status, body = request("POST", "/v1/messages", `{"model":"audit-model","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	if status != 200 {
		t.Fatalf("anthropic stream failed: %d %s", status, body)
	}
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		`"type":"text_delta"`,
		"event: content_block_stop",
		"event: message_delta",
		`"stop_reason":"end_turn"`,
		"event: message_stop",
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("anthropic stream missing %q: %s", want, body)
		}
	}
	if bytes.Count(body, []byte("event: message_start")) != 1 || bytes.Count(body, []byte("event: message_stop")) != 1 {
		t.Fatalf("anthropic stream duplicated its envelope: %s", body)
	}
	status, body = request("POST", "/v1/messages", `{"model":"audit-model","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
	if status != 200 || !bytes.Contains(body, []byte(`"type":"message"`)) || !bytes.Contains(body, []byte(`"stop_reason":"end_turn"`)) {
		t.Fatalf("anthropic non-stream envelope is not Anthropic shaped: %d %s", status, body)
	}
	// Tool calls must survive the Anthropic route as tool_use blocks.
	status, body = request("POST", "/v1/messages", `{"model":"audit-model","max_tokens":64,"messages":[{"role":"user","content":"lookup"}],"tools":[{"name":"lookup","description":"fixture tool","input_schema":{"type":"object"}}],"stream":true}`)
	if status != 200 || !bytes.Contains(body, []byte(`"type":"tool_use"`)) || !bytes.Contains(body, []byte(`"partial_json"`)) || !bytes.Contains(body, []byte(`"stop_reason":"tool_use"`)) {
		t.Fatalf("anthropic tool stream failed: %d %s", status, body)
	}
	status, body = request("POST", "/v1/messages/count_tokens", `{"model":"audit-model","messages":[{"role":"user","content":"hello"}]}`)
	if status != 200 || !bytes.Contains(body, []byte(`"input_tokens"`)) {
		t.Fatalf("anthropic count_tokens shape is wrong: %d %s", status, body)
	}
	t.Log("Anthropic streams carry the full event sequence, tool blocks and count_tokens")
	stop()
	start()
	assertModel()
	status, body = request("POST", "/v1/chat/completions", `{"model":"audit-model","messages":[{"role":"user","content":"after restart"}]}`)
	if status != 200 || !bytes.Contains(body, []byte("hello from fixture")) {
		t.Fatalf("restart lost credential: %d %s", status, body)
	}
	t.Log("saved account remains callable after CPA restart")
	status, body = request("GET", "/v0/management/codearts-provider-auth-url", "")
	if status != 200 {
		t.Fatalf("login start failed: %d %s", status, body)
	}
	var login struct {
		URL   string `json:"url"`
		State string `json:"state"`
	}
	json.Unmarshal(body, &login)
	loginURL, err := url.Parse(login.URL)
	if err != nil {
		t.Fatal(err)
	}
	callback, err := url.Parse(loginURL.Query().Get("auth_callback_url"))
	if err != nil {
		t.Fatal(err)
	}
	query := callback.Query()
	query.Set("secret", "browser-secret")
	callback.RawQuery = query.Encode()
	// Use the protected callback submission also used by remote deployments.
	callbackBody, _ := json.Marshal(map[string]string{"state": login.State, "callback_url": callback.String()})
	status, body = request("POST", "/v0/management/codearts-provider/login/callback", string(callbackBody))
	if status != 200 {
		t.Fatalf("callback failed: %d %s", status, body)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		status, body = request("GET", "/v0/management/get-auth-status?state="+url.QueryEscape(login.State), "")
		if bytes.Contains(body, []byte(`"status":"ok"`)) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if status != 200 || !bytes.Contains(body, []byte(`"status":"ok"`)) || loginCalls.Load() == 0 {
		t.Fatalf("login did not complete: %d %s", status, body)
	}
	status, body = request("GET", "/v0/management/codearts-provider/accounts", "")
	if status != 200 || !bytes.Contains(body, []byte("browser-user")) {
		t.Fatalf("CPA did not persist browser account: %d %s", status, body)
	}
	t.Log("CPA browser login/poll persisted the subscription credential")

	// Second flow: the browser hits the plugin's own callback listener with no
	// secret, which is what the CodeArts console does when it only echoes the
	// ticket. An earlier plugin version rejected that callback and the flow hung
	// forever, so this is asserted against the real host, not just unit tests.
	status, body = request("GET", "/v0/management/codearts-provider-auth-url", "")
	if status != 200 {
		t.Fatalf("second login start failed: %d %s", status, body)
	}
	var secondLogin struct {
		URL   string `json:"url"`
		State string `json:"state"`
	}
	json.Unmarshal(body, &secondLogin)
	secondURL, err := url.Parse(secondLogin.URL)
	if err != nil {
		t.Fatal(err)
	}
	secondCallback, err := url.Parse(secondURL.Query().Get("auth_callback_url"))
	if err != nil {
		t.Fatal(err)
	}
	// The pending flow must explain itself rather than stay silent.
	status, body = request("GET", "/v0/management/codearts-provider/login/status?state="+url.QueryEscape(secondLogin.State), "")
	if status != 200 || !bytes.Contains(body, []byte("浏览器")) {
		t.Fatalf("pending login did not report its stage: %d %s", status, body)
	}
	// Snapshot the exchange counter before triggering the callback: the plugin
	// polls the ticket endpoint on its own schedule and may complete the login
	// before the assertions below run.
	beforeCalls := loginCalls.Load()
	callbackResp, err := client.Get(secondCallback.String())
	if err != nil {
		t.Fatalf("browser-style callback could not reach the plugin listener: %v", err)
	}
	callbackPayload, _ := io.ReadAll(callbackResp.Body)
	callbackResp.Body.Close()
	if callbackResp.StatusCode != 200 || !bytes.Contains(callbackPayload, []byte("Secret received")) {
		t.Fatalf("plugin rejected a secret-less browser callback: %d %s", callbackResp.StatusCode, callbackPayload)
	}
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		status, body = request("GET", "/v0/management/get-auth-status?state="+url.QueryEscape(secondLogin.State), "")
		if bytes.Contains(body, []byte(`"status":"ok"`)) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if status != 200 || !bytes.Contains(body, []byte(`"status":"ok"`)) || loginCalls.Load() == beforeCalls {
		t.Fatalf("secret-less browser login did not complete: %d %s", status, body)
	}
	status, body = request("GET", "/v0/management/codearts-provider/accounts", "")
	if status != 200 || !bytes.Contains(body, []byte(`"login_type":"WEB"`)) {
		t.Fatalf("browser login was not persisted as a WEB credential: %d %s", status, body)
	}
	t.Log("secret-less browser callback completed the login through the plugin listener")

	// The dashboard is the operator-facing surface: it must be the Chinese,
	// single-authorization page the extension's own flow implies, and it must not
	// grow a second sign-in path.
	status, body = request("GET", "/v0/resource/plugins/codearts-provider/panel", "")
	if status != 200 {
		t.Fatalf("panel resource unavailable: %d", status)
	}
	for _, want := range []string{`lang="zh-CN"`, "charset=\"utf-8\"", "开始授权", "提交回调地址，完成授权", "账号", "定时任务"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("panel is missing %q", want)
		}
	}
	for _, unwanted := range []string{"Sign in to Huawei Cloud", "Import credential", "Access key ID", "Scheduled tasks"} {
		if bytes.Contains(body, []byte(unwanted)) {
			t.Fatalf("panel still exposes the old UI string %q", unwanted)
		}
	}
	t.Log("panel serves the Chinese single-authorization dashboard")

	nextStatus.Store(429)
	status, body = request("POST", "/v1/chat/completions", `{"model":"audit-model","messages":[{"role":"user","content":"rate limit"}],"stream":true}`)
	if status != 429 {
		t.Fatalf("CPA lost upstream 429: %d %s", status, body)
	}
	t.Log("CPA preserved upstream rate-limit status for streaming clients")
	if chatCalls.Load() < 4 {
		t.Fatal("chat requests did not reach the upstream")
	}
}
