package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginName        = "cpa-plugin-smart-scheduler"
	pluginVersion     = "0.3.1"
	defaultMinSamples = 3
	defaultSlowAvgMs  = 25000 // 25s
	defaultHardMaxMs  = 90000 // 90s
	defaultDeepQuietM = 300   // 300 minutes (5h) for first severe circuit break
	defaultRetryQuietM = 600  // 600 minutes (10h) for canary retry failure
	defaultMinActive  = 3
	ewmaAlpha         = 0.3   // EWMA smoothing factor (0.3 for new sample, 0.7 for historical)
)

type AccountStatus string

const (
	StatusHealthy     AccountStatus = "healthy"
	StatusCoolingDown AccountStatus = "cooling_down"
	StatusCanary      AccountStatus = "canary" // Half-open probing state
)

type Config struct {
	Enabled               bool `yaml:"enabled"`
	Priority              int  `yaml:"priority"`
	MinSamples            int  `yaml:"min_samples"`
	SlowAvgMs             int  `yaml:"slow_avg_ms"`
	HardMaxMs             int  `yaml:"hard_max_ms"`
	CooldownMinutes       int  `yaml:"cooldown_minutes"`
	CanaryFailMinutes     int  `yaml:"canary_fail_minutes"`
	MinActiveRatioPercent int  `yaml:"min_active_ratio_percent"`
}

func defaultConfig() Config {
	return Config{
		Enabled:               true,
		Priority:              1,
		MinSamples:            defaultMinSamples,
		SlowAvgMs:             defaultSlowAvgMs,
		HardMaxMs:             defaultHardMaxMs,
		CooldownMinutes:       defaultDeepQuietM,
		CanaryFailMinutes:     defaultRetryQuietM,
		MinActiveRatioPercent: 30,
	}
}

var currentConfig atomic.Value

type AccountHealth struct {
	AuthID          string        `json:"auth_id"`
	Status          AccountStatus `json:"status"` // "healthy", "cooling_down", "canary"
	InFlight        int64         `json:"in_flight"`
	EWMATTFTMs      float64       `json:"ewma_ttft_ms"`
	AvgTTFTMs       int64         `json:"avg_ttft_ms"`
	LastTTFTMs      int64         `json:"last_ttft_ms"`
	TotalRequests   int64         `json:"total_requests"`
	TotalFails      int64         `json:"total_fails"`
	DemotedUntil    time.Time     `json:"demoted_until,omitempty"`
	DemotedReason   string        `json:"demoted_reason,omitempty"`
	Flap            int           `json:"flap"`
	LastSeen        time.Time     `json:"last_seen"`
	RestoredAt      time.Time     `json:"restored_at,omitempty"`
	CooldownMinutes float64       `json:"cooldown_remaining_minutes,omitempty"`
}

type SchedulerState struct {
	mu       sync.RWMutex
	accounts map[string]*AccountHealth
	rng      *rand.Rand
}

var globalState = &SchedulerState{
	accounts: make(map[string]*AccountHealth),
	rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

func init() {
	currentConfig.Store(defaultConfig())
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = 1
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}

	m := C.GoString(method)
	var reqBytes []byte
	if request != nil && requestLen > 0 {
		reqBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}

	raw, errHandle := handleMethod(m, reqBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, size C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = size
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, payload []byte) ([]byte, error) {
	switch method {
	case "plugin.register":
		return handleRegister(payload)
	case "plugin.reconfigure":
		return handleReconfigure(payload)
	case "scheduler.pick":
		return handleSchedulerPick(payload)
	case "usage.handle":
		return handleUsage(payload)
	case "management.register":
		return handleManagementRegister()
	case "management.handle":
		return handleManagementHandle(payload)
	default:
		return errorEnvelope("unknown_method", "method not supported: "+method), nil
	}
}

func handleRegister(payload []byte) ([]byte, error) {
	_ = applyConfig(payload)
	reg := map[string]any{
		"schema_version": 1,
		"metadata": map[string]any{
			"Name":             "Smart TTFT & P2C Scheduler",
			"Version":          pluginVersion,
			"Author":           "yanhao98",
			"GitHubRepository": "https://github.com/yanhao98/cpa-plugin-smart-scheduler",
			"Description":      "工业级大模型号池调度插件：P2C + Peak-EWMA 最低延迟负载均衡、Half-Open 金丝雀单笔试水探活、实时在飞并发追踪、排除404遍历、两段式 90m/300m 熔断。",
			"ConfigFields": []map[string]any{
				{
					"Name":        "slow_avg_ms",
					"Type":        "int",
					"Default":     defaultSlowAvgMs,
					"Description": "EWMA 首字延迟慢判定线（毫秒），用于 P2C 动态降权分流，默认 25000",
				},
				{
					"Name":        "hard_max_ms",
					"Type":        "int",
					"Default":     defaultHardMaxMs,
					"Description": "单笔极端慢硬熔断阈值（毫秒），默认 90000",
				},
				{
					"Name":        "cooldown_minutes",
					"Type":        "int",
					"Default":     defaultDeepQuietM,
					"Description": "恶性慢/429/超时单笔熔断静默时长（分钟），默认 300 (5小时)",
				},
				{
					"Name":        "canary_fail_minutes",
					"Type":        "int",
					"Default":     defaultRetryQuietM,
					"Description": "金丝雀试水失败惩罚时长（分钟），默认 600 (10小时)",
				},
				{
					"Name":        "min_active_ratio_percent",
					"Type":        "int",
					"Default":     30,
					"Description": "全池最小活跃保底比例（%），默认 30%",
				},
			},
		},
		"capabilities": map[string]bool{
			"scheduler":      true,
			"usage_plugin":   true,
			"management_api": true,
		},
	}
	return okEnvelope(reg)
}

func handleReconfigure(payload []byte) ([]byte, error) {
	if err := applyConfig(payload); err != nil {
		return errorEnvelope("invalid_config", err.Error()), nil
	}
	return handleRegister(payload)
}

func applyConfig(payload []byte) error {
	var req lifecycleRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return err
	}
	cfg := defaultConfig()
	if len(req.ConfigYAML) > 0 {
		if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
			return err
		}
	}
	currentConfig.Store(cfg)
	return nil
}

