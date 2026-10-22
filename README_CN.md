# CPA Smart Scheduler Plugin (cpa-plugin-smart-scheduler)

基于 [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 原生 C ABI 接口构建的**进程内智能首字延迟（TTFT）监控与高级调度熔断器插件**。

融合了现代分布式网关与大模型网关顶尖的 **P2C (Power of Two Choices) + Peak-EWMA 最低延迟负载均衡** 以及 **Half-Open 金丝雀单笔试探熔断机制**，在 Go 内存中实现单请求级的微秒级自适应分发。

> 📖 **完整算法与全生命周期状态机设计文档**：请参阅 [SCHEDULING_LOGIC.md](SCHEDULING_LOGIC.md)。

---

## 核心架构与调度算法（v0.3.1）

### 1. P2C (Power of Two Choices) + Peak-EWMA 最低延迟路由（轻度慢绝不静默，自然降权）
- **核心原则**：轻度首字变慢（如 20s）绝不强行关冷宫，号池容量绝不缩水。
- **算法实现**：
  每笔请求到达时，插件从健康候选池中随机抽取两个候选账号 $A$ 和 $B$，计算实时综合代价：
  $$\text{Cost} = \text{EWMA\_TTFT} \times (\text{InFlight} + 1)$$
  - $\text{EWMA\_TTFT}$：该账号近期首字延迟的平滑加权均值（$\alpha = 0.3$）；
  - $\text{InFlight}$：该账号当前正在处理中的活跃并发请求数；
  - **决策**：选择代价较小的账号派发，兼顾了“谁当前延迟最低”与“谁当前最空闲”。慢号自然吃不到主力流量，高并发时自动充当泄洪兜底。

### 2. 精准分诊与 503 `MODEL_CAPACITY_EXHAUSTED` 豁免
- 当 Google 返回 `503 MODEL_CAPACITY_EXHAUSTED`（上游机房集群瞬时满载），**判定账号完全无罪**，绝不熔断、不累积惩罚，动态提取 Google RetryInfo 的 retryDelay（如 23s）精准避让，绝不多等一秒，彻底杜绝全网误杀。
- 404 NOT_FOUND（未授权模型号池遍历）彻底免疫，绝不计入故障。

### 3. 恶性故障 5 小时深度熔断
- 只有触发**真·恶性故障**（单笔首字 $\ge 90\text{s}$ 或 429 配额耗竭或 $\ge 90\text{s}$ 超时断连）时，才触发熔断，隔离 **300 分钟（5 小时）**，完整熬死 Google 5 小时滚动配额。

### 4. Half-Open 金丝雀单笔试水与升级惩罚
```text
[Healthy 正常主力] ── 触发极端慢(≥90s)/429 ──> [CoolingDown 静默禁闭 5小时]
        ▲                                                │
        │ 单笔试水成功 (首字<25s且成功)                     │ 满 5 小时
        │                                                ▼
        └─────────────────────────────────────── [Canary 金丝雀试水态]
                                                         │
                                                         │ 单笔试水失败 / 依然慢(≥25s)
                                                         ▼
                                          [CoolingDown 惩罚升级 10 小时]
```
- **纯被动金丝雀**：坐满 5 小时后，状态变为 `Canary`，**严格只放行 1 笔真实请求试水**（零后台探活包）；
- **试水转正**：首字 $< 25\text{s}$ 且成功，立即转正 `Healthy`；
- **试水失败重惩**：若试水依然卡死或 429，**惩罚升级翻倍至 10 小时（600 分钟）**；失败的这笔请求由网关底层 `request-retry` 自动换下一个健康账号无缝接力。

### 5. 动态保底容量与长期自愈
- 任何极端恶劣情况下，自动保留至少 30%（或至少 3 个）账号在线，绝对防止号池被全员降空。
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
      slow_avg_ms: 25000           # EWMA 首字延迟慢判定线（毫秒），用于 P2C 降权，默认 25s
      hard_max_ms: 90000           # 单笔极端慢硬熔断阈值（毫秒），默认 90s
      cooldown_minutes: 300        # 恶性慢/429/超时单笔熔断静默时长（分钟），默认 300 (5小时)
      canary_fail_minutes: 600     # 金丝雀试水失败升级惩罚时长（分钟），默认 600 (10小时)
      min_active_ratio_percent: 30 # 最低在线账号保底比例（%），默认 30%
```

---

## 插件状态实时诊断 API

插件挂载在 CLIProxyAPI 内部管理接口下：

```bash
curl -H "Authorization: Bearer <MANAGEMENT_KEY>" \
  http://localhost:8317/v0/management/plugins/cpa-plugin-smart-scheduler/status
```

---

## 开源协议

本项目采用 [MIT License](LICENSE) 开源协议。
