# CPA Smart Scheduler Plugin (cpa-plugin-smart-scheduler)

A native in-process C ABI scheduler and latency circuit-breaker plugin for [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI).

Combines **P2C (Power of Two Choices) + Peak-EWMA latency load balancing** with **Half-Open Canary Probing**, eliminating external priority-script lag by dispatching requests at nanosecond speed in Go memory.

---

## Architecture Highlights

1. **P2C + Peak-EWMA Routing**:
   Samples 2 candidates randomly per request, evaluates $\text{Cost} = \text{EWMA\_TTFT} \times (\text{InFlight} + 1)$, and selects the lower-cost account to avoid hot-spotting and queue dogpiling.
2. **Half-Open Canary Trial**:
   Accounts waking up from cooling-down enter `canary` mode, allowing exactly 1 probe request. Promoted to `healthy` only if TTFT < 25s, protecting all other production traffic.
3. **Model 404 Pool-Walk Immune**:
   Filters unentitled model 404 NOT_FOUND errors completely.
4. **Dynamic Capacity Floor**:
   Maintains $\ge 30\%$ active accounts to prevent empty-pool deadlock.
5. **Embedded Diagnostic API**:
   `/v0/management/plugins/cpa-plugin-smart-scheduler/status` exposes real-time in-flight concurrency, EWMA latencies, and cooldown countdowns.

---

## Configuration

In CLIProxyAPI's `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-plugin-smart-scheduler:
      enabled: true
      priority: 1
      slow_avg_ms: 25000
      hard_max_ms: 90000
      base_quiet_minutes: 90
      repeat_quiet_minutes: 300
      min_active_ratio_percent: 30
```

---

## License

[MIT License](LICENSE)
