# CPA Smart Scheduler Plugin (cpa-plugin-smart-scheduler)

A native in-process C ABI traffic routing and outlier detection circuit-breaker plugin for [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI), featuring **per-model latency isolation and zero magic numbers**.

Combines **Per-Model P2C (Power of Two Choices) + Peak-EWMA lowest-latency load balancing** with **Envoy-standard Outlier Detection and Half-Open Canary Probing**, dispatching requests at nanosecond speed directly in Go memory.

> 📖 **Detailed Architecture & Scheduling Algorithm Documentation**: See [SCHEDULING_LOGIC.md](SCHEDULING_LOGIC.md).

---

## Architecture Highlights (v0.5.0)

1. **Zero Arbitrary Magic Numbers**:
   Completely eliminates hardcoded latency seeds (e.g. 8000ms). Cold-start routing dynamically inherits the **Dynamic Fleet Mean** for that specific model with a 0.8 exploration bias. Unmeasured fleets start with an identical neutral baseline (`1.0`), smoothly degenerating into least-connections / random exploration. The very first real sample replaces the seed 100% without artificial blending.
2. **Per-Model Latency Isolation**:
   In LLM workloads, latency varies dramatically between models (e.g. 1s on small flash models vs 25s on thinking models). Each account independently tracks a `models` map. P2C evaluates candidate costs strictly against the requested model:
   ```text
   Cost(a, M) = BaseLatency(a, M) * (InFlight(a) + 1)
   ```
3. **Decoupled Latency Routing & Failure Circuit Breaking**:
   The system **never trips circuit breakers due to latency alone**. Latency is continuously handled via P2C Peak-EWMA.
4. **Envoy-Standard Outlier Detection**:
   Circuit breaking is triggered exclusively by discrete failure signals:
   - **429 Rate Limit (RESOURCE_EXHAUSTED)**: Immediate 300-minute (5-hour) ejection, covering Google's rolling quota window.
   - **Consecutive Gateway Failures**: Ejected after 3 consecutive 5xx or transport disconnects.
5. **503 MODEL_CAPACITY_EXHAUSTED Exemption**:
   Dynamically extracts `retryDelay` from Google's `google.rpc.RetryInfo` (e.g. `23s`) for precise cluster backoff without penalizing or ejecting the account.
6. **Binary Half-Open Canary Probing**:
   After 5 hours, allows exactly 1 request to probe recovery. Promotes to healthy on any HTTP 2xx completion regardless of TTFT; escalates to a 10-hour cooldown on repeat error.
7. **Envoy Panic Threshold**:
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
