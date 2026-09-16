package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// agentModePath is the OpenAI-compatible endpoint the official extension uses in
// agent mode.
const agentModePath = "/api/v2/chat/completions"

// nativeChatPath is the proprietary CodeArts chat endpoint.
const nativeChatPath = "/v1/chat/chat"

// activeStreams tracks in-flight executor streams so shutdown can stop them.
var activeStreams sync.Map

// executorRequest mirrors the ExecutorRequest JSON the host sends, including the
// plugin-only stream identifier the host adds for streaming calls.
type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// executorExecute handles the non-streaming execution path. It always streams
// upstream and aggregates, because the CodeArts native protocol only offers a
// streaming variant.
func executorExecute(request []byte) ([]byte, error) {
	req, errDecode := decodeExecutorRequest(request)
	if errDecode != nil {
		return nil, errDecode
	}
	cfg := config()
	cred, errCred := credentialFromStorage(req.StorageJSON)
	if errCred != nil {
		return failEnvelope("invalid_credential", "stored credential could not be decoded: "+errCred.Error(), http.StatusUnauthorized)
	}
	if !cred.valid() && !cfg.InsistMissingCredentials {
		return failEnvelope(
			"missing_credential",
			"no CodeArts Doer credential is available for this model; sign in through the management API or add an auth file",
			http.StatusUnauthorized,
		)
	}

	upstreamBody, endpoint, headers, errBuild := buildUpstreamRequest(cfg, req, cred, true)
	if errBuild != nil {
		return failEnvelope("invalid_request", errBuild.Error(), http.StatusBadRequest)
	}

	response, errDo := hostHTTPDo(http.MethodPost, endpoint, headers, upstreamBody)
	if errDo != nil {
		return failEnvelope("upstream_unreachable", "upstream request failed: "+errDo.Error(), http.StatusBadGateway)
	}
	if response.StatusCode != http.StatusOK {
		status := httpStatusFor(response.StatusCode)
		code := "upstream_error"
		if response.StatusCode == http.StatusUnauthorized {
			code = "invalid_credential"
		} else if response.StatusCode == http.StatusForbidden {
			code = "insufficient_quota"
		} else if response.StatusCode == http.StatusTooManyRequests {
			code = "rate_limit_exceeded"
		}
		return failEnvelope(code, fmt.Sprintf("upstream returned HTTP %d: %s", response.StatusCode, truncate(string(response.Body), 500)), status)
	}

	payload, errAggregate := aggregateUpstream(cfg, req.Model, response.Body)
	if errAggregate != nil {
		return failEnvelope("upstream_error", errAggregate.Error(), http.StatusBadGateway)
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

// executorExecuteStream handles the streaming path. When the host supplies a
// stream id the plugin emits chunks as they arrive; otherwise it falls back to
// buffering the whole response into the single synchronous reply.
func executorExecuteStream(request []byte) ([]byte, error) {
	req, errDecode := decodeExecutorRequest(request)
	if errDecode != nil {
		return nil, errDecode
	}
	cfg := config()
	cred, errCred := credentialFromStorage(req.StorageJSON)
	if errCred != nil {
		return nil, fmt.Errorf("decode stored credential: %w", errCred)
	}
	if !cred.valid() && !cfg.InsistMissingCredentials {
		return nil, fmt.Errorf("no CodeArts Doer credential is available for this model; sign in through the management API or add an auth file")
	}

	upstreamBody, endpoint, headers, errBuild := buildUpstreamRequest(cfg, req, cred, true)
	if errBuild != nil {
		return nil, errBuild
	}

	// Without a stream id the host cannot receive asynchronous chunks, so the
	// response is buffered and returned inline.
	if strings.TrimSpace(req.StreamID) == "" {
		response, errDo := hostHTTPDo(http.MethodPost, endpoint, headers, upstreamBody)
		if errDo != nil {
			return nil, fmt.Errorf("upstream request failed: %w", errDo)
		}
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("upstream returned HTTP %d: %s", response.StatusCode, truncate(string(response.Body), 400))
		}
		return bufferedStreamResponse(cfg, req.Model, response.Body)
	}

	stop, errRegister := registerActiveStream(req.StreamID)
	if errRegister != nil {
		return nil, errRegister
	}
	go func() {
		defer stop()
		runUpstreamStream(cfg, req, endpoint, headers, upstreamBody, cred)
	}()

	// An empty chunk list tells the host to consume the async stream bridge.
	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

// runUpstreamStream drives one upstream streaming request and forwards
// translated frames to the host. It always closes the stream exactly once.
func runUpstreamStream(cfg *Config, req executorRequest, endpoint string, headers map[string]string, body []byte, cred *credential) {
	streamID := req.StreamID
	fail := func(message string) {
		emitStreamError(streamID, message)
		closeStream(streamID)
	}

	open, errOpen := hostHTTPDoStream(req.HostCallbackID, http.MethodPost, endpoint, headers, body)
	if errOpen != nil {
		fail("upstream stream could not be opened: " + errOpen.Error())
		return
	}
	if open.StatusCode != http.StatusOK {
		snippet := ""
		if payload, done, errRead := hostHTTPStreamRead(open.StreamID); errRead == nil && done && len(payload) > 0 {
			snippet = truncate(string(payload), 400)
		}
		_ = hostHTTPStreamClose(open.StreamID)
		fail(fmt.Sprintf("upstream returned HTTP %d: %s", open.StatusCode, snippet))
		return
	}

	translator := newStreamRenderer(cfg, req.Model)

	for {
		payload, done, errRead := hostHTTPStreamRead(open.StreamID)
		if errRead != nil {
			_ = hostHTTPStreamClose(open.StreamID)
			fail("upstream stream read failed: " + errRead.Error())
			return
		}
		if len(payload) > 0 {
			for _, frame := range translator.feed(payload) {
				if errEmit := hostStreamEmit(streamID, frame); errEmit != nil {
					// The client went away; stop reading upstream.
					_ = hostHTTPStreamClose(open.StreamID)
					closeStream(streamID)
					return
				}
			}
		}
		if done {
			break
		}
	}
	_ = hostHTTPStreamClose(open.StreamID)

	for _, frame := range translator.finish() {
		if errEmit := hostStreamEmit(streamID, frame); errEmit != nil {
			closeStream(streamID)
			return
		}
	}
	closeStream(streamID)
}

// bufferedStreamResponse renders the whole upstream response as one SSE reply.
func bufferedStreamResponse(cfg *Config, model string, body []byte) ([]byte, error) {
	renderer := newStreamRenderer(cfg, model)
	frames := renderer.feed(body)
	frames = append(frames, renderer.finish()...)

	joined := bytes.Join(frames, nil)
	// The host decodes this result into rpcExecutorStreamResponse, which is
	// tagged in snake_case.
	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
		"chunks": []pluginapi.ExecutorStreamChunk{
			{Payload: joined},
		},
	})
}

