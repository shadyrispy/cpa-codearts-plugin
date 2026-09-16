package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestCheckinTaskRequiresURL documents that a checkin task without a captured
// URL fails with an actionable message instead of calling something arbitrary.
func TestCheckinTaskRequiresURL(t *testing.T) {
	errRun := runCheckinTask(ScheduleTask{ID: "ci", Type: TaskCheckin})
	if errRun == nil {
		t.Fatal("a checkin task without checkin_url must fail")
	}
	if !strings.Contains(errRun.Error(), "checkin_url") {
		t.Fatalf("the error should name the missing field, got %q", errRun.Error())
	}
}

// TestCheckinTaskWithoutCredentialIsRefused verifies the claim is never sent
// unsigned, because an unsigned claim cannot be attributed to the account.
func TestCheckinTaskWithoutCredentialIsRefused(t *testing.T) {
	task := ScheduleTask{
		ID:            "ci",
		Type:          TaskCheckin,
		CheckinURL:    "https://example.invalid/claim",
		CheckinMethod: "POST",
	}
	// Without a host there is no credential inventory, so firstCredential fails.
	errRun := runCheckinTask(task)
	if errRun == nil {
		t.Fatal("a checkin task without a usable credential must fail")
	}
	if !strings.Contains(errRun.Error(), "credential") {
		t.Fatalf("the error should mention the missing credential, got %q", errRun.Error())
	}
}

// TestCheckinConfigDefaults pins the normalisation of the checkin fields so a
// minimal task entry behaves predictably.
func TestCheckinConfigDefaults(t *testing.T) {
	cfg := ScheduleConfig{Tasks: []ScheduleTask{{
		ID:         "daily-claim",
		Type:       TaskCheckin,
		Cron:       "0 9 * * *",
		CheckinURL: "  /v1/benefit/claim  ",
	}}}
	cfg.normalize()
	task := cfg.Tasks[0]
	if task.CheckinMethod != "POST" {
		t.Errorf("checkin_method = %q, want POST by default", task.CheckinMethod)
	}
	if task.CheckinURL != "/v1/benefit/claim" {
		t.Errorf("checkin_url = %q, want it trimmed", task.CheckinURL)
	}
	if task.Type != TaskCheckin {
		t.Errorf("type = %q, want checkin", task.Type)
	}
	if errCron := validateCron(task.Cron); errCron != nil {
		t.Errorf("cron invalid: %v", errCron)
	}
}

// TestCheckinTaskFromYAML exercises the real decode path for a fully specified
// checkin task.
func TestCheckinTaskFromYAML(t *testing.T) {
	yamlDoc := `
enabled: true
base_url: "https://snap-access.cn-north-4.myhuaweicloud.com"
schedule:
  enabled: true
  tasks:
    - id: "daily-checkin"
      type: "checkin"
      cron: "0 9 * * *"
      checkin_method: "POST"
      checkin_url: "/v1/activity/benefit/daily/claim"
      checkin_body: '{"activityCode":"daily_sign_in"}'
      checkin_headers:
        x-snap-traceid: "fixed"
      checkin_success_marker: '"code":"0"'
      checkin_already_marker: "already"
`
	request, _ := json.Marshal(map[string]any{
		"config_yaml": encodeBase64([]byte(yamlDoc)),
	})
	cfg, errParse := parseConfig(request)
	if errParse != nil {
		t.Fatalf("parseConfig: %v", errParse)
	}
	tasks := cfg.scheduleTasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	task := tasks[0]
	if task.Type != TaskCheckin {
		t.Fatalf("type = %q, want checkin", task.Type)
	}
	if task.CheckinURL != "/v1/activity/benefit/daily/claim" {
		t.Errorf("checkin_url = %q", task.CheckinURL)
	}
	if task.CheckinBody != `{"activityCode":"daily_sign_in"}` {
		t.Errorf("checkin_body = %q", task.CheckinBody)
	}
	if task.CheckinHeaders["x-snap-traceid"] != "fixed" {
		t.Errorf("checkin_headers = %v", task.CheckinHeaders)
	}
	if task.CheckinSuccessMarker != `"code":"0"` {
		t.Errorf("checkin_success_marker = %q", task.CheckinSuccessMarker)
	}
	if task.CheckinAlreadyMarker != "already" {
		t.Errorf("checkin_already_marker = %q", task.CheckinAlreadyMarker)
	}
}

// TestRecordTaskResultKeepsRicherOutcome ensures a task-specific result is not
// overwritten by the generic "ok" placeholder.
func TestRecordTaskResultKeepsRicherOutcome(t *testing.T) {
	const id = "test-record-result"
	recordTaskResult(id, "already claimed today")
	scheduler.record(id, nowTime(), nil)

	states := scheduler.snapshot()
	state, ok := states[id]
	if !ok {
		t.Fatalf("task %q was not recorded", id)
	}
	if state.LastResult != "already claimed today" {
		t.Fatalf("last_result = %q, want the specific outcome preserved", state.LastResult)
	}
	if state.LastError != "" {
		t.Fatalf("last_error = %q, want empty for a success", state.LastError)
	}
}

// TestBenefitsRouteExplainsUnconfiguredState verifies the operator gets
// actionable guidance instead of a bare failure when no checkin task exists.
func TestBenefitsAndCheckinExplainUnconfiguredState(t *testing.T) {
	cfg := defaultConfig()
	cfg.Schedule = ScheduleConfig{Enabled: true, DisableDefaults: true, Tasks: nil}
	currentConfig.Store(cfg)
	defer currentConfig.Store(defaultConfig())

	resp := handleBenefits()
	if resp.StatusCode != 200 {
		t.Fatalf("benefits status = %d, want 200", resp.StatusCode)
	}
	var described map[string]any
	if errUnmarshal := json.Unmarshal(resp.Body, &described); errUnmarshal != nil {
		t.Fatalf("benefits body is not JSON: %v", errUnmarshal)
	}
	if described["configured"] != false {
		t.Errorf("configured = %v, want false", described["configured"])
	}
	if described["explanation"] == nil {
		t.Error("the explanation should always be present")
	}
	if len(described["how_to_capture"].([]any)) == 0 {
		t.Error("capture instructions should be present when nothing is configured")
	}

	checkin := handleCheckin(pluginapiManagementRequest(`{}`))
	var out map[string]any
	if errUnmarshal := json.Unmarshal(checkin.Body, &out); errUnmarshal != nil {
		t.Fatalf("checkin body is not JSON: %v", errUnmarshal)
	}
	if out["configured"] != false {
		t.Errorf("checkin configured = %v, want false", out["configured"])
	}
	steps, _ := out["how_to_enable"].([]any)
	if len(steps) == 0 {
		t.Error("checkin should explain how to enable the task")
	}
}

// nowTime is a tiny indirection so the test does not need the time package for
// a single call site.
func nowTime() time.Time { return time.Now() }

// pluginapiManagementRequest builds a ManagementRequest with a JSON body.
func pluginapiManagementRequest(body string) pluginapi.ManagementRequest {
	return pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/codearts-provider/checkin", Body: []byte(body)}
}
