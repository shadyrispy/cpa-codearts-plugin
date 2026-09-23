package main

// The gateway reports quota and rate-limit failures inside an HTTP 200 stream.
// Those bytes used to arrive after the response status had already been handed to
// the client, so the host saw a successful empty stream and kept sending traffic
// to the exhausted account. These tests pin the gate that turns a pre-answer
// envelope back into a failed request the host can route around.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const answerFrame = "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"}}]}\n\n"

type gateHost struct {
	mu      sync.Mutex
	frames  []hostHTTPStreamChunk
	next    int
	emitted [][]byte
	errs    []string
	closes  int
}

// gateQuotaFrame is the captured live envelope wrapped in the SSE framing upstream uses.
const gateQuotaFrame = "data: " + quotaFaultFrame + "\n\n"

// runGateStream drives one async executor stream against a scripted upstream.
func runGateStream(t *testing.T, frames []hostHTTPStreamChunk) (envelope, *gateHost) {
	t.Helper()
	cfg := defaultConfig()
	cfg.DiscoverModels = false
	cfg.ChatSessionHeartbeat = false
	useModelTestConfig(t, cfg)
	cred := credential{AccessKeyID: "gate-ak", SecretAccessKey: "gate-sk", SecurityToken: "gate-sts"}
	host := &gateHost{frames: frames}
	testHost(t, func(method string, request any) (json.RawMessage, error) {
		switch method {
		case "host.http.do_stream":
			return json.Marshal(hostHTTPStreamOpen{StatusCode: http.StatusOK, StreamID: "upstream-gate"})
		case "host.http.stream_read":
			host.mu.Lock()
			defer host.mu.Unlock()
			if host.next >= len(host.frames) {
				return json.Marshal(hostHTTPStreamChunk{Done: true})
			}
			frame := host.frames[host.next]
			host.next++
			return json.Marshal(frame)
		case "host.http.stream_close":
			return json.RawMessage(`{}`), nil
		case "host.stream.emit":
			req := request.(map[string]any)
			host.mu.Lock()
			defer host.mu.Unlock()
			if payload, ok := req["payload"].([]byte); ok && len(payload) > 0 {
				host.emitted = append(host.emitted, payload)
			}
			if text, ok := req["error"].(string); ok && text != "" {
				host.errs = append(host.errs, text)
			}
			return json.RawMessage(`{}`), nil
		case "host.stream.close":
			host.mu.Lock()
			defer host.mu.Unlock()
			host.closes++
			return json.RawMessage(`{}`), nil
		default:
			return nil, fmt.Errorf("unexpected host callback %s", method)
		}
	})
	req := executorRequest{}
	req.Model = "fixture-model"
	req.Payload = []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	req.StorageJSON = mustMarshal(t, cred)
	req.StreamID = "client-gate"
	raw, errStream := executorExecuteStream(mustMarshal(t, req))
	if errStream != nil {
		t.Fatal(errStream)
	}
	result := envelope{}
	if errUnmarshal := json.Unmarshal(raw, &result); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	return result, host
}

// waitFor polls a condition the pump goroutine satisfies asynchronously.
func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestQuotaEnvelopeBeforeFirstChunkBecomesRotatableFailure(t *testing.T) {
	result, host := runGateStream(t, []hostHTTPStreamChunk{{Payload: []byte(gateQuotaFrame), Done: true}})
	if result.OK || result.Error == nil {
		t.Fatalf("exhausted quota was reported as a stream the client must consume: %+v", result)
	}
	if result.Error.HTTPStatus != http.StatusForbidden || result.Error.Code != "insufficient_quota" {
		t.Fatalf("host cannot rotate on this failure: %+v", result.Error)
	}
	if !strings.Contains(result.Error.Message, "insufficient quota") || !strings.Contains(result.Error.Message, "InferHub.4291.200") {
		t.Fatalf("upstream reason was lost: %q", result.Error.Message)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.emitted) != 0 {
		t.Fatalf("a stream the client never got was already fed: %q", host.emitted)
	}
	if host.closes != 0 {
		t.Fatalf("the host stream was closed although it was never handed over: %d", host.closes)
	}
}

func TestAnsweredStreamKeepsStreamingAndReportsLaterFaultInBand(t *testing.T) {
	result, host := runGateStream(t, []hostHTTPStreamChunk{
		{Payload: []byte(answerFrame)},
		{Payload: []byte(gateQuotaFrame), Done: true},
	})
	if !result.OK {
		t.Fatalf("a stream that already answered must not be turned into a failed request: %+v", result.Error)
	}
	waitFor(t, "the in-band fault", func() bool {
		host.mu.Lock()
		defer host.mu.Unlock()
		return len(host.errs) > 0 && host.closes == 1
	})
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.emitted) == 0 || !strings.Contains(string(host.emitted[0]), "answer") {
		t.Fatalf("the partial answer was discarded: %q", host.emitted)
	}
}

func TestSilentUpstreamIsReportedAsFaultBeforeHandoff(t *testing.T) {
	result, _ := runGateStream(t, []hostHTTPStreamChunk{
		{Error: "connection reset by peer"},
	})
	if result.OK || result.Error == nil {
		t.Fatalf("a stream that died before answering was reported as success: %+v", result)
	}
	if result.Error.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("read failure lost its status: %+v", result.Error)
	}
}

// A stream that ends without ever answering still has to settle the gate: an
// unreported head would leave the executor waiting out its full request deadline
// (600s by default) before handing anything over.
func TestCleanStreamWithoutAnswerIsHandedOverWithoutWaiting(t *testing.T) {
	started := time.Now()
	result, host := runGateStream(t, nil)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the gate waited %s for a head that never arrived", elapsed)
	}
	if !result.OK {
		t.Fatalf("an empty-but-clean stream is not a failed request: %+v", result.Error)
	}
	waitFor(t, "the hand-off", func() bool {
		host.mu.Lock()
		defer host.mu.Unlock()
		return host.closes == 1
	})
}

func TestGateHandshakeNeverStrandsThePump(t *testing.T) {
	gate := newStreamGate()
	if fault := gate.await(20 * time.Millisecond); fault != nil {
		t.Fatalf("an unanswered stream must be handed over, got %v", fault)
	}
	// The pump only notices afterwards: its report must not block, and the verdict
	// the timed-out await queued has to be there for it to read.
	returned := make(chan struct{})
	go func() {
		gate.reportHead(&upstreamStreamFault{Code: "upstream_error", Status: http.StatusBadGateway, Message: "late"})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("reportHead blocked after the executor stopped listening")
	}
	select {
	case proceed := <-gate.verdict:
		if !proceed {
			t.Fatal("a timed-out gate told the pump to abort")
		}
	case <-time.After(time.Second):
		t.Fatal("the pump is blocked on a verdict that never arrives")
	}
}
