# CPA Smart Scheduler Plugin (cpa-plugin-smart-scheduler)

A native in-process C ABI traffic routing and outlier detection circuit-breaker plugin for [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI), featuring **stream-specialized TTFT routing, per-model latency isolation, and zero magic numbers**.

Combines **Stream-Specialized Per-Model P2C (Power of Two Choices) + Peak-EWMA load balancing (with Least-Connections fallback for non-streaming)** with **Envoy-standard Outlier Detection and Half-Open Canary Probing**, dispatching requests at nanosecond speed directly in Go memory.

> 📖 **Detailed Architecture & Scheduling Algorithm Documentation**: See [SCHEDULING_LOGIC.md](SCHEDULING_LOGIC.md).

---

## Architecture Highlights (v0.5.1)

1. **Stream-Specialized Dual-Track Routing**:
   LLM latency decomposes into $\text{Prefill Time} + (\text{Output Tokens} \times \text{TPOT})$. Non-streaming request duration (0.5s to 25s) is heavily confounded by the number of output tokens requested by the prompt, not backend node congestion.
   - **Streaming (`Stream = true`)**: Dedicated to pure **Time To First Token (TTFT)**, which is independent of generation length. Fed into P2C Peak-EWMA to smoothly divert traffic away from congested accounts.
   - **Non-Streaming (`Stream = false`)**: **Never touches the EWMA TTFT ledger**, completely preventing essay-length responses from polluting latency metrics. Routes via pure **Least Connections (`InFlight + 1`)**, naturally buffering long-running tasks.
2. **Zero Arbitrary Magic Numbers**:
   Completely eliminates hardcoded latency seeds (e.g. 8000ms). Cold-start routing dynamically inherits the **Dynamic Fleet Mean** for that specific model with a 0.8 exploration bias. Unmeasured fleets start with an identical neutral baseline (`1000.0`), smoothly degenerating into least-connections / random exploration. The very first real sample replaces the seed 100% without artificial blending.
3. **Per-Model Latency Isolation**:
   In LLM workloads, latency varies dramatically between models (e.g. 1s on small flash models vs 25s on thinking models). Each account independently tracks a `models` map. P2C evaluates candidate costs strictly against the requested model:
   ```text
   Cost(a, M, stream) = BaseScore(a, M, stream) * (InFlight(a) + 1)
   ```
4. **Decoupled Latency Routing & Failure Circuit Breaking**:
   The system **never trips circuit breakers due to latency alone**. Latency is continuously handled via P2C Peak-EWMA.
5. **Envoy-Standard Outlier Detection**:
   Circuit breaking is triggered exclusively by discrete failure signals:
   - **429 Rate Limit (RESOURCE_EXHAUSTED)**: Immediate 300-minute (5-hour) ejection, covering Google's rolling quota window.
   - **Consecutive Gateway Failures**: Ejected after 3 consecutive 5xx or transport disconnects.
6. **503 MODEL_CAPACITY_EXHAUSTED Exemption**:
   Dynamically extracts `retryDelay` from Google's `google.rpc.RetryInfo` (e.g. `23s`) for precise cluster backoff without penalizing or ejecting the account.
7. **Binary Half-Open Canary Probing**:
   After 5 hours, allows exactly 1 request to probe recovery. Promotes to healthy on any HTTP 2xx completion regardless of TTFT; escalates to a 10-hour cooldown on repeat error.
8. **Envoy Panic Threshold**:
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