// 调度核心：P2C (Power of Two Choices) + Peak-EWMA + Half-Open 金丝雀探活
func handleSchedulerPick(payload []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errorEnvelope("invalid_request", err.Error()), nil
	}

	if len(req.Candidates) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	cfg := currentConfig.Load().(Config)
	if !cfg.Enabled {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	now := time.Now()
	globalState.mu.Lock()
	defer globalState.mu.Unlock()

	var eligibleCandidates []pluginapi.SchedulerAuthCandidate
	var coolingCandidates []*AccountHealth

	// 1. 状态流转与候选资格筛选
	for _, cand := range req.Candidates {
		acc, exists := globalState.accounts[cand.ID]
		if !exists {
			acc = &AccountHealth{
				AuthID:     cand.ID,
				Status:     StatusHealthy,
				EWMATTFTMs: 8000, // 初始预估 8s
				LastSeen:   now,
			}
			globalState.accounts[cand.ID] = acc
		}

		if acc.Status == StatusCoolingDown {
			if now.After(acc.DemotedUntil) {
				// 静默期满：转入 Half-Open 金丝雀试水状态！
				acc.Status = StatusCanary
				acc.DemotedReason = "冷却期满，进入金丝雀单笔试水状态"
			} else {
				coolingCandidates = append(coolingCandidates, acc)
				continue
			}
		}

		if acc.Status == StatusCanary {
			// 金丝雀状态保护：只允许放行 1 笔请求试水！
			if acc.InFlight == 0 {
				eligibleCandidates = append(eligibleCandidates, cand)
			}
			continue
		}

		// 正常健康账号
		if acc.Status == StatusHealthy {
			eligibleCandidates = append(eligibleCandidates, cand)
		}
	}

	// 2. 动态保底容量防御：防止极度恶劣情况下号池被全部降空
	minActive := int(math.Ceil(float64(len(req.Candidates)) * float64(cfg.MinActiveRatioPercent) / 100.0))
	if minActive < defaultMinActive {
		minActive = defaultMinActive
	}
	if minActive > len(req.Candidates) {
		minActive = len(req.Candidates)
	}

	if len(eligibleCandidates) < minActive && len(coolingCandidates) > 0 {
		// 按冷却截止时间升序排序（最快将要恢复的排前）
		sort.Slice(coolingCandidates, func(i, j int) bool {
			return coolingCandidates[i].DemotedUntil.Before(coolingCandidates[j].DemotedUntil)
		})

		needed := minActive - len(eligibleCandidates)
		for i := 0; i < needed && i < len(coolingCandidates); i++ {
			cAcc := coolingCandidates[i]
			for _, cand := range req.Candidates {
				if cand.ID == cAcc.AuthID {
					eligibleCandidates = append(eligibleCandidates, cand)
					break
				}
			}
		}
	}

	if len(eligibleCandidates) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	// 3. P2C (Power of Two Choices) 路由决策算法
	// 从候选池中随机抽取 2 个账号，计算 Score = EWMA_TTFT * (InFlight + 1)，选择代价更小的账号
	var selectedID string
	if len(eligibleCandidates) == 1 {
		selectedID = eligibleCandidates[0].ID
	} else {
		// 随机挑选两个不同候选者
		i := globalState.rng.Intn(len(eligibleCandidates))
		j := globalState.rng.Intn(len(eligibleCandidates) - 1)
		if j >= i {
			j++
		}
		candA := eligibleCandidates[i]
		candB := eligibleCandidates[j]

		accA := globalState.accounts[candA.ID]
		accB := globalState.accounts[candB.ID]

		costA := calculatePeakEWMACost(accA)
		costB := calculatePeakEWMACost(accB)

		if costA <= costB {
			selectedID = candA.ID
		} else {
			selectedID = candB.ID
		}
	}

	// 记录在飞并发数递增
	if acc, ok := globalState.accounts[selectedID]; ok {
		acc.InFlight++
		acc.LastSeen = now
	}

	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:  selectedID,
		Handled: true,
	})
}

