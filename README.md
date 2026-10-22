# CPA Smart Scheduler Plugin (cpa-plugin-smart-scheduler)

A native in-process C ABI traffic routing and outlier detection circuit-breaker plugin for [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI).

Combines **P2C (Power of Two Choices) + Peak-EWMA lowest-latency load balancing** with **Envoy-standard Outlier Detection and Half-Open Canary Probing**, dispatching requests at nanosecond speed directly in Go memory.

> 📖 **Detailed Architecture & Scheduling Algorithm Documentation**: See [SCHEDULING_LOGIC.md](SCHEDULING_LOGIC.md).

---

## Architecture Highlights (v0.4.0)

1. **Decoupled Latency Routing & Failure Circuit Breaking**:
   In LLM workloads, TTFT varies naturally due to prompt sizes and reasoning tokens. The system **never trips circuit breakers due to latency alone**. Latency is continuously handled via P2C Peak-EWMA:
   ```text
   Cost = EWMA_TTFT * (InFlight + 1)
   ```
2. **Envoy-Standard Outlier Detection**:
   Circuit breaking is triggered exclusively by discrete failure signals:
   - **429 Rate Limit (RESOURCE_EXHAUSTED)**: Immediate 300-minute (5-hour) ejection, covering Google's rolling quota window.
   - **Consecutive Gateway Failures**: Ejected after 3 consecutive 5xx or transport disconnects. Transient single drops are forgiven.
3. **503 MODEL_CAPACITY_EXHAUSTED Exemption**:
   Dynamically extracts `retryDelay` from Google's `google.rpc.RetryInfo` (e.g. `23s`) for precise cluster backoff without penalizing or ejecting the account.
4. **Binary Half-Open Canary Probing**:
   After 5 hours, allows exactly 1 request to probe recovery. Promotes to healthy on any HTTP 2xx completion regardless of TTFT; escalates to a 10-hour cooldown on repeat error.
5. **Envoy Panic Threshold**:
   Maintains $\ge 30\%$ active accounts to prevent cluster deadlocks during cascading incidents.

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
      consecutive_gateway_failures: 3
      base_ejection_minutes: 300
      max_ejection_minutes: 600
      panic_threshold_percent: 30
      ewma_alpha: 0.3
```

---

## License

[MIT License](LICENSE)
