package main

/*
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
	void *data;
	size_t len;
	size_t cap;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(const char *op, const uint8_t *in, size_t in_len, cliproxy_buffer *out);
typedef void (*cliproxy_plugin_free_fn)(void *ptr, size_t size);
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
	pluginName                    = "cpa-plugin-smart-scheduler"
	pluginVersion                 = "0.4.0"
	defaultConsecutiveFailures    = 3      // Envoy standard consecutive gateway failures
	defaultBaseEjectionMinutes    = 300    // 300m (5 hours), Google Gemini quota window
	defaultMaxEjectionMinutes     = 600    // 600m (10 hours), maximum backoff cap
	defaultPanicThresholdPercent  = 30     // Envoy healthy_panic_threshold (minimum 30% active accounts)
	defaultMinActive              = 3      // Minimum active accounts floor
	defaultEWMAAlpha              = 0.3    // Exponential smoothing factor for TTFT
)

type AccountStatus string

const (
	StatusHealthy     AccountStatus = "healthy"
	StatusCoolingDown AccountStatus = "cooling_down"
	StatusCanary      AccountStatus = "canary" // Half-open probing state
)

type Config struct {
	Enabled                    bool    `yaml:"enabled"`
	Priority                   int     `yaml:"priority"`
	ConsecutiveGatewayFailures int     `yaml:"consecutive_gateway_failures"`
	BaseEjectionMinutes        int     `yaml:"base_ejection_minutes"`
	MaxEjectionMinutes         int     `yaml:"max_ejection_minutes"`
	PanicThresholdPercent      int     `yaml:"panic_threshold_percent"`
	EWMAAlpha                  float64 `yaml:"ewma_alpha"`
}

func defaultConfig() Config {
	return Config{
		Enabled:                    true,
		Priority:                   1,
		ConsecutiveGatewayFailures: defaultConsecutiveFailures,
		BaseEjectionMinutes:        defaultBaseEjectionMinutes,
		MaxEjectionMinutes:         defaultMaxEjectionMinutes,
		PanicThresholdPercent:      defaultPanicThresholdPercent,
		EWMAAlpha:                  defaultEWMAAlpha,
	}
}

var currentConfig atomic.Value

type AccountHealth struct {
	AuthID          string        `json:"auth_id"`
	Status          AccountStatus `json:"status"` // "healthy", "cooling_down", "canary"
	InFlight        int64         `json:"in_flight"`
	EWMATTFTMs      float64       `json:"ewma_ttft_ms"`
	LastTTFTMs      int64         `json:"last_ttft_ms"`
	TotalRequests   int64         `json:"total_requests"`
	TotalFails      int64         `json:"total_fails"`
	ConsecutiveFail int           `json:"consecutive_fails"`
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
	plugin.call = (C.cliproxy_plugin_call_fn)(C.cliproxyPluginCall)
	plugin.free_buffer = (C.cliproxy_plugin_free_fn)(C.cliproxyPluginFree)
	plugin.shutdown = (C.cliproxy_plugin_shutdown_fn)(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(cOp *C.char, inPtr *C.uint8_t, inLen C.size_t, outBuf *C.cliproxy_buffer) C.int {
	if outBuf == nil {
		return 1
	}
	op := C.GoString(cOp)
	var in []byte
	if inLen > 0 && inPtr != nil {
		in = C.GoBytes(unsafe.Pointer(inPtr), C.int(inLen))
	}

	res, err := dispatch(op, in)
	if err != nil {
		return 1
	}
	if len(res) == 0 {
		outBuf.data = nil
		outBuf.len = 0
		outBuf.cap = 0
		return 0
	}

	cData := C.CBytes(res)
	outBuf.data = cData
	outBuf.len = C.size_t(len(res))
	outBuf.cap = C.size_t(len(res))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, size C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func dispatch(op string, payload []byte) ([]byte, error) {
	switch op {
	case "plugin.register":
		return handleRegister(payload)
	case "plugin.reconfigure":
		return handleReconfigure(payload)
	case "scheduler.pick":
		return handlePickAuth(payload)
	case "usage.handle":
		return handleUsage(payload)
	case "management.register":
		return handleManagementRegister()
	case "management.handle":
		return handleManagementHTTP(payload)
	default:
		return errorEnvelope("unsupported_operation", fmt.Sprintf("unknown op: %s", op)), nil
	}
}

func okEnvelope(val any) ([]byte, error) {
	b, err := json.Marshal(val)
	if err != nil {
		return nil, err
	}
	env := envelope{OK: true, Result: b}
	return json.Marshal(env)
}

func errorEnvelope(code, msg string) []byte {
	env := envelope{
		OK:    false,
		Error: &envelopeError{Code: code, Message: msg},
	}
	b, _ := json.Marshal(env)
	return b
}

func handleRegister(payload []byte) ([]byte, error) {
	if err := applyConfig(payload); err != nil {
		return errorEnvelope("invalid_config", err.Error()), nil
	}

	reg := map[string]any{
		"schema_version": 1,
		"metadata": map[string]any{
			"Name":             "Smart TTFT & P2C Scheduler",
			"Version":          pluginVersion,
			"Author":           "yanhao98",
			"GitHubRepository": "https://github.com/yanhao98/cpa-plugin-smart-scheduler",
			"Description":      "Decoupled P2C + Peak-EWMA Traffic Router & Envoy-Standard Outlier Detection Circuit Breaker",
			"ConfigFields": []map[string]any{
				{
					"Name":        "consecutive_gateway_failures",
					"Type":        "int",
					"Default":     defaultConsecutiveFailures,
					"Description": "Envoy 标准连续网关故障熔断门限（5xx/断连），默认 3",
				},
				{
					"Name":        "base_ejection_minutes",
					"Type":        "int",
					"Default":     defaultBaseEjectionMinutes,
					"Description": "基础熔断隔离时长（分钟），默认 300 (5小时对齐 Google 配额窗口)",
				},
				{
					"Name":        "max_ejection_minutes",
					"Type":        "int",
					"Default":     defaultMaxEjectionMinutes,
					"Description": "最大熔断隔离上限时长（分钟），默认 600 (10小时)",
				},
				{
					"Name":        "panic_threshold_percent",
					"Type":        "int",
					"Default":     defaultPanicThresholdPercent,
					"Description": "Envoy 标准恐慌保底门限百分比（%），默认 30%",
				},
				{
					"Name":        "ewma_alpha",
					"Type":        "float",
					"Default":     defaultEWMAAlpha,
					"Description": "EWMA 首字延迟平滑系数，默认 0.3",
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
	if cfg.ConsecutiveGatewayFailures <= 0 {
		cfg.ConsecutiveGatewayFailures = defaultConsecutiveFailures
	}
	if cfg.BaseEjectionMinutes <= 0 {
		cfg.BaseEjectionMinutes = defaultBaseEjectionMinutes
	}
	if cfg.MaxEjectionMinutes <= 0 {
		cfg.MaxEjectionMinutes = defaultMaxEjectionMinutes
	}
	if cfg.PanicThresholdPercent <= 0 {
		cfg.PanicThresholdPercent = defaultPanicThresholdPercent
	}
	if cfg.EWMAAlpha <= 0 || cfg.EWMAAlpha > 1 {
		cfg.EWMAAlpha = defaultEWMAAlpha
	}
	currentConfig.Store(cfg)
	return nil
}

// ==========================================
// 子系统 1：P2C + Peak-EWMA 流量路由器 (Traffic Router)
// 业界标准：Twitter Finagle / Linkerd
// 纯数学连续代价优化，绝不因延迟而驱逐节点
// ==========================================

func calculatePeakEWMACost(acc *AccountHealth) float64 {
	if acc == nil {
		return 100000.0
	}
	base := acc.EWMATTFTMs
	if base <= 0 {
		base = 8000.0 // 默认初值 8s
	}
	// 在飞并发惩罚：Cost = EWMA_TTFT * (InFlight + 1)
	multiplier := float64(acc.InFlight + 1)
	if acc.Status == StatusCanary {
		// 金丝雀试水账号给予 0.9 试探倾斜系数，优先捕获单笔试水流量
		return base * 0.9 * multiplier
	}
	return base * multiplier
}

func handlePickAuth(payload []byte) ([]byte, error) {
	cfg := currentConfig.Load().(Config)
	if !cfg.Enabled {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errorEnvelope("invalid_payload", err.Error()), nil
	}
	if len(req.Candidates) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	now := time.Now()
	globalState.mu.Lock()
	defer globalState.mu.Unlock()

	var eligibleCandidates []pluginapi.SchedulerAuthCandidate
	var coolingCandidates []*AccountHealth

	for _, cand := range req.Candidates {
		acc, ok := globalState.accounts[cand.ID]
		if !ok {
			acc = &AccountHealth{
				AuthID:     cand.ID,
				Status:     StatusHealthy,
				EWMATTFTMs: 8000,
				LastSeen:   now,
			}
			globalState.accounts[cand.ID] = acc
		}

		if acc.Status == StatusCoolingDown {
			if now.After(acc.DemotedUntil) {
				// 隔离期满：自动转移至 Half-Open 金丝雀试水态
				acc.Status = StatusCanary
				acc.DemotedReason = "隔离期满，进入金丝雀单笔试水状态"
			} else {
				coolingCandidates = append(coolingCandidates, acc)
				continue
			}
		}

		if acc.Status == StatusCanary {
			// 半开状态严格保护：仅允许 1 笔试水在飞，不允许多并发冲刷
			if acc.InFlight == 0 {
				eligibleCandidates = append(eligibleCandidates, cand)
			}
			continue
		}

		if acc.Status == StatusHealthy {
			eligibleCandidates = append(eligibleCandidates, cand)
		}
	}

	// Envoy Panic Threshold (恐慌保底门限)
	// 当可用健康节点比例低于 panic_threshold_percent 时，激活 Panic 模式借调隔离中节点，保证服务永不断流
	minActive := int(math.Ceil(float64(len(req.Candidates)) * float64(cfg.PanicThresholdPercent) / 100.0))
	if minActive < defaultMinActive {
		minActive = defaultMinActive
	}
	if minActive > len(req.Candidates) {
		minActive = len(req.Candidates)
	}

	if len(eligibleCandidates) < minActive && len(coolingCandidates) > 0 {
		// 按最快解禁时间升序排序（最接近恢复的优先借调）
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

	// P2C (Power of Two Choices) 随机二选一比较代价
	var selectedID string
	if len(eligibleCandidates) == 1 {
		selectedID = eligibleCandidates[0].ID
	} else {
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

	// 原子标记在飞并发数递增
	if acc, ok := globalState.accounts[selectedID]; ok {
		acc.InFlight++
		acc.LastSeen = now
	}

	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:  selectedID,
		Handled: true,
	})
}

// ==========================================
// 子系统 2：异常点检测与熔断器 (Outlier Detection & Circuit Breaker)
// 业界标准：Envoy Outlier Detection & Martin Fowler Circuit Breaker
// 严格基于离散故障信号（429、连续5xx/断连）熔断，延迟指标纯交由 P2C 降权
// ==========================================

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
	return 30 * time.Second // 默认 30s 兜底
}

func handleUsage(payload []byte) ([]byte, error) {
	var rec pluginapi.UsageRecord
	if err := json.Unmarshal(payload, &rec); err != nil {
		return errorEnvelope("invalid_payload", err.Error()), nil
	}

	authID := strings.TrimSpace(rec.AuthID)
	if authID == "" {
		return okEnvelope(map[string]any{})
	}

	// 豁免规则 1：彻底排除 404 NOT_FOUND（未授权模型号池遍历，属于常规路由探测，绝不计入故障）
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

	// 豁免规则 2：排除 503 MODEL_CAPACITY_EXHAUSTED（Google 服务器机房集群算力瞬时满载，非账号故障）
	if rec.Failure.StatusCode == 503 && (strings.Contains(rec.Failure.Body, "MODEL_CAPACITY_EXHAUSTED") || strings.Contains(rec.Failure.Body, "No capacity")) {
		delay := parseGoogleRetryDelay(rec.Failure.Body)
		acc.DemotedUntil = now.Add(delay)
		acc.DemotedReason = fmt.Sprintf("Google机房集群算力瞬时满载(503 MODEL_CAPACITY_EXHAUSTED)，精准避让%s", delay.Round(time.Second))
		return okEnvelope(map[string]any{})
	}

	// ----------------------------------------------------
	// 熔断与恢复检验：纯粹基于二元成功/失败状态，无任何主观延迟阈值
	// ----------------------------------------------------

	// 【Half-Open 金丝雀状态检验】
	if acc.Status == StatusCanary {
		if !rec.Failed {
			// 试水成功！收到正常响应，证明上游封禁已彻底解除，立刻转正！
			acc.Status = StatusHealthy
			acc.RestoredAt = now
			acc.ConsecutiveFail = 0
			acc.DemotedUntil = time.Time{}
			acc.DemotedReason = ""
			ttftMs := rec.TTFT.Milliseconds()
			if ttftMs > 0 {
				acc.LastTTFTMs = ttftMs
				acc.EWMATTFTMs = float64(ttftMs)
			}
		} else {
			// 试水失败！明确报错，重度惩罚升级（指数退避）
			acc.TotalFails++
			acc.Flap++
			acc.Status = StatusCoolingDown
			ejectMinutes := cfg.MaxEjectionMinutes
			acc.DemotedUntil = now.Add(time.Duration(ejectMinutes) * time.Minute)
			acc.DemotedReason = fmt.Sprintf("金丝雀试水未通过(状态码 %d)，惩罚升级%dm", rec.Failure.StatusCode, ejectMinutes)
		}
		return okEnvelope(map[string]any{})
	}

	// 【正常在岗账号的故障判定】
	if rec.Failed {
		acc.TotalFails++

		// 判定一：429 配额耗竭（强隔离）
		if rec.Failure.StatusCode == 429 || strings.Contains(rec.Failure.Body, "RESOURCE_EXHAUSTED") {
			acc.Flap++
			acc.ConsecutiveFail = 0
			acc.Status = StatusCoolingDown
			acc.DemotedUntil = now.Add(time.Duration(cfg.BaseEjectionMinutes) * time.Minute)
			acc.DemotedReason = "上游429配额耗尽(RESOURCE_EXHAUSTED)，熔断隔离5h"
			return okEnvelope(map[string]any{})
		}

		// 判定二：Envoy 连续网关故障检测（5xx / 网络超时 / 连接中断）
		acc.ConsecutiveFail++
		if acc.ConsecutiveFail >= cfg.ConsecutiveGatewayFailures {
			acc.Flap++
			acc.Status = StatusCoolingDown
			acc.DemotedUntil = now.Add(time.Duration(cfg.BaseEjectionMinutes) * time.Minute)
			acc.DemotedReason = fmt.Sprintf("连续 %d 次上游网关故障(状态码 %d)，触发隔离5h", acc.ConsecutiveFail, rec.Failure.StatusCode)
			acc.ConsecutiveFail = 0
			return okEnvelope(map[string]any{})
		}
		return okEnvelope(map[string]any{})
	}

	// ----------------------------------------------------
	// 成功请求处理：重置连续故障计数，平滑更新 EWMA 延迟
	// ----------------------------------------------------
	acc.ConsecutiveFail = 0
	ttftMs := rec.TTFT.Milliseconds()
	if ttftMs > 0 {
		acc.LastTTFTMs = ttftMs
		if acc.EWMATTFTMs <= 0 {
			acc.EWMATTFTMs = float64(ttftMs)
		} else {
			alpha := cfg.EWMAAlpha
			acc.EWMATTFTMs = alpha*float64(ttftMs) + (1.0-alpha)*acc.EWMATTFTMs
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

func handleManagementHTTP(payload []byte) ([]byte, error) {
	var req struct {
		Method string `json:"method"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return errorEnvelope("invalid_payload", err.Error()), nil
	}

	if req.Method == "GET" && strings.HasSuffix(req.Path, "/status") {
		return handleManagementStatus()
	}

	return okEnvelope(map[string]any{
		"status_code": http.StatusNotFound,
		"headers": map[string]string{
			"Content-Type": "application/json",
		},
		"body": `{"error": "route_not_found"}`,
	})
}

func handleManagementStatus() ([]byte, error) {
	cfg := currentConfig.Load().(Config)
	now := time.Now()

	globalState.mu.RLock()
	defer globalState.mu.RUnlock()

	var list []*AccountHealth
	healthyCount := 0
	coolingCount := 0
	canaryCount := 0

	for _, acc := range globalState.accounts {
		clone := *acc
		if clone.Status == StatusCoolingDown && !clone.DemotedUntil.IsZero() {
			rem := clone.DemotedUntil.Sub(now).Minutes()
			if rem > 0 {
				clone.CooldownMinutes = math.Round(rem*10) / 10
			} else {
				clone.CooldownMinutes = 0
			}
		}
		list = append(list, &clone)

		switch clone.Status {
		case StatusHealthy:
			healthyCount++
		case StatusCoolingDown:
			coolingCount++
		case StatusCanary:
			canaryCount++
		}
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].EWMATTFTMs < list[j].EWMATTFTMs
	})

	resp := map[string]any{
		"plugin":  pluginName,
		"version": pluginVersion,
		"config":  cfg,
		"summary": map[string]any{
			"total_accounts":   len(list),
			"healthy_accounts": healthyCount,
			"cooling_accounts": coolingCount,
			"canary_accounts":  canaryCount,
		},
		"accounts": list,
	}

	b, _ := json.Marshal(resp)
	return okEnvelope(map[string]any{
		"status_code": http.StatusOK,
		"headers": map[string]string{
			"Content-Type": "application/json",
		},
		"body": string(b),
	})
}

func main() {}
