package main

// The benefit gateway is deliberately excluded from CPA's cold-start model
// registration (a slow answer there makes the host drop the whole registration),
// which leaves the time-limited benefit models unadvertised after every restart.
// These tests pin the two halves of the fix: the registration reply replays the
// list a previous refresh confirmed without touching the network, and the
// scheduled account refresh stores that list in a way that can never roll back a
// rotated token.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func benefitCatalogueHost(t *testing.T, callback func(*url.URL, http.Header) (hostHTTPResponse, error)) {
	t.Helper()
	testHost(t, func(method string, request any) (json.RawMessage, error) {
		if method != "host.http.do" {
			t.Fatalf("unexpected host callback %s", method)
		}
		req := request.(map[string]any)
		parsed, errParse := url.Parse(req["url"].(string))
		if errParse != nil {
			t.Fatal(errParse)
		}
		response, err := callback(parsed, http.Header(req["headers"].(map[string][]string)))
		if err != nil {
			return nil, err
		}
		return json.Marshal(response)
	})
}

func TestColdRegistrationReplaysRememberedBenefitCatalogue(t *testing.T) {
	resetModelCache(t)
	cfg := defaultConfig()
	cfg.BaseURL = "https://catalog.test"
	cfg.BenefitGatewayURL = "https://gateway.test"
	cfg.ModelAgentIDs = []string{"empty-agent"}
	useModelTestConfig(t, cfg)
	remembered := []ModelConfig{{ID: "glm-5.3-flash", DisplayName: "glm-5.3-flash", ContextLength: 1048576, MaxOutputTokens: 131072, Source: "benefit"}}
	cred := credential{
		AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "sts",
		BenefitCatalogue: &benefitCatalogueSnapshot{FetchedAt: time.Now().Add(-time.Hour).UTC(), Models: remembered},
	}
	benefitTouches := 0
	benefitCatalogueHost(t, func(u *url.URL, _ http.Header) (hostHTTPResponse, error) {
		if u.Path == "/api/v1/gateway/config" || u.Path == "/v1/benefit-gateway-config" {
			benefitTouches++
			return modelJSON([]byte(`{}`)), nil
		}
		return modelJSON([]byte(`{"gpts":{"models":[]}}`)), nil
	})
	raw, errModels := modelsForAuth(mustMarshal(t, pluginapi.AuthModelRequest{
		AuthProvider: providerID, StorageJSON: mustMarshal(t, map[string]any{storageKey: cred}),
	}))
	response := testEnvelopeResult[pluginapi.ModelResponse](t, raw, errModels)
	if benefitTouches != 0 {
		t.Fatalf("cold registration performed %d benefit gateway requests", benefitTouches)
	}
	if len(response.Models) != 1 || response.Models[0].ID != "glm-5.3-flash" {
		t.Fatalf("remembered benefit model was not advertised: %+v", response.Models)
	}
}