// P2C 成本计算公式：Cost = EWMA_TTFT * (InFlight + 1)
func calculatePeakEWMACost(acc *AccountHealth) float64 {
	if acc == nil {
		return 100000.0
	}
	base := acc.EWMATTFTMs
	if base <= 0 {
		base = 8000.0 // 默认 8s
	}
	// 金丝雀试水账号稍微增加一点探测优先级，便于早点完成试水
	multiplier := float64(acc.InFlight + 1)
	if acc.Status == StatusCanary {
		return base * 0.9 * multiplier
	}
	return base * multiplier
}

// 动态解析 Google 上游返回的 RetryInfo 中的 retryDelay（如 "23s"）
func parseGoogleRetryDelay(body string) time.Duration {
	if body == "" {
		return 30 * time.Second
	}
	var resp struct {
		Error struct {
			Details []struct {
				Type       string `json:"@type"`
				RetryDelay string `json:"retryDelay"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err == nil {
		for _, d := range resp.Error.Details {
			if d.RetryDelay != "" {
				if dur, err := time.ParseDuration(d.RetryDelay); err == nil && dur > 0 {
					return dur
				}
			}
		}
	}
	return 30 * time.Second // 兜底 30 秒
}

// 请求流式结束/失败回调：更新 TTFT、InFlight、熔断判定
func handleUsage(payload []byte) ([]byte, error) {
	var rec pluginapi.UsageRecord
	if err := json.Unmarshal(payload, &rec); err != nil {
		return errorEnvelope("invalid_payload", err.Error()), nil
	}

	authID := strings.TrimSpace(rec.AuthID)
	if authID == "" {
		return okEnvelope(map[string]any{})
	}

	// 核心过滤：彻底排除 404 NOT_FOUND（未授权模型号池遍历绝不计入故障）
	if rec.Failure.StatusCode == 404 {
		globalState.mu.Lock()
		if acc, ok := globalState.accounts[authID]; ok {
			if acc.InFlight > 0 {
				acc.InFlight--
			}
		}
		globalState.mu.Unlock()
		return okEnvelope(map[string]any{})
	}

	cfg := currentConfig.Load().(Config)
	now := time.Now()

	globalState.mu.Lock()
	defer globalState.mu.Unlock()

	acc, exists := globalState.accounts[authID]
	if !exists {
		acc = &AccountHealth{
			AuthID:     authID,
			Status:     StatusHealthy,
			EWMATTFTMs: 8000,
			LastSeen:   now,
		}
		globalState.accounts[authID] = acc
	}

	// 在飞并发计数归还
	if acc.InFlight > 0 {
		acc.InFlight--
	}
	acc.TotalRequests++
	acc.LastSeen = now

	// 长期健康自愈：账号在 Healthy 状态下连续稳定运行满 12 小时，重置抖动计数 flap
	if acc.Status == StatusHealthy && !acc.RestoredAt.IsZero() && now.Sub(acc.RestoredAt) >= 12*time.Hour {
		acc.Flap = 0
	}

	// 核心过滤 2：排除 503 MODEL_CAPACITY_EXHAUSTED（Google 服务器机房集群算力瞬时满载，非账号故障）
	if rec.Failure.StatusCode == 503 && (strings.Contains(rec.Failure.Body, "MODEL_CAPACITY_EXHAUSTED") || strings.Contains(rec.Failure.Body, "No capacity")) {
		// 动态解析 Google 官方指示的精准冷却时长 (如 "23s")
		delay := parseGoogleRetryDelay(rec.Failure.Body)
		acc.DemotedUntil = now.Add(delay)
		acc.DemotedReason = fmt.Sprintf("Google上游集群算力瞬时满载(503 MODEL_CAPACITY_EXHAUSTED)，动态避让%s", delay.Round(time.Second))
		return okEnvelope(map[string]any{})
	}

	ttftMs := rec.TTFT.Milliseconds()
	acc.LastTTFTMs = ttftMs

	// 判定恶性故障：单笔极端慢(>=90s) 或 真429额度耗尽 或 严重超时断连(>=90s)
	isHardSingle := ttftMs >= int64(cfg.HardMaxMs)
	isSevereFail := rec.Failed && (rec.Failure.StatusCode == 429 || rec.Latency >= time.Duration(cfg.HardMaxMs)*time.Millisecond)

	// 【Half-Open 金丝雀状态专属检验】
	if acc.Status == StatusCanary {
		if !rec.Failed && ttftMs > 0 && ttftMs < int64(cfg.SlowAvgMs) {
			// 金丝雀单笔试水成功！首字 < 25s 且无报错，证明 Google 慢队列已彻底退散，满血转正！
			acc.Status = StatusHealthy
			acc.RestoredAt = now
			acc.DemotedUntil = time.Time{}
			acc.DemotedReason = ""
			acc.EWMATTFTMs = float64(ttftMs)
		} else {
			// 金丝雀试水失败或依然慢：重度退避翻倍至 10 小时 (600 分钟)！
			acc.TotalFails++
			acc.Flap++
			acc.Status = StatusCoolingDown
			acc.DemotedUntil = now.Add(time.Duration(cfg.CanaryFailMinutes) * time.Minute)
			acc.DemotedReason = fmt.Sprintf("金丝雀试水未通过(首字 %dms, 失败=%v)，惩罚升级10h", ttftMs, rec.Failed)
		}
		return okEnvelope(map[string]any{})
	}

	// 【正常在岗账号的恶性故障熔断】
	if isHardSingle || isSevereFail {
		acc.TotalFails++
		acc.Flap++
		acc.Status = StatusCoolingDown
		acc.DemotedUntil = now.Add(time.Duration(cfg.CooldownMinutes) * time.Minute)
		if isHardSingle {
			acc.DemotedReason = fmt.Sprintf("单笔极端慢首字 %dms(>=%dms)，触发5h熔断", ttftMs, cfg.HardMaxMs)
		} else {
			acc.DemotedReason = fmt.Sprintf("真429/严重超时(状态码 %d)，触发5h熔断", rec.Failure.StatusCode)
		}
		return okEnvelope(map[string]any{})
	}

	if rec.Failed {
		acc.TotalFails++
		return okEnvelope(map[string]any{})
	}

	// 【正常在岗账号的平滑首字更新】：只更新 EWMA 供 P2C 算法动态降权，绝不关冷宫！
	if ttftMs > 0 {
		if acc.EWMATTFTMs <= 0 {
			acc.EWMATTFTMs = float64(ttftMs)
		} else {
			acc.EWMATTFTMs = ewmaAlpha*float64(ttftMs) + (1-ewmaAlpha)*acc.EWMATTFTMs
		}
	}

	return okEnvelope(map[string]any{})
}

// 注册管理路由
func handleManagementRegister() ([]byte, error) {
	routes := map[string]any{
		"routes": []map[string]any{
			{
				"Method": "GET",
				"Path":   "/plugins/cpa-plugin-smart-scheduler/status",
			},
		},
	}
	return okEnvelope(routes)
}

// 管理端点：实时查看内存中每个账号的健康状态、在飞并发、EWMA延迟
func handleManagementHandle(payload []byte) ([]byte, error) {
	now := time.Now()
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()

	list := make([]AccountHealth, 0, len(globalState.accounts))
	for _, acc := range globalState.accounts {
		item := *acc
		if item.Status == StatusCoolingDown {
			if now.Before(item.DemotedUntil) {
				item.CooldownMinutes = math.Round(item.DemotedUntil.Sub(now).Minutes()*10) / 10
			} else {
				item.Status = StatusCanary
				item.CooldownMinutes = 0
			}
		}
		item.AvgTTFTMs = int64(math.Round(item.EWMATTFTMs))
		list = append(list, item)
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].AvgTTFTMs < list[j].AvgTTFTMs
	})

	data, _ := json.Marshal(map[string]any{
		"time":     now.Format(time.RFC3339),
		"accounts": list,
	})

	resp := map[string]any{
		"status_code": http.StatusOK,
		"headers": map[string]string{
			"Content-Type": "application/json",
		},
		"body": string(data),
	}
	return okEnvelope(resp)
}

func okEnvelope(result any) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: json.RawMessage(raw)})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

func main() {}
