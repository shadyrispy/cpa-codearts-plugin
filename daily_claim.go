package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	TaskDailyClaim   TaskType = "daily_claim"
	dailyClaimTaskID          = "daily-benefit-claim"
	epDailyClaim              = "/api/v1/benefit/claim"
)

var dailyClaimZone = time.FixedZone("Asia/Shanghai", 8*60*60)
var dailyClaimLocks sync.Map

func dailyClaimTask(enabled bool) ScheduleTask {
	return ScheduleTask{ID: dailyClaimTaskID, Type: TaskDailyClaim, Cron: "5-55/10 * * * *", Enabled: &enabled}
}

type dailyClaimState struct {
	Version     int       `json:"version"`
	Day         string    `json:"day"`
	Attempts    int       `json:"attempts"`
	LastAttempt time.Time `json:"last_attempt"`
	Accepted    bool      `json:"accepted"`
}

func dailyClaimPath(cfg *Config, file pluginapi.HostAuthFileEntry, cred *credential) (string, error) {
	if cfg.StateDir == "" && !filepath.IsAbs(file.Path) {
		return "", fmt.Errorf("账号缺少可持久化的认证文件路径")
	}
	// Account identity, not temporary keys: refreshes and duplicate logins must
	// not cause a second claim. Fall back to the file name for imported AK/SK.
	identity := firstNonEmptyString(cred.DomainID, cred.UserID, file.Name, file.AuthIndex)
	if identity == "" {
		return "", fmt.Errorf("账号缺少稳定标识，无法记录每日领取")
	}
	hash := sha256.Sum256([]byte(strings.TrimRight(cfg.BenefitGatewayURL, "/") + "\n" + identity))
	return cfg.statePath(filepath.Dir(file.Path), fmt.Sprintf("daily-claim-%x.state", hash))
}

