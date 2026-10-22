# CPA Smart Scheduler Plugin (cpa-plugin-smart-scheduler)

A native in-process C ABI scheduler and latency circuit-breaker plugin for [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI).

Designed specifically for LLM account pools (especially Google Antigravity / Gemini Pro, Codex, Claude) to eliminate **silent queue degradation, 100s+ latency spikes, repeated thrashing of failing credentials, and unentitled model 404 false-positives**.

---

## Key Features

- **In-Process Real-Time Observability**:
  Hooks into `usage_plugin` (`usage.handle`) to capture request TTFT (Time To First Token) and latency at nanosecond latency inside the CPA process without external polling.
- **Adaptive Two-Tier Circuit Breaking**:
  - **Mild Degradation (90m Cooldown)**: Isolates accounts whose rolling TTFT exceeds 25s for 90 minutes.
  - **Severe / Quota Throttling (300m Cooldown)**: Single requests $\ge 90\text{s}$ or 504/499 timeouts trigger a 300-minute (5-hour) cooldown to outlast Google's 5-hour rolling quota window.
- **Model 404 Pool-Walk Immune**:
  Explicitly ignores 404 NOT_FOUND errors caused by pool-walk queries on unentitled models.
- **Dynamic Capacity Floor**:
  Maintains at least 30% active credentials under any condition to avoid emptying the pool.
- **Flap Decay & Self-Healing**:
  Accounts running cleanly for 12 hours automatically reset their flap count.
- **Embedded Diagnostic Management Route**:
  Exposes `/v0/management/plugins/cpa-plugin-smart-scheduler/status` for instant inspection.

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
