package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const dailyClaimOK = `{"error_code":"0000","error_msg":"success","result":{"channel":"codearts"}}`

func dailyClaimFixture(t *testing.T) (*Config, pluginapi.HostAuthFileEntry, *credential, *atomic.Int32) {
	t.Helper()
	cfg := defaultConfig()
	cfg.Schedule.Enabled = false
	file := pluginapi.HostAuthFileEntry{AuthIndex: "claim-fixture", Provider: providerID, Name: "fixture.json", Path: filepath.Join(t.TempDir(), "fixture.json")}
	cred := &credential{DomainID: "fixture-account", UserID: "fixture-user", AccessKeyID: "fake-ak", SecretAccessKey: "fake-sk", SecurityToken: "fake-token"}
	var calls atomic.Int32
	testHost(t, func(method string, request any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return json.Marshal(map[string]any{"files": []pluginapi.HostAuthFileEntry{file}})
		case "host.auth.get":
			doc, _ := buildAuthFileDocument(*cred)
			return json.Marshal(map[string]any{"json": json.RawMessage(doc)})
		case "host.http.do":
			calls.Add(1)
			req := request.(map[string]any)
			headers := http.Header(req["headers"].(map[string][]string))
			if req["method"] != "POST" || req["url"] != cfg.BenefitGatewayURL+epDailyClaim || headers.Get("X-Domain-Id") != "" || headers.Get("Content-Type") != "application/json" || !strings.Contains(headers.Get("Authorization"), "SignedHeaders=content-type;host;x-sdk-date;x-security-token,") {
				t.Error("claim request did not match official method/endpoint/signature")
			}
			if b, ok := req["body"].([]byte); ok && len(b) != 0 {
				t.Error("claim body must be empty")
			}
			return json.Marshal(hostHTTPResponse{StatusCode: 200, Body: []byte(dailyClaimOK)})
		default:
			return json.RawMessage(`{}`), nil
		}
	})
	previous := config()
	currentConfig.Store(cfg)
	t.Cleanup(func() { stopScheduleSettings(); currentConfig.Store(previous) })
	return cfg, file, cred, &calls
}

func TestDailyClaimOptInAndManualWithAutomaticOff(t *testing.T) {
	cfg, _, _, calls := dailyClaimFixture(t)
	if cfg.DailyClaim.Enabled || len(cfg.scheduleTasks()) != 2 || len(cfg.visibleScheduleTasks()) != 3 {
		t.Fatal("daily claims must be opt-in and visible")
	}
	now := func() time.Time { return time.Date(2026, 9, 22, 12, 0, 0, 0, dailyClaimZone) }
	if err := runDailyClaimSweep(dailyClaimTask(false), now, false); err != nil || calls.Load() != 0 {
		t.Fatal("automatic claim ignored disabled flags")
	}
	if err := runDailyClaimSweep(dailyClaimTask(false), now, true); err != nil || calls.Load() != 1 {
		t.Fatalf("manual claim failed: %v", err)
	}
	cfg.Schedule.Tasks = []ScheduleTask{{ID: "custom", Type: TaskHTTP}}
	cfg.DailyClaim.Enabled = true
	if tasks := cfg.scheduleTasks(); len(tasks) != 2 || tasks[0].ID != "custom" {
		t.Fatal("custom task was replaced")
	}
}

func TestDailyClaimConcurrentRestartAndNextDay(t *testing.T) {
	cfg, file, cred, calls := dailyClaimFixture(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, dailyClaimZone)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			if _, err := claimAccount(cfg, file, cred, now, true); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("duplicate claims: %d", calls.Load())
	}
	path, _ := dailyClaimPath(cfg, file, cred)
	data, _ := os.ReadFile(path)
	for _, secret := range []string{cred.AccessKeyID, cred.SecretAccessKey, cred.SecurityToken, cred.DomainID} {
		if strings.Contains(string(data), secret) {
			t.Error("state leaked identity or credentials")
		}
	}
	// No in-memory success cache: replacing locks simulates a fresh process.
	dailyClaimLocks.Delete(path)
	refreshed := *cred
	refreshed.AccessKeyID = "rotated-ak"
	if result, err := claimAccount(cfg, file, &refreshed, now.Add(time.Hour), true); err != nil || result != "already" {
		t.Fatalf("restart/refresh lost dedup: %s %v", result, err)
	}
	if _, err := claimAccount(cfg, file, cred, now.Add(24*time.Hour), false); err != nil || calls.Load() != 2 {
		t.Fatalf("next-day claim failed: %v", err)
	}
}