func claimAccount(cfg *Config, file pluginapi.HostAuthFileEntry, cred *credential, now time.Time, manual bool) (string, error) {
	local := now.In(dailyClaimZone)
	if !manual && local.Hour() == 0 && local.Minute() < 5 {
		return "skipped", nil
	}
	path, err := dailyClaimPath(cfg, file, cred)
	if err != nil {
		return "", err
	}
	lock, _ := dailyClaimLocks.LoadOrStore(path, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	var state dailyClaimState
	err = readPluginState(path, &state)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err == nil {
		_, dateErr := time.Parse("2006-01-02", state.Day)
		if state.Version != 1 || dateErr != nil || state.Attempts < 1 || state.Attempts > 6 || state.LastAttempt.IsZero() {
			return "", fmt.Errorf("每日领取记录无效，未发送请求")
		}
	}
	day := local.Format("2006-01-02")
	if state.Day > day {
		return "", fmt.Errorf("系统日期早于领取记录，未发送请求")
	}
	if state.Day != day {
		state = dailyClaimState{Version: 1, Day: day}
	}
	if state.Accepted {
		return "already", nil
	}
	if state.Attempts >= 6 || (!state.LastAttempt.IsZero() && now.Sub(state.LastAttempt) < 10*time.Minute) {
		return "skipped", nil
	}
	state.Attempts++
	state.LastAttempt = now
	// Persist the attempt BEFORE any network I/O. Broken/read-only storage
	// fails closed, and an interrupted request cannot produce a tight retry loop.
	if err = savePluginState(path, state); err != nil {
		return "", err
	}
	cred, err = prepareCredentialForUse(file.AuthIndex, cred)
	if err != nil {
		return "", fmt.Errorf("领取前刷新账号凭证失败，请检查登录状态")
	}
	if err = requestDailyClaim(cfg, cred); err != nil {
		return "", err
	}
	state.Accepted = true
	if err = savePluginState(path, state); err != nil {
		return "", fmt.Errorf("上游已接受领取，但本地保存失败；请检查状态目录")
	}
	quotas.mu.Lock()
	if snapshot, ok := quotas.snapshots[file.AuthIndex]; ok {
		snapshot.Benefit, snapshot.BenefitError, snapshot.BenefitFetchedAt = nil, "", time.Time{}
		quotas.snapshots[file.AuthIndex] = snapshot
	}
	quotas.mu.Unlock()
	return "accepted", nil
}

// Matched to the official 26.9.101 capture: POST with a genuinely EMPTY body,
// no regional X-Domain-Id, and precisely these four signed headers.
func requestDailyClaim(cfg *Config, cred *credential) error {
	if cfg.APIMode == "native" || strings.TrimSpace(cfg.BenefitGatewayURL) == "" {
		return fmt.Errorf("每日领取需要 agent 模式和福利网关")
	}
	if !cred.valid() {
		return fmt.Errorf("每日领取需要有效账号凭证")
	}
	endpoint := strings.TrimRight(cfg.BenefitGatewayURL, "/") + epDailyClaim
	copyCred := *cred
	copyCred.DomainID = ""
	headers, err := signRequest(http.MethodPost, endpoint, map[string]string{"Content-Type": "application/json"}, nil, &copyCred, true)
	if err != nil {
		return fmt.Errorf("每日领取签名失败")
	}
	// Cron has no cancellable callback ID in CPA's ABI. Use the host transport
	// and keep this off chat/quota paths; overlap guards cover slow requests.
	resp, err := hostHTTPDo(http.MethodPost, endpoint, headers, nil)
	if err != nil {
		return fmt.Errorf("每日领取请求失败，请检查网络或登录状态")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("每日领取返回 HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		Code   string `json:"error_code"`
		Result *struct {
			Channel string `json:"channel"`
		} `json:"result"`
	}
	if json.Unmarshal(resp.Body, &envelope) != nil || envelope.Code != "0000" || envelope.Result == nil || envelope.Result.Channel != "codearts" {
		return fmt.Errorf("每日领取未返回有效成功结果，请在官方页面检查领取资格")
	}
	return nil
}

func runDailyClaimSweep(task ScheduleTask, now func() time.Time, manual bool) error {
	cfg := config()
	if !manual && (!cfg.DailyClaim.Enabled || !cfg.Schedule.Enabled || cfg.schedulePending || cfg.scheduleStateError != "") {
		return nil
	}
	if cfg.APIMode == "native" {
		return fmt.Errorf("每日领取仅支持 agent 模式")
	}
	files, err := hostAuthList()
	if err != nil {
		return fmt.Errorf("读取账号列表失败")
	}
	accepted, already, skipped, failed := 0, 0, 0, 0
	seen := map[string]bool{}
	var lastErr error
	for _, file := range files {
		if file.Disabled || (normalizeProvider(file.Provider) != providerID && normalizeProvider(file.Type) != providerID) {
			continue
		}
		storage, err := hostAuthGet(file.AuthIndex)
		if err != nil {
			failed++
			lastErr = fmt.Errorf("读取账号凭证失败")
			continue
		}
		cred, err := credentialFromStorage(storage)
		if err != nil || !cred.valid() {
			failed++
			lastErr = fmt.Errorf("账号凭证无效")
			continue
		}
		path, err := dailyClaimPath(cfg, file, cred)
		if err != nil {
			failed++
			lastErr = err
			continue
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		result, err := claimAccount(cfg, file, cred, now(), manual)
		if err != nil {
			failed++
			lastErr = err
			continue
		}
		switch result {
		case "accepted":
			accepted++
		case "already":
			already++
		default:
			skipped++
		}
	}
	result := fmt.Sprintf("领取已受理 %d，今日已领取 %d，等待重试/日期窗口 %d，失败 %d；余额请单独刷新", accepted, already, skipped, failed)
	if len(seen) == 0 && failed == 0 {
		return fmt.Errorf("没有可领取的 CodeArts 账号")
	}
	if failed > 0 {
		return fmt.Errorf("%s：%v", result, lastErr)
	}
	recordTaskResult(task.ID, result)
	return nil
}