// executorCountTokens returns a conservative local estimate. CodeArts Doer does
// not expose a token counting endpoint, so the plugin deliberately avoids
// claiming an exact count.
func executorCountTokens(request []byte) ([]byte, error) {
	req, errDecode := decodeExecutorRequest(request)
	if errDecode != nil {
		return nil, errDecode
	}
	total := estimateTokens(req.Payload) + estimateTokens(req.OriginalRequest)
	payload, errMarshal := json.Marshal(map[string]any{
		"total_tokens": total,
		"note":         "estimated locally; CodeArts Doer does not expose a token counting endpoint",
	})
	if errMarshal != nil {
		return nil, errMarshal
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

// executorHTTPRequest is not supported: the CodeArts Doer gateway is not an
// OpenAI-compatible passthrough, and silently proxying arbitrary paths would
// hide that from the caller.
func executorHTTPRequest(_ []byte) ([]byte, error) {
	return failEnvelope(
		"unsupported",
		"the CodeArts Doer plugin does not implement raw HTTP proxying; use the executor routes instead",
		http.StatusNotImplemented,
	)
}

// ---------------------------------------------------------------------------
// request construction
// ---------------------------------------------------------------------------

// buildUpstreamRequest renders the upstream body, signs it and returns the
// endpoint plus the complete header set to send.
func buildUpstreamRequest(cfg *Config, req executorRequest, cred *credential, stream bool) ([]byte, string, map[string]string, error) {
	endpoint := strings.TrimRight(cfg.BaseURL, "/")
	var body []byte
	var errBuild error

	switch cfg.APIMode {
	case "native":
		endpoint += nativeChatPath
		body, errBuild = buildNativeBody(cfg, req, stream)
	default:
		endpoint += agentModePath
		body, errBuild = buildAgentBody(cfg, req, stream)
	}
	if errBuild != nil {
		return nil, "", nil, errBuild
	}

	headers := baseUpstreamHeaders(cfg, req)
	if !cred.valid() {
		// Debugging escape hatch: send the request unsigned.
		logWarn("sending an unsigned upstream request because no credential is available", map[string]any{"endpoint": endpoint})
		return body, endpoint, headers, nil
	}
	signed, errSign := signRequest(http.MethodPost, endpoint, headers, body, cred, cfg.SignHost)
	if errSign != nil {
		return nil, "", nil, fmt.Errorf("sign upstream request: %w", errSign)
	}
	return body, endpoint, signed, nil
}

// baseUpstreamHeaders reproduces the protocol headers the official extension
// sends. These participate in the request signature.
func baseUpstreamHeaders(cfg *Config, req executorRequest) map[string]string {
	headers := map[string]string{
		"Content-Type": "application/json",
		"Accept":       "text/event-stream",
		"client_version": firstNonEmptyString(
			cfg.ClientVersion,
			"Vscode_"+cfg.PluginVersion,
		),
		"Agent-Type":      "ChatAgent",
		"X-Language":      cfg.Language,
		"x-snap-traceid":  strings.ReplaceAll(randomUUIDv4(), "-", ""),
		"plugin-name":     cfg.PluginName,
		"plugin-version":  cfg.PluginVersion,
		"is_confidential": fmt.Sprintf("%t", cfg.IsConfidential),
	}
	if cfg.Heartbeat {
		headers["heartbeat-enable"] = "true"
	}
	for key, value := range cfg.ExtraHeaders {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		headers[trimmed] = value
	}
	// Some deployments expect the client-authorised model to travel as a
	// header as well.
	if model := strings.TrimSpace(req.Model); model != "" {
		headers["x-model-id"] = model
	}
	return headers
}

// buildAgentBody renders an OpenAI-compatible request for /api/v2/chat/completions.
func buildAgentBody(cfg *Config, req executorRequest, stream bool) ([]byte, error) {
	payload := map[string]any{}
	if len(req.Payload) > 0 {
		if errUnmarshal := json.Unmarshal(req.Payload, &payload); errUnmarshal != nil {
			return nil, fmt.Errorf("client request body is not valid JSON: %w", errUnmarshal)
		}
	}
	payload["model"] = cfg.upstreamModel(req.Model)
	payload["stream"] = stream
	if _, ok := payload["messages"]; !ok {
		return nil, fmt.Errorf("client request body has no messages array")
	}
	if stream {
		// The upstream includes a final usage frame when asked.
		payload["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(payload)
}

// nativeMessage is one CodeArts context block. The native protocol models user
// input as typed blocks rather than role/content pairs.
type nativeMessage struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// buildNativeBody renders the proprietary /v1/chat/chat request.
func buildNativeBody(cfg *Config, req executorRequest, stream bool) ([]byte, error) {
	var payload struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		User     string         `json:"user"`
		Metadata map[string]any `json:"metadata"`
	}
	if len(req.Payload) > 0 {
		if errUnmarshal := json.Unmarshal(req.Payload, &payload); errUnmarshal != nil {
			return nil, fmt.Errorf("client request body is not valid JSON: %w", errUnmarshal)
		}
	}
	if len(payload.Messages) == 0 {
		return nil, fmt.Errorf("client request body has no messages array")
	}

	messages := make([]nativeMessage, 0, len(payload.Messages))
	for _, message := range payload.Messages {
		content := decodeMessageContent(message.Content)
		if strings.TrimSpace(content) == "" {
			continue
		}
		// The native protocol has a single user-side context stream, so the role
		// is folded into the block text to preserve conversational structure.
		switch strings.ToLower(strings.TrimSpace(message.Role)) {
		case "system":
			messages = append(messages, nativeMessage{Type: "text", Content: "System instruction:\n" + content})
		case "assistant":
			messages = append(messages, nativeMessage{Type: "text", Content: "Assistant:\n" + content})
		default:
			messages = append(messages, nativeMessage{Type: "text", Content: content})
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("client request body contains no usable message content")
	}

	modelID := cfg.upstreamModel(req.Model)
	body := map[string]any{
		"chat_id":               strings.ReplaceAll(randomUUIDv4(), "-", ""),
		"messages":              messages,
		"client":                "IDE",
		"model_id":              modelID,
		"is_delta_response":     true,
		"batch_task_parameters": []any{},
		"task_parameters": map[string]any{
			"version":      "v1",
			"trigger_mode": "ENTER",
			"contexts":     []any{},
			"ide":          "CLIProxyAPI",
			"isNewClient":  true,
		},
	}
	if agentID := strings.TrimSpace(cfg.AgentID); agentID != "" {
		body["agent_id"] = agentID
	}
	if payload.User != "" {
		body["user_id"] = payload.User
	}
	return json.Marshal(body)
}

// decodeMessageContent accepts both plain string content and the array form
// used by multimodal OpenAI clients.
func decodeMessageContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if errUnmarshal := json.Unmarshal(raw, &text); errUnmarshal == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if errUnmarshal := json.Unmarshal(raw, &parts); errUnmarshal == nil {
		var builder strings.Builder
		for _, part := range parts {
			if part.Text != "" {
				if builder.Len() > 0 {
					builder.WriteString("\n")
				}
				builder.WriteString(part.Text)
			}
		}
		return builder.String()
	}
	return ""
}

// ---------------------------------------------------------------------------
// streaming renderer
// ---------------------------------------------------------------------------

// streamRenderer converts upstream bytes into client-ready OpenAI SSE frames.
type streamRenderer struct {
	mode       string
	translator *nativeTranslator
	decoder    sseDecoder
}

func newStreamRenderer(cfg *Config, model string) *streamRenderer {
	renderer := &streamRenderer{mode: cfg.APIMode}
	if cfg.APIMode == "native" {
		renderer.translator = newNativeTranslator(model)
	}
	return renderer
}

// feed consumes upstream bytes and returns frames to forward verbatim.
func (r *streamRenderer) feed(data []byte) [][]byte {
	if r.translator != nil {
		return r.translator.translate(data)
	}
	// Agent mode already streams OpenAI SSE, so frames are forwarded unchanged
	// once complete frames have been assembled.
	var out [][]byte
	for _, frame := range r.decoder.push(data) {
		out = append(out, []byte(frame+"\n\n"))
	}
	return out
}

// finish emits the terminating frames for the stream.
func (r *streamRenderer) finish() [][]byte {
	if r.translator != nil {
		return r.translator.finalFrames()
	}
	// Flush any trailing agent-mode frame that was not blank-line terminated.
	var out [][]byte
	if trailing := strings.TrimSpace(r.decoder.buffer.String()); trailing != "" {
		if !isDoneFrame([]byte(trailing)) {
			out = append(out, []byte(trailing+"\n\n"))
		}
	}
	out = append(out, []byte("data: [DONE]\n\n"))
	return out
}

// aggregateUpstream folds a complete upstream SSE body into a single
// non-streaming OpenAI completion.
func aggregateUpstream(cfg *Config, model string, body []byte) ([]byte, error) {
	renderer := newStreamRenderer(cfg, model)
	frames := renderer.feed(body)
	// Agent mode finishes with a done marker that carries no content; dropping it
	// keeps the aggregator from seeing a sentinel frame.
	aggregator := newAggregator("", cfg.upstreamModel(model))
	for _, frame := range frames {
		aggregator.addFrame(frame)
	}
	return aggregator.completion(), nil
}

func decodeExecutorRequest(request []byte) (executorRequest, error) {
	var req executorRequest
	if len(request) == 0 {
		return req, fmt.Errorf("executor request is empty")
	}
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return req, fmt.Errorf("decode executor request: %w", errUnmarshal)
	}
	return req, nil
}

// estimateTokens approximates a token count from byte length. It is deliberately
// conservative and clearly labelled as an estimate by the caller.
func estimateTokens(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	return len(data)/4 + 1
}

// ---------------------------------------------------------------------------
// stream lifecycle
// ---------------------------------------------------------------------------

// registerActiveStream records an in-flight executor stream so shutdown can
// cancel it, and returns a function that deregisters it.
func registerActiveStream(streamID string) (func(), error) {
	_, cancel := context.WithCancel(context.Background())
	if _, loaded := activeStreams.LoadOrStore(streamID, cancel); loaded {
		cancel()
		return nil, fmt.Errorf("stream %s is already active", streamID)
	}
	stop := func() {
		cancel()
		activeStreams.Delete(streamID)
	}
	return stop, nil
}

func closeAllActiveStreams() {
	activeStreams.Range(func(key, value any) bool {
		if cancel, ok := value.(context.CancelFunc); ok {
			cancel()
		}
		activeStreams.Delete(key)
		return true
	})
}

// ---------------------------------------------------------------------------
// host callback wrappers
// ---------------------------------------------------------------------------

// hostHTTPResponse matches the host's buffered HTTP reply. The host marshals
// pluginapi.HTTPResponse, which has no JSON tags, so the field names are Go
// identifiers here.
type hostHTTPResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

// hostHTTPDo performs one buffered HTTP request through the host transport so
// proxy settings, logging and request capture stay under host policy.
func hostHTTPDo(method, endpoint string, headers map[string]string, body []byte) (*hostHTTPResponse, error) {
	result, errCall := hostCall("host.http.do", map[string]any{
		"method":  method,
		"url":     endpoint,
		"headers": toHeaderMap(headers),
		"body":    body,
	})
	if errCall != nil {
		return nil, errCall
	}
	var response hostHTTPResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host http response: %w", errUnmarshal)
	}
	return &response, nil
}

// hostHTTPStreamOpen matches the host's rpcHostHTTPStreamResponse, which is
// tagged in snake_case: encoding/json does not fold "status_code" onto a Go
// field named StatusCode, so the tags must match the host exactly.
type hostHTTPStreamOpen struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers"`
	StreamID   string              `json:"stream_id"`
}