func TestConfirmBenefitCatalogueRemembersWithoutRollingBackTokens(t *testing.T) {
	resetModelCache(t)
	cfg := defaultConfig()
	cfg.BaseURL = "https://catalog.test"
	cfg.BenefitGatewayURL = "https://gateway.test"
	useModelTestConfig(t, cfg)
	// The host already holds a newer security token than the one this refresh
	// started with; saving must keep the host's copy.
	stored := credential{AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "sts-current"}
	storedPayload := mustMarshal(t, map[string]any{storageKey: stored})
	older := stored
	older.SecurityToken = "sts-from-earlier-run"
	var savedName string
	var savedBody []byte
	testHost(t, func(method string, request any) (json.RawMessage, error) {
		switch method {
		case "host.http.do":
			req := request.(map[string]any)
			parsed, errParse := url.Parse(req["url"].(string))
			if errParse != nil {
				t.Fatal(errParse)
			}
			return benefitCatalogueHostResponse(t, cfg, parsed)
		case "host.auth.list":
			return json.Marshal(map[string]any{"files": []pluginapi.HostAuthFileEntry{
				{AuthIndex: "idx-1", Name: "account.json", Provider: providerID},
			}})
		case "host.auth.get":
			return json.Marshal(map[string]any{"auth_index": "idx-1", "name": "account.json", "json": json.RawMessage(storedPayload)})
		case "host.auth.save":
			req := request.(map[string]any)
			savedName = req["name"].(string)
			body, errMarshal := json.Marshal(req["json"])
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			savedBody = body
			return json.Marshal(map[string]any{"ok": true})
		default:
			t.Fatalf("unexpected host callback %s", method)
			return nil, nil
		}
	})
	if errConfirm := confirmBenefitCatalogue(cfg, "idx-1", &older, time.Now()); errConfirm != nil {
		t.Fatalf("confirm failed: %v", errConfirm)
	}
	if savedName != "account.json" {
		t.Fatalf("no credential was saved (name %q)", savedName)
	}
	doc := map[string]any{}
	if errUnmarshal := json.Unmarshal(savedBody, &doc); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	restored, errCred := credentialFromStorage(savedBody)
	if errCred != nil {
		t.Fatal(errCred)
	}
	if restored.SecurityToken != "sts-current" {
		t.Fatalf("saving the catalogue rolled the token back to %q", restored.SecurityToken)
	}
	if restored.BenefitCatalogue == nil || len(restored.BenefitCatalogue.Models) != 3 {
		t.Fatalf("benefit catalogue was not remembered: %+v", restored.BenefitCatalogue)
	}
	for _, model := range restored.BenefitCatalogue.Models {
		if model.Source != "benefit" {
			t.Fatalf("remembered model %s lost its benefit route: %+v", model.ID, model)
		}
	}
	if got := restored.BenefitCatalogue.FetchedAt.Sub(time.Now().UTC()).Abs(); got > time.Minute {
		t.Fatalf("fetched_at = %s, want about now", restored.BenefitCatalogue.FetchedAt)
	}
}

func TestConfirmBenefitCatalogueKeepsLastGoodListWhenUpstreamFails(t *testing.T) {
	resetModelCache(t)
	cfg := defaultConfig()
	cfg.BaseURL = "https://catalog.test"
	cfg.BenefitGatewayURL = "https://gateway.test"
	useModelTestConfig(t, cfg)
	previous := []ModelConfig{{ID: "glm-5.3-flash", DisplayName: "glm-5.3-flash", ContextLength: 1048576, Source: "benefit"}}
	cred := &credential{AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "sts",
		BenefitCatalogue: &benefitCatalogueSnapshot{FetchedAt: time.Now().Add(-24 * time.Hour).UTC(), Models: previous}}
	saves := 0
	testHost(t, func(method string, request any) (json.RawMessage, error) {
		if method == "host.auth.save" {
			saves++
			return json.Marshal(map[string]any{"ok": true})
		}
		if method == "host.auth.list" {
			return json.Marshal(map[string]any{"files": []pluginapi.HostAuthFileEntry{{AuthIndex: "idx-1", Name: "account.json", Provider: providerID}}})
		}
		req := request.(map[string]any)
		if method != "host.http.do" {
			t.Fatalf("unexpected host callback %s", method)
		}
		parsed, errParse := url.Parse(req["url"].(string))
		if errParse != nil {
			t.Fatal(errParse)
		}
		switch parsed.Path {
		case "/v1/benefit-gateway-config":
			return json.Marshal(hostHTTPResponse{StatusCode: http.StatusServiceUnavailable, Body: []byte(`{"error":"IAM unavailable"}`)})
		default:
			return json.Marshal(modelJSON([]byte(`{"gpts":{"models":[]}}`)))
		}
	})
	if errConfirm := confirmBenefitCatalogue(cfg, "idx-1", cred, time.Now()); errConfirm == nil {
		t.Fatal("a failed benefit read was reported as a success")
	} else if !strings.Contains(errConfirm.Error(), "benefit") && !strings.Contains(errConfirm.Error(), "catalogue") {
		t.Fatalf("unhelpful error: %v", errConfirm)
	}
	if saves != 0 {
		t.Fatalf("a failed benefit read rewrote the credential %d time(s)", saves)
	}
}

