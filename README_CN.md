# CPA 智能调度插件（cpa-plugin-smart-scheduler）

基于 [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 原生 C ABI 接口构建的**专精流式首字（TTFT）与非流式最小连接数（Least Connections）双轨流量路由及 Envoy 标准异常点熔断器插件**。

融合了现代分布式网关顶尖的 **按模型隔离与专精流式首字的 P2C (Power of Two Choices) + Peak-EWMA 最低延迟负载均衡** 以及 **Envoy 标准 Outlier Detection 异常点熔断与半开金丝雀试探机制**，彻底清除任何人为硬编码常数，在 Go 内存中实现微秒级自适应分发。

> 📖 **完整算法与全生命周期状态机设计文档**：请参阅 [SCHEDULING_LOGIC.md](SCHEDULING_LOGIC.md)。

---

## 核心架构与设计原则（v0.5.1）

### 1. 流式/非流式双轨制路由（Stream-Specialized Dual Track）
- **物理本质**：大模型总耗时为 $\text{Prefill Time} + (\text{Output Tokens} \times \text{TPOT})$。非流式请求耗时（0.5s ~ 25s）受用户要求输出的文本长度决定，**绝不能代表节点真实延迟**；
- **流式请求（`Stream = true`）**：专精纯净的 **TTFT 首字延迟**，不受输出长度影响，代入 P2C Peak-EWMA 代价公式精确避让慢节点；
- **非流式请求（`Stream = false`）**：**绝不更新 EWMA 首字账本**，彻底杜绝长文生成污染延迟基准；调度时纯粹比对活跃并发数 `(InFlight + 1)`，自动退化为工业级**最小连接数（Least Connections）调度**。

### 2. 彻底去除人为硬编码常数（Zero Magic Numbers）
- **无固定初始延迟**：彻底废除任何硬编码的固定数字（如 8000ms），绝不因拍脑袋预估引发冷启动饥饿（Cold-Start Starvation）或羊群踩踏（Stampede）；
- **动态集群均值（Dynamic Fleet Mean）**：新号冷启动时，动态继承当前号池在该模型上的真实平均首字延迟（$\text{Cost} = \text{FleetMean} \times 0.8$），赋予无害探索偏置；
- **全冷启动中性基准**：若全池均未调用过该模型，所有账号基准完全一致（1.0），Cost 纯粹取决于活跃并发数 `(InFlight + 1)`，平滑退化为均匀随机 / 最小连接数探索；
- **首包直接覆盖（First-Sample 100% Replacement）**：拿到第一笔真实流式响应耗时后，100% 直接替换初始基准，严禁与假种子掺水混合。

### 3. 按模型严格隔离延迟度量（Per-Model Latency Isolation）
- **模型级独立度量**：每个账号内部维护 `models: map[string]*ModelMetrics`，各模型独立记录首字延迟、平滑加权与请求计数；
- **精确路由比对**：P2C 比对时，仅抽取两候选节点在**当前请求目标模型**上的综合代价进行择优：
  ```text
  Cost(a, M, stream) = BaseScore(a, M, stream) * (InFlight(a) + 1)
  ```

### 4. Envoy 标准异常点熔断（Outlier Detection）
熔断器纯粹基于离散故障信号做出响应，流式与非流式一视同仁严格拦截：
- **429 配额耗竭**：立即隔离 300 分钟（5 小时），精准覆盖 Google 滚动配额恢复周期；
- **连续网关故障**：连续 3 次遇到 5xx 或网络断连，触发隔离 5 小时；单次偶发抖动不予惩罚。

### 5. 上游语义感知与 503 机房满载豁免
- 当 Google 返回 `503 MODEL_CAPACITY_EXHAUSTED` 时，插件自动动态提取响应体中 `google.rpc.RetryInfo.retryDelay` 字段（如 `"23s"`）进行精准避让；
- **判定账号无过错**：不记入故障，不累积惩罚，避让结束后自动无缝恢复，彻底杜绝全网误杀；
- 404 NOT_FOUND（未授权模型号池遍历）彻底免疫，零惩罚。

### 6. Half-Open 金丝雀单笔试水与升级惩罚
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

### 7. Envoy 恐慌保底门限（Panic Threshold）
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
