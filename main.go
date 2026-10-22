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
	pluginVersion     = "0.1.0"
	defaultMinSamples = 3
	defaultSlowAvgMs  = 25000 // 25s
	defaultHardMaxMs  = 90000 // 90s
	defaultBaseQuietM = 90    // 90 minutes for mild slow (flap=1)
	defaultDeepQuietM = 300   // 300 minutes (5h) for severe/repeat slow
	defaultMinActive  = 3
)

type Config struct {
	Enabled               bool `yaml:"enabled"`
	Priority              int  `yaml:"priority"`
	MinSamples            int  `yaml:"min_samples"`
	SlowAvgMs             int  `yaml:"slow_avg_ms"`
	HardMaxMs             int  `yaml:"hard_max_ms"`
	BaseQuietMinutes      int  `yaml:"base_quiet_minutes"`
	RepeatQuietMinutes    int  `yaml:"repeat_quiet_minutes"`
	MinActiveRatioPercent int  `yaml:"min_active_ratio_percent"`
}

func defaultConfig() Config {
	return Config{
		Enabled:               true,
		Priority:              1,
		MinSamples:            defaultMinSamples,
		SlowAvgMs:             defaultSlowAvgMs,
		HardMaxMs:             defaultHardMaxMs,
		BaseQuietMinutes:      defaultBaseQuietM,
		RepeatQuietMinutes:    defaultDeepQuietM,
		MinActiveRatioPercent: 30,
	}
}

var currentConfig atomic.Value

type AccountHealth struct {
	AuthID          string        `json:"auth_id"`
	RollingTTFT     []int64       `json:"rolling_ttft_ms,omitempty"`
	AvgTTFTMs       int64         `json:"avg_ttft_ms"`
	LastTTFTMs      int64         `json:"last_ttft_ms"`
	TotalRequests   int64         `json:"total_requests"`
	TotalFails      int64         `json:"total_fails"`
	DemotedUntil    time.Time     `json:"demoted_until"`
	DemotedReason   string        `json:"demoted_reason,omitempty"`
	Flap            int           `json:"flap"`
	LastSeen        time.Time     `json:"last_seen"`
	RestoredAt      time.Time     `json:"restored_at"`
	Status          string        `json:"status"` // "healthy" or "cooling_down"
	CooldownMinutes float64       `json:"cooldown_remaining_minutes,omitempty"`
}

type SchedulerState struct {
	mu            sync.RWMutex
	accounts      map[string]*AccountHealth
	roundRobinIdx uint64
}