// hostHTTPStreamChunk matches the host's rpcHostHTTPStreamReadResponse.
type hostHTTPStreamChunk struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

// hostHTTPDoStream opens a streaming HTTP request through the host.
func hostHTTPDoStream(hostCallbackID, method, endpoint string, headers map[string]string, body []byte) (*hostHTTPStreamOpen, error) {
	request := map[string]any{
		"method":  method,
		"url":     endpoint,
		"headers": toHeaderMap(headers),
		"body":    body,
	}
	if strings.TrimSpace(hostCallbackID) != "" {
		// Forwarding the callback id keeps the nested execution scoped to this
		// plugin's request context.
		request["host_callback_id"] = hostCallbackID
	}
	result, errCall := hostCall("host.http.do_stream", request)
	if errCall != nil {
		return nil, errCall
	}
	var open hostHTTPStreamOpen
	if errUnmarshal := json.Unmarshal(result, &open); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host http stream open: %w", errUnmarshal)
	}
	if open.StreamID == "" {
		return nil, fmt.Errorf("host returned no stream id")
	}
	return &open, nil
}

// hostHTTPStreamRead reads the next chunk of a host-brokered HTTP stream.
func hostHTTPStreamRead(streamID string) ([]byte, bool, error) {
	result, errCall := hostCall("host.http.stream_read", map[string]any{"stream_id": streamID})
	if errCall != nil {
		return nil, false, errCall
	}
	var chunk hostHTTPStreamChunk
	if errUnmarshal := json.Unmarshal(result, &chunk); errUnmarshal != nil {
		return nil, false, fmt.Errorf("decode host stream chunk: %w", errUnmarshal)
	}
	if chunk.Error != "" {
		return chunk.Payload, chunk.Done, fmt.Errorf("%s", chunk.Error)
	}
	return chunk.Payload, chunk.Done, nil
}

