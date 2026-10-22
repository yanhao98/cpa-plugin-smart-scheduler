# CPA Smart Scheduler Plugin (cpa-plugin-smart-scheduler)

基于 [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 原生 C ABI 接口构建的**进程内智能首字延迟（TTFT）监控与高级调度熔断器插件**。

融合了微服务领域与大模型网关顶尖的 **P2C (Power of Two Choices) + Peak-EWMA 最低延迟负载均衡** 以及 **Half-Open 金丝雀单笔试探熔断机制**，彻底告别外部脚本轮询改 priority 的滞后模式，在 Go 内存中实现单请求级的极致自适应分发。

---

## 核心架构与调度算法

### 1. P2C (Power of Two Choices) + Peak-EWMA 最低延迟路由
- **痛点**：盲目轮询会把并发请求连续灌给正在思考的账号，引发 Google 并发排队；单纯挑“最快的一个号”又会导致惊群效应（Thundering Herd）。
- **算法实现**：
  每笔请求到达时，插件从健康候选池中随机抽取两个候选账号 $A$ 和 $B$，计算实时综合代价：
  $$\text{Cost} = \text{EWMA\_TTFT} \times (\text{InFlight} + 1)$$
  - $\text{EWMA\_TTFT}$：该账号近期首字延迟的平滑加权均值（$\alpha = 0.3$）；
  - $\text{InFlight}$：该账号当前正在处理中的活跃并发请求数；
  - **决策**：选择代价较小的账号派发，兼顾了“谁当前延迟最低”与“谁当前最空闲”。

### 2. Half-Open 金丝雀单笔试水探活（彻底杜绝慢号复发冲垮）
- **痛点**：冷却时间一到全量放行，若上游慢队列未解除，真实并发会瞬间被连续超时冲垮。
- **机制流程**：
  ```text
  [Healthy 正常在岗] ── 严重超时/单笔≥90s ──> [CoolingDown 静默禁闭 90m/300m]
          ▲                                             │
          │ 单笔试水成功 (首字<25s)                       │ 冷却期满
          │                                             ▼
          └───────────────────────────────────── [Canary 金丝雀试水态]
                                                        │
                                                        │ 单笔试水失败/依然慢
                                                        ▼
                                         [CoolingDown 重新熔断惩罚 5h]
  ```
  - 账号在静默期满后，**不直接全量放回**，而是转入 `Canary`（试水）状态；
  - 调度器**严格只放行 1 笔请求**打给它试水；在此期间所有其他正常流量全部绕开；
  - 试水成功（首字 $< 25\text{s}$ 且成功） $\rightarrow$ 瞬间转正为 `Healthy`，全量接客；
  - 试水失败 $\rightarrow$ 立即打回 300 分钟冷宫。**全局有且仅有这 1 笔试水请求受影响，全池其他用户被 100% 保护**。

### 3. 404 NOT_FOUND 遍历免疫
- 彻底过滤未授权模型池遍历产生的 404 错误，外部调用不存在的模型时绝不会误伤号池。

### 4. 动态保底容量防御
- 任何极端恶劣情况下，自动保留至少 30%（或至少 3 个）账号在线，绝对防止号池被全员降空。

### 5. 长期健康自愈（Flap Decay）
- 账号在健康状态下稳定运行满 12 小时且零报错，历史抖动惩罚计数自动归零。

---

## 配置说明

在 CLIProxyAPI 的 `config.yaml` 中配置启用：

```yaml
plugins:
  enabled: true       # 开启全局插件支持
  dir: "plugins"      # 插件目录
  configs:
    cpa-plugin-smart-scheduler:
      enabled: true
      priority: 1
      # 可选微调参数（均内置最佳实践默认值）：
      slow_avg_ms: 25000         # EWMA 首字慢判定线（毫秒），默认 25s
      hard_max_ms: 90000         # 单笔极端慢硬熔断阈值（毫秒），默认 90s
      base_quiet_minutes: 90     # 首次轻度慢静默时长（分钟），默认 90
      repeat_quiet_minutes: 300  # 重度/重复慢静默时长（分钟），默认 300 (5小时)
      min_active_ratio_percent: 30 # 最低在线账号保底比例（%），默认 30%
```

---

## 插件状态实时诊断 API

插件挂载在 CLIProxyAPI 内部管理接口下：

```bash
curl -H "Authorization: Bearer <MANAGEMENT_KEY>" \
  http://localhost:8317/v0/management/plugins/cpa-plugin-smart-scheduler/status
```

**响应示例**：
```json
{
  "time": "2026-10-22T21:15:00+08:00",
  "accounts": [
    {
      "auth_id": "antigravity-aronowoberdick@gmail.com.json",
      "status": "healthy",
      "in_flight": 0,
      "ewma_ttft_ms": 7800.5,
      "avg_ttft_ms": 7801,
      "last_ttft_ms": 8100,
      "total_requests": 352,
      "total_fails": 0
    },
    {
      "auth_id": "antigravity-baolamle721@gmail.com.json",
      "status": "canary",
      "in_flight": 1,
      "demoted_reason": "冷却期满，进入金丝雀单笔试水状态",
      "total_requests": 45,
      "total_fails": 2
    },
    {
      "auth_id": "antigravity-diazgilson542@gmail.com.json",
      "status": "cooling_down",
      "in_flight": 0,
      "demoted_reason": "单笔极端慢首字 105000ms(>=90000ms)",
      "cooldown_remaining_minutes": 142.3,
      "total_requests": 68,
      "total_fails": 5
    }
  ]
}
```

---

## 开源协议

本项目采用 [MIT License](LICENSE) 开源协议。