func TestDailyClaimBeijingBoundaryAndRetryBudget(t *testing.T) {
	cfg, file, cred, calls := dailyClaimFixture(t)
	before := time.Date(2026, 9, 21, 16, 4, 0, 0, time.UTC)
	if _, err := claimAccount(cfg, file, cred, before, false); err != nil || calls.Load() != 0 {
		t.Fatal("ran before Beijing 00:05")
	}
	testHost(t, func(method string, request any) (json.RawMessage, error) {
		if method == "host.http.do" {
			calls.Add(1)
			return json.Marshal(hostHTTPResponse{StatusCode: 500})
		}
		return json.RawMessage(`{}`), nil
	})
	for i := 0; i < 7; i++ {
		now := before.Add(time.Minute + time.Duration(i)*10*time.Minute)
		_, err := claimAccount(cfg, file, cred, now, true)
		if i < 6 && err == nil {
			t.Fatal("upstream failure reported success")
		}
		_, _ = claimAccount(cfg, file, cred, now.Add(time.Minute), true)
	}
	if calls.Load() != 6 {
		t.Fatalf("retry budget violated: %d", calls.Load())
	}
}

func TestDailyClaimStorageFailsClosed(t *testing.T) {
	cfg, file, cred, calls := dailyClaimFixture(t)
	missing := file
	missing.Path = ""
	if _, err := claimAccount(cfg, missing, cred, time.Now(), true); err == nil {
		t.Fatal("missing path accepted")
	}
	path, _ := dailyClaimPath(cfg, file, cred)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := claimAccount(cfg, file, cred, time.Now(), true); err == nil || calls.Load() != 0 {
		t.Fatal("corrupt state sent a claim")
	}
}

func TestDailyClaimRejectsFalseSuccessAndNeverLeaksResponse(t *testing.T) {
	cfg, _, cred, _ := dailyClaimFixture(t)
	for _, body := range []string{`{}`, `{"error_code":"0000"}`, `{"error_code":"0000","result":{"channel":"wrong"}}`, `{"error_code":"denied","error_msg":"fake-token"}`, "not JSON fake-token"} {
		func() {
			restore := setHostCall(func(string, any) (json.RawMessage, error) {
				return json.Marshal(hostHTTPResponse{StatusCode: 200, Body: []byte(body)})
			})
			defer restore()
			err := requestDailyClaim(cfg, cred)
			if err == nil || strings.Contains(err.Error(), "fake-token") {
				t.Fatalf("invalid response handling: %v", err)
			}
		}()
	}
}

func TestDailyClaimSweepFiltersAndDeduplicatesAccounts(t *testing.T) {
	cfg, file, cred, calls := dailyClaimFixture(t)
	duplicate, disabled, foreign, second := file, file, file, file
	duplicate.AuthIndex, duplicate.Name = "duplicate", "duplicate.json"
	duplicate.Path = filepath.Join(filepath.Dir(file.Path), duplicate.Name)
	disabled.AuthIndex, disabled.Disabled = "disabled", true
	foreign.AuthIndex, foreign.Provider = "foreign", "other"
	second.AuthIndex, second.Name = "second", "second.json"
	second.Path = filepath.Join(filepath.Dir(file.Path), second.Name)
	file.Unavailable = true // Quota exhaustion is not a reason to skip a claim.
	restore := setHostCall(func(method string, request any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return json.Marshal(map[string]any{"files": []pluginapi.HostAuthFileEntry{file, duplicate, disabled, foreign, second}})
		case "host.auth.get":
			id := request.(map[string]any)["auth_index"].(string)
			if id == "disabled" || id == "foreign" {
				t.Error("read excluded account")
			}
			copyCred := *cred
			if id == "second" {
				copyCred.DomainID = "second-account"
			}
			doc, _ := buildAuthFileDocument(copyCred)
			return json.Marshal(map[string]any{"json": json.RawMessage(doc)})
		case "host.http.do":
			calls.Add(1)
			return json.Marshal(hostHTTPResponse{StatusCode: 200, Body: []byte(dailyClaimOK)})
		default:
			return nil, fmt.Errorf("unexpected callback %s", method)
		}
	})
	defer restore()
	if err := runDailyClaimSweep(dailyClaimTask(false), time.Now, true); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || cfg.DailyClaim.Enabled {
		t.Fatalf("wanted two manual claims, got %d", calls.Load())
	}
}
