# CPA 智能调度插件（cpa-plugin-smart-scheduler）

基于 [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 原生 C ABI 接口构建的**进程内智能流量路由与 Envoy 标准异常点熔断器插件**。

融合了现代分布式网关与大模型网关顶尖的 **P2C (Power of Two Choices) + Peak-EWMA 最低延迟负载均衡** 以及 **Envoy 标准 Outlier Detection 异常点熔断与半开金丝雀试探机制**，在 Go 内存中实现单请求级的微秒级自适应分发。

> 📖 **完整算法与全生命周期状态机设计文档**：请参阅 [SCHEDULING_LOGIC.md](SCHEDULING_LOGIC.md)。

---

## 核心架构与设计原则（v0.4.0）

### 1. 彻底解耦“连续延迟路由”与“离散故障熔断”
- **业界实践**：在 LLM 场景下，由于 Prompt 长度与思考输出（Reasoning Tokens）差异极大，首字延迟（TTFT）在 2s ~ 30s 均属正常业务请求。**本系统绝不因“延迟变慢”将账号熔断隔离**。
- **P2C + Peak-EWMA 流量路由**：
  每笔请求到达时，插件从健康候选池中随机抽取两个候选账号 $A$ 和 $B$，计算实时综合代价：
  ```text
  Cost = EWMA_TTFT * (InFlight + 1)
  ```
  - `EWMA_TTFT`：该账号近期首字延迟的平滑加权均值（$\alpha = 0.3$）；
  - `InFlight`：该账号当前正在处理中的活跃并发请求数；
  - **决策**：选择代价较小的账号派发。优质空闲账号优先吃单；当并发打满时，乘数自然推高代价，促使流量平滑溢出到其他空闲账号，兼具低延迟与高吞吐。

### 2. Envoy 标准异常点熔断（Outlier Detection）
熔断器纯粹基于离散故障信号做出响应，不依赖任何主观延迟阈值：
- **429 配额耗竭**：立即隔离 300 分钟（5 小时），精准覆盖 Google 滚动配额恢复周期；
- **连续网关故障**：连续 3 次遇到 5xx 或网络断连，触发隔离 5 小时；单次偶发抖动不予惩罚。

### 3. 上游语义感知与 503 机房满载豁免
- 当 Google 返回 `503 MODEL_CAPACITY_EXHAUSTED` 时，插件自动动态提取响应体中 `google.rpc.RetryInfo.retryDelay` 字段（如 `"23s"`）进行精准避让；
- **判定账号无过错**：不记入故障，不累积惩罚，避让结束后自动无缝恢复，彻底杜绝全网误杀。
- 404 NOT_FOUND（未授权模型号池遍历）彻底免疫，零惩罚。

### 4. Half-Open 金丝雀单笔试水与升级惩罚
```text
[Healthy 正常主力] ── 触发 429 / 连续 3 次故障 ──> [CoolingDown 隔离冷却 5 小时]
        ▲                                                │
        │ 单笔试水成功 (HTTP 2xx 且无报错)                 │ 满 5 小时
        │ 证明上游解除封锁，立刻转正                         ▼
        └─────────────────────────────────────── [Canary 金丝雀试水态]
                                                         │
                                                         │ 单笔试水失败 (上游再次报错)
                                                         ▼
                                          [CoolingDown 惩罚升级 10 小时]
```
- **纯被动单笔放行**：隔离期满后转为 `Canary`，严格只放行 1 笔真实请求试水（零后台探活包）；
- **二元检验准则**：只要请求成功完成（HTTP 2xx），无论耗时多少均判定解禁成功，立刻满血转正；若依然报错，触发指数退避，惩罚升级至 10 小时。

### 5. Envoy 恐慌保底门限（Panic Threshold）
- 当全池健康账号比例低于 30%（或少于 3 个）时，系统自动激活恐慌保底机制，借调最快解禁的隔离账号兜底，绝对防止号池全关导致服务雪崩。

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
      consecutive_gateway_failures: 3  # Envoy 标准连续网关故障门限（默认 3）
      base_ejection_minutes: 300       # 基准隔离时长（默认 300 分钟 / 5 小时）
      max_ejection_minutes: 600        # 最大隔离上限（默认 600 分钟 / 10 小时）
      panic_threshold_percent: 30      # Envoy 恐慌保底门限百分比（默认 30%）
      ewma_alpha: 0.3                  # EWMA 首字平滑系数（默认 0.3）
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