func TestBenefitCatalogueDecidesWhenToReconfirm(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	live := defaultConfig()
	live.DiscoverModels = true
	live.BenefitGatewayURL = "https://gateway.test"
	cases := []struct {
		name string
		cfg  *Config
		cred *credential
		want bool
	}{
		{"never fetched", live, &credential{AccessKeyID: "ak", SecretAccessKey: "sk"}, true},
		{"fresh", live, &credential{AccessKeyID: "ak", SecretAccessKey: "sk", BenefitCatalogue: &benefitCatalogueSnapshot{FetchedAt: now, Models: []ModelConfig{{ID: "x"}}}}, false},
		{"expired", live, &credential{AccessKeyID: "ak", SecretAccessKey: "sk", BenefitCatalogue: &benefitCatalogueSnapshot{FetchedAt: now.Add(-7 * time.Hour), Models: []ModelConfig{{ID: "x"}}}}, true},
		{"empty list", live, &credential{AccessKeyID: "ak", SecretAccessKey: "sk", BenefitCatalogue: &benefitCatalogueSnapshot{FetchedAt: now}}, true},
		{"discovery off", func() *Config { c := *live; c.DiscoverModels = false; return &c }(), &credential{AccessKeyID: "ak", SecretAccessKey: "sk"}, false},
		{"native mode", func() *Config { c := *live; c.APIMode = "native"; return &c }(), &credential{AccessKeyID: "ak", SecretAccessKey: "sk"}, false},
		{"no credential", live, &credential{}, false},
	}
	for _, tc := range cases {
		if got := benefitCatalogueStale(tc.cfg, tc.cred, now); got != tc.want {
			t.Errorf("%s: stale = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBenefitCatalogueUnchangedIgnoresOrderAndMetadata(t *testing.T) {
	stored := &benefitCatalogueSnapshot{Models: []ModelConfig{
		{ID: "a", ContextLength: 100, MaxOutputTokens: 10},
		{ID: "b", ContextLength: 200, MaxOutputTokens: 20},
	}}
	same := []ModelConfig{
		{ID: "b", ContextLength: 200, MaxOutputTokens: 20},
		{ID: "a", ContextLength: 100, MaxOutputTokens: 10},
	}
	if !benefitCatalogueUnchanged(stored, same) {
		t.Fatal("an unchanged list was treated as changed")
	}
	grew := append(append([]ModelConfig{}, same...), ModelConfig{ID: "c"})
	if benefitCatalogueUnchanged(stored, grew) {
		t.Fatal("an added model was treated as unchanged")
	}
	metadata := []ModelConfig{
		{ID: "a", ContextLength: 100, MaxOutputTokens: 11},
		{ID: "b", ContextLength: 200, MaxOutputTokens: 20},
	}
	if benefitCatalogueUnchanged(stored, metadata) {
		t.Fatal("a changed context window was treated as unchanged")
	}
	if benefitCatalogueUnchanged(nil, same) {
		t.Fatal("an account without a stored list reported no change")
	}
}

func benefitCatalogueHostResponse(t *testing.T, cfg *Config, u *url.URL) (json.RawMessage, error) {
	t.Helper()
	switch u.Path {
	case "/v1/agent-center/agents/useragents":
		return json.Marshal(modelJSON(modelFixture(t, "useragents")))
	case "/v1/agent-center/agents/detail":
		if u.Query().Get("agent_id") == "a8bcb36232554267a5142361cc25a393" {
			return json.Marshal(modelJSON(modelFixture(t, "agent-detail")))
		}
		return json.Marshal(modelJSON([]byte(`{"gpts":{"models":[]}}`)))
	case "/v1/model/builtin":
		return json.Marshal(modelJSON(modelFixture(t, "builtin")))
	case "/v1/benefit-gateway-config":
		return json.Marshal(modelJSON([]byte(`{"enabled":true}`)))
	case "/api/v1/gateway/config":
		return json.Marshal(modelJSON(modelFixture(t, "gateway-config")))
	default:
		t.Fatalf("unexpected catalogue path %s", u.Path)
		return nil, fmt.Errorf("unexpected path %s", u.Path)
	}
}