var globalState = &SchedulerState{
	accounts: make(map[string]*AccountHealth),
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
			"Name":             "Smart TTFT Scheduler",
			"Version":          pluginVersion,
			"Author":           "yanhao98",
			"GitHubRepository": "https://github.com/yanhao98/cpa-plugin-smart-scheduler",
			"Description":      "智能首字延迟与熔断调度器：毫秒级实时统计首字延迟、排除404误判、90m/300m两段式静默退避与平滑调度。",
			"ConfigFields": []map[string]any{
				{
					"Name":        "slow_avg_ms",
					"Type":        "int",
					"Default":     defaultSlowAvgMs,
					"Description": "平均首字延迟慢判定阈值（毫秒），默认 25000",
				},
				{
					"Name":        "hard_max_ms",
					"Type":        "int",
					"Default":     defaultHardMaxMs,
					"Description": "单笔极端慢首字硬熔断阈值（毫秒），默认 90000",
				},
				{
					"Name":        "base_quiet_minutes",
					"Type":        "int",
					"Default":     defaultBaseQuietM,
					"Description": "首次轻度降级静默时长（分钟），默认 90",
				},
				{
					"Name":        "repeat_quiet_minutes",
					"Type":        "int",
					"Default":     defaultDeepQuietM,
					"Description": "重复/恶性慢降级静默时长（分钟），默认 300 (5小时)",
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

// 调度器选择逻辑
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

	// 1. 分类候选账号：健康在线 vs 冷却中
	var healthyCandidates []pluginapi.SchedulerAuthCandidate
	var coolingCandidates []*AccountHealth

	for _, cand := range req.Candidates {
		acc, exists := globalState.accounts[cand.ID]
		if !exists {
			acc = &AccountHealth{
				AuthID:   cand.ID,
				Status:   "healthy",
				LastSeen: now,
			}
			globalState.accounts[cand.ID] = acc
		}

		// 检查冷却状态
		if now.Before(acc.DemotedUntil) {
			coolingCandidates = append(coolingCandidates, acc)
		} else {
			if acc.Status == "cooling_down" {
				// 冷却期满自然恢复
				acc.Status = "healthy"
				acc.RestoredAt = now
				acc.DemotedUntil = time.Time{}
				acc.DemotedReason = ""
			}
			healthyCandidates = append(healthyCandidates, cand)
		}
	}

	// 2. 保底检查：若健康候选数少于动态保底量，唤醒最接近恢复的账号补充
	minActive := int(math.Ceil(float64(len(req.Candidates)) * float64(cfg.MinActiveRatioPercent) / 100.0))
	if minActive < defaultMinActive {
		minActive = defaultMinActive
	}
	if minActive > len(req.Candidates) {
		minActive = len(req.Candidates)
	}

	if len(healthyCandidates) < minActive && len(coolingCandidates) > 0 {
		// 按冷却截止时间升序排序（最快恢复的排前）
		sort.Slice(coolingCandidates, func(i, j int) bool {
			return coolingCandidates[i].DemotedUntil.Before(coolingCandidates[j].DemotedUntil)
		})

		needed := minActive - len(healthyCandidates)
		for i := 0; i < needed && i < len(coolingCandidates); i++ {
			cAcc := coolingCandidates[i]
			for _, cand := range req.Candidates {
				if cand.ID == cAcc.AuthID {
					healthyCandidates = append(healthyCandidates, cand)
					break
				}
			}
		}
	}

	if len(healthyCandidates) == 0 {
		// 极端兜底，让宿主默认逻辑处理
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	// 3. 智能选择：在健康候选者中，按平滑轮询（Round-Robin）分发
	idx := atomic.AddUint64(&globalState.roundRobinIdx, 1)
	selected := healthyCandidates[idx%uint64(len(healthyCandidates))]

	if acc, ok := globalState.accounts[selected.ID]; ok {
		acc.LastSeen = now
	}

	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:  selected.ID,
		Handled: true,
	})
}

// 接收用量及延迟事件
func handleUsage(payload []byte) ([]byte, error) {
	var rec pluginapi.UsageRecord
	if err := json.Unmarshal(payload, &rec); err != nil {
		return errorEnvelope("invalid_payload", err.Error()), nil
	}

	authID := strings.TrimSpace(rec.AuthID)
	if authID == "" {
		return okEnvelope(map[string]any{})
	}

	// 核心过滤：排除 404 NOT_FOUND（模型未授权遍历不是账号故障）
	if rec.Failure.StatusCode == 404 {
		return okEnvelope(map[string]any{})
	}

	cfg := currentConfig.Load().(Config)
	now := time.Now()

	globalState.mu.Lock()
	defer globalState.mu.Unlock()

	acc, exists := globalState.accounts[authID]
	if !exists {
		acc = &AccountHealth{
			AuthID:   authID,
			Status:   "healthy",
			LastSeen: now,
		}
		globalState.accounts[authID] = acc
	}
	acc.TotalRequests++
	acc.LastSeen = now

	// 自愈机制：若账号稳定处于健康态满 12 小时且零报错，重置 flap
	if acc.Status == "healthy" && !acc.RestoredAt.IsZero() && now.Sub(acc.RestoredAt) >= 12*time.Hour {
		acc.Flap = 0
	}

	ttftMs := rec.TTFT.Milliseconds()
	acc.LastTTFTMs = ttftMs

	// 1. 单笔极端慢硬熔断 (>= 90s) 或严重系统失败 (504/499)
	isHardSingle := ttftMs >= int64(cfg.HardMaxMs)
	isSevereFail := rec.Failed && (rec.Failure.StatusCode == 429 || rec.Failure.StatusCode >= 500 || rec.Latency >= time.Duration(cfg.HardMaxMs)*time.Millisecond)

	if isHardSingle || isSevereFail {
		acc.TotalFails++
		acc.Flap++
		quietMinutes := cfg.RepeatQuietMinutes // 300m (5h)
		acc.Status = "cooling_down"
		acc.DemotedUntil = now.Add(time.Duration(quietMinutes) * time.Minute)
		if isHardSingle {
			acc.DemotedReason = fmt.Sprintf("单笔极端慢首字 %dms(>=%dms)", ttftMs, cfg.HardMaxMs)
		} else {
			acc.DemotedReason = fmt.Sprintf("严重上游失败(状态码 %d)", rec.Failure.StatusCode)
		}
		return okEnvelope(map[string]any{})
	}

	if rec.Failed {
		acc.TotalFails++
		return okEnvelope(map[string]any{})
	}

	// 2. 正常成功请求，记录滚动 TTFT
	if ttftMs > 0 {
		acc.RollingTTFT = append(acc.RollingTTFT, ttftMs)
		if len(acc.RollingTTFT) > 20 {
			acc.RollingTTFT = acc.RollingTTFT[len(acc.RollingTTFT)-20:]
		}

		var sum int64
		for _, v := range acc.RollingTTFT {
			sum += v
		}
		acc.AvgTTFTMs = sum / int64(len(acc.RollingTTFT))

		// 检查平均耗时是否触发轻度慢降级 (>= 25s)
		if len(acc.RollingTTFT) >= cfg.MinSamples && acc.AvgTTFTMs >= int64(cfg.SlowAvgMs) {
			acc.Flap++
			quietMinutes := cfg.BaseQuietMinutes // 90m
			if acc.Flap >= 2 {
				quietMinutes = cfg.RepeatQuietMinutes // 300m
			}
			acc.Status = "cooling_down"
			acc.DemotedUntil = now.Add(time.Duration(quietMinutes) * time.Minute)
			acc.DemotedReason = fmt.Sprintf("平均首字 %dms(>=%dms), flap=%d", acc.AvgTTFTMs, cfg.SlowAvgMs, acc.Flap)
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

// 管理路由响应
func handleManagementHandle(payload []byte) ([]byte, error) {
	now := time.Now()
	globalState.mu.RLock()
	defer globalState.mu.RUnlock()

	list := make([]AccountHealth, 0, len(globalState.accounts))
	for _, acc := range globalState.accounts {
		item := *acc
		if now.Before(item.DemotedUntil) {
			item.Status = "cooling_down"
			item.CooldownMinutes = math.Round(item.DemotedUntil.Sub(now).Minutes()*10) / 10
		} else {
			item.Status = "healthy"
			item.CooldownMinutes = 0
		}
		list = append(list, item)
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].AvgTTFTMs > list[j].AvgTTFTMs
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
