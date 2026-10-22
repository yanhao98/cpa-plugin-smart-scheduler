# CPA Smart Scheduler Plugin (cpa-plugin-smart-scheduler)

基于 [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 原生 C ABI 接口构建的**进程内智能首字延迟（TTFT）监控与熔断调度器插件**。

专门解决大模型号池（特别是 Google Antigravity / Gemini Pro 等）遇到的**暗降级排队、百秒长延迟、反复放回假死号、以及未授权模型遍历 404 误判**等核心痛点。

---

## 核心特性

- **进程内毫秒级实时监控**：
  基于原生 `usage_plugin`（`usage.handle`），在每次请求流式返回的瞬间纳秒级提取真实 `TTFT`（首字延迟），完全不需要外部脚本定时轮询。
- **动态两段式熔断惩罚**：
  - **轻度慢（90 分钟静默）**：近 20 笔滚动首字均值 $\ge 25\text{s}$ 时，隔离冷却 90 分钟（平稳避开瞬时 QPS 挤压）；
  - **重度慢 / 恶性故障（5 小时静默）**：单笔首字 $\ge 90\text{s}$ 或 504/499 超时断连时，一票否决隔离 300 分钟（完整熬过 Google 的 5 小时滚动配额限制）；
- **排除 404 遍历误判**：
  精准过滤模型未授权遍历产生的 404 NOT_FOUND 错误，彻底杜绝外部发起不存在模型时导致的“全池挨个背锅降级”。
- **动态保底容量防御**：
  内置动态保底规则（默认保留 $\ge 30\%$ 活跃号），任何极端情况下都不会把号池降空，死守业务可用性。
- **健康衰减自愈机制**：
  账号恢复后持续平稳运行满 12 小时且零报错，历史抖动计数（`flap`）自动归零，恢复轻度退避资格。
- **内置诊断管理接口**：
  自带 `/v0/management/plugins/cpa-plugin-smart-scheduler/status` 端点，直观输出所有账号在内存中的实时首字延迟、冷却剩余倒计时与健康状态。

---

## 安装与部署

### 方式 1：手动安装（推荐）

1. 从 [Releases 页面](https://github.com/yanhao98/cpa-plugin-smart-scheduler/releases) 下载适合你平台的 `.zip` 压缩包（如 `linux_amd64.zip`）；
2. 解压出 `cpa-plugin-smart-scheduler.so`；
3. 将 `.so` 文件放入 CLIProxyAPI 容器挂载的 `plugins/` 目录；
4. 在 `config.yaml` 中配置启用即可。

### 方式 2：自行编译

```bash
git clone https://github.com/yanhao98/cpa-plugin-smart-scheduler.git
cd cpa-plugin-smart-scheduler
make build
# 生成 cpa-plugin-smart-scheduler.so
```

---

## 配置说明

在 CLIProxyAPI 的 `config.yaml` 中添加以下配置：

```yaml
plugins:
  enabled: true       # 开启全局插件功能
  dir: "plugins"      # 插件目录
  configs:
    cpa-plugin-smart-scheduler:
      enabled: true
      priority: 1
      # 可选配置（均有健全默认值）：
      slow_avg_ms: 25000         # 滚动平均首字慢判定线（毫秒），默认 25s
      hard_max_ms: 90000         # 单笔极端慢硬熔断线（毫秒），默认 90s
      base_quiet_minutes: 90     # 首次轻度慢静默时长（分钟），默认 90m
      repeat_quiet_minutes: 300  # 重度/重复慢静默时长（分钟），默认 300m (5小时)
      min_active_ratio_percent: 30 # 最低在线账号保底比例（%），默认 30%
```

---

## 插件状态查询 API

插件挂载在 CLIProxyAPI 内部管理接口下：

```bash
curl -H "Authorization: Bearer <MANAGEMENT_KEY>" \
  http://localhost:8317/v0/management/plugins/cpa-plugin-smart-scheduler/status
```

**响应示例**：
```json
{
  "time": "2026-10-22T20:50:00+08:00",
  "accounts": [
    {
      "auth_id": "antigravity-aronowoberdick@gmail.com.json",
      "status": "healthy",
      "avg_ttft_ms": 7800,
      "last_ttft_ms": 8200,
      "total_requests": 142,
      "total_fails": 0,
      "cooldown_remaining_minutes": 0
    },
    {
      "auth_id": "antigravity-diazgilson542@gmail.com.json",
      "status": "cooling_down",
      "avg_ttft_ms": 85000,
      "last_ttft_ms": 105000,
      "demoted_reason": "单笔极端慢首字 105000ms(>=90000ms)",
      "cooldown_remaining_minutes": 184.5
    }
  ]
}
```

---

## 开源协议

本项目采用 [MIT License](LICENSE) 开源协议。
