# CPA Smart Scheduler Plugin (cpa-plugin-smart-scheduler)

A native in-process C ABI scheduler and latency circuit-breaker plugin for [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI).

Combines **P2C (Power of Two Choices) + Peak-EWMA latency load balancing** with **Half-Open Canary Probing**, dispatching requests at nanosecond speed directly in Go memory.

> 📖 **Detailed Architecture & Scheduling Algorithm Documentation**: See [SCHEDULING_LOGIC.md](SCHEDULING_LOGIC.md).

---

## Architecture Highlights (v0.3.1)

1. **P2C + Peak-EWMA Routing (No Forced Idle for Mild Latency)**:
   Mild delays naturally inflate $\text{Cost} = \text{EWMA\_TTFT} \times (\text{InFlight} + 1)$, steering traffic to faster accounts without taking nodes offline.
2. **503 MODEL_CAPACITY_EXHAUSTED Exemption**:
   Exempts upstream capacity shortages from account penalties, dynamically extracting retryDelay from Google RetryInfo (e.g. 23s) for precise backoff.
3. **Severe 5-Hour Circuit Breaking**:
   Single TTFT $\ge 90\text{s}$ or hard 429 rate limits isolate the account for 300 minutes (5 hours).
4. **Half-Open Canary with 10-Hour Escalation**:
   Allows exactly 1 request to test recovery. Promotes to healthy on success; escalates to a 10-hour cooldown on repeat failure.
5. **Model 404 Pool-Walk Immune**:
   Filters unentitled model 404 NOT_FOUND errors completely.
6. **Dynamic Capacity Floor**:
   Maintains $\ge 30\%$ active accounts to prevent empty-pool deadlock.

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
      cooldown_minutes: 300
      canary_fail_minutes: 600
      min_active_ratio_percent: 30
```

---

## License

[MIT License](LICENSE)