// hostHTTPStreamClose releases a host-brokered HTTP stream. Streaming callbacks
// must always be closed explicitly.
func hostHTTPStreamClose(streamID string) error {
	_, errCall := hostCall("host.http.stream_close", map[string]any{"stream_id": streamID})
	return errCall
}

// hostStreamEmit pushes one translated frame to the client.
func hostStreamEmit(streamID string, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	_, errCall := hostCall("host.stream.emit", map[string]any{
		"stream_id": streamID,
		"payload":   payload,
	})
	return errCall
}

func emitStreamError(streamID, message string) {
	_, errCall := hostCall("host.stream.emit", map[string]any{
		"stream_id": streamID,
		"error":     message,
	})
	if errCall != nil {
		logWarn("failed to report a stream error to the host", map[string]any{"error": message})
	}
}

// closeStream signals the end of an executor stream.
func closeStream(streamID string) {
	if _, errCall := hostCall("host.stream.close", map[string]any{"stream_id": streamID}); errCall != nil {
		logWarn("failed to close the host stream", map[string]any{"stream_id": streamID})
	}
}

// toHeaderMap converts a flat header map into the multi-value form the host
// expects.
func toHeaderMap(headers map[string]string) map[string][]string {
	out := make(map[string][]string, len(headers))
	for key, value := range headers {
		out[key] = []string{value}
	}
	return out
}
