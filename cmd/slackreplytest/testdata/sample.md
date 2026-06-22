# Deployment incident review: `checkout-api` latency spike

**Summary.** Between 14:02 and 14:41 UTC the `checkout-api` service served p99 latencies above 4 seconds, roughly twelve times its normal baseline of ~320ms. The trigger was a routine config rollout that doubled the connection-pool acquisition timeout while simultaneously halving the maximum pool size, which under peak afternoon traffic starved request handlers of database connections. No data was lost and no orders were dropped, but approximately 3.7% of checkout attempts in that window experienced a retryable timeout that the client eventually recovered from. This review walks through the timeline, the root cause, the contributing factors, and the concrete follow-ups we are committing to.

## Timeline

The incident began quietly. At 13:58 UTC the platform team merged a configuration change intended to make connection handling more conservative ahead of an unrelated database migration scheduled for the weekend. The change shipped through the normal pipeline and passed every automated gate, because none of those gates model behavior under sustained concurrent load. The rollout reached the first production cell at 14:01, and within ninety seconds the latency graphs for that cell began to climb. By 14:08 the rollout had progressed to three of eight cells, and the aggregate p99 had crossed the alerting threshold.

> The on-call engineer was paged at 14:09 and acknowledged within two minutes. Their first hypothesis — a downstream database problem — was reasonable given the symptom, but it cost us roughly eleven minutes of investigation before the config rollout was identified as the actual trigger.

At 14:22 the on-call engineer correlated the latency onset with the rollout start time and paused the deployment. Pausing stopped the blast radius from growing but did not heal the already-affected cells, because the bad configuration was still live there. At 14:34 the team initiated a rollback, and by 14:41 every cell had returned to baseline latency. The incident was declared resolved at 14:48 after fifteen minutes of stable metrics.

## Root cause

The configuration change altered two pool parameters at once. The first lowered `max_pool_size` from 64 to 32 per instance. The second raised `acquire_timeout_ms` from 250 to 2000. Each change in isolation is defensible. Together, under load, they interact badly: when the pool is exhausted, a handler now waits up to two full seconds to acquire a connection instead of failing fast at 250ms. During that two-second wait the handler holds a request slot, which means fewer slots are available for incoming work, which increases the depth of the wait queue, which increases the average acquisition wait, in a reinforcing loop. Halving the pool size made exhaustion far more likely to begin with, and the longer timeout turned what would have been a brief, self-correcting blip into a sustained degradation.

The key insight is that the two parameters were tuned by different people for different reasons and reviewed independently. Neither reviewer saw the combined effect, because our config system presents each value as a standalone field with no notion of how they relate. A load test would have caught this immediately, but config-only changes do not currently trigger a load test in CI.

## Contributing factors

Several things turned a small mistake into a user-visible incident:

- **No load modeling for config changes.** Code changes run a synthetic load suite; config changes skip it entirely on the assumption that they are low-risk. This incident is a clear counterexample.
- **Coupled parameters presented independently.** The pool size and the acquisition timeout are deeply related, but the config UI and the review tooling treat them as unrelated scalars.
- **Alerting on symptom, not cause.** Our paging alert fires on p99 latency, which is correct, but we have no alert that correlates a latency change with a recent rollout, so the responder had to make that connection manually.
- **Slow rollback ergonomics.** Rolling back required locating the previous config revision by hand and re-applying it. There is no one-command "revert last config rollout" path, which added several minutes during the most time-sensitive part of the response.

## What went well

It is worth naming the things that worked, because we want to keep doing them. The automated rollout halted progression at three of eight cells rather than all eight, which capped the blast radius. The on-call rotation responded quickly and communicated clearly in the incident channel throughout. Customer impact was limited to retryable timeouts rather than hard failures, because the client SDK already implements exponential backoff with jitter. And the metrics we needed to diagnose the problem — per-cell latency, pool utilization, and rollout markers — were all present and correctly labeled, even if we had not wired them together into a single alert.

## Remediation

We are committing to the following changes, each with an owner and a target date.

| Action | Owner | Target | Status |
|--------|-------|--------|--------|
| Run the synthetic load suite on config-only changes | platform | next sprint | planned |
| Group coupled pool parameters in the config schema | platform | two sprints | planned |
| Add a "latency change vs recent rollout" correlation alert | observability | next sprint | in design |
| Ship a one-command config rollback | release-eng | next sprint | planned |
| Document safe ranges for pool tuning | platform | two sprints | planned |

The highest-leverage item is the load suite on config changes, because it closes the gap that let this ship in the first place. The correlation alert is a close second, since it would have saved roughly eleven minutes of misdirected investigation.

## Appendix: reproduction

For anyone wanting to reproduce the failure locally, the following snippet drives the pool into the same starvation regime against a local instance. Run it with the bad configuration applied and watch the acquisition wait climb.

```python
import asyncio, time, statistics

async def hammer(pool, n=500):
    waits = []
    async def one():
        start = time.monotonic()
        conn = await pool.acquire()       # blocks up to acquire_timeout_ms
        waits.append(time.monotonic() - start)
        await asyncio.sleep(0.05)          # simulate query work
        await pool.release(conn)
    await asyncio.gather(*(one() for _ in range(n)))
    print("p99 acquire wait:", statistics.quantiles(waits, n=100)[-1])

asyncio.run(hammer(pool))
```

With `max_pool_size=32` and `acquire_timeout_ms=2000`, the p99 acquire wait climbs past 1.8 seconds almost immediately. Restoring `max_pool_size=64` and `acquire_timeout_ms=250` brings it back under 40 milliseconds. The takeaway is that these two numbers must be tuned together, never independently, and any change to either should be validated under a realistic concurrency profile before it reaches production.

If you have questions about this review or want to volunteer to own one of the remediation items, reply in this thread and we will route it to the right person. Thanks to everyone who jumped on the response and to the on-call engineer who kept the incident channel clear and calm under pressure.

## Customer communication

Because impact was limited to retryable timeouts, we did not post a public status-page incident, but we did proactively reach out to the three enterprise accounts whose dashboards showed elevated checkout latency during the window. Each received a short note explaining that a configuration change had briefly degraded performance, that no orders were lost, and that a fix was already in place. Two of the three replied to thank us for the heads-up; the third asked for a written root-cause summary, which is part of why this document is being shared more widely than usual. The lesson here is that proactive, specific communication — naming the window, the impact, and the resolution — consistently lands better than either silence or a vague "we experienced an issue" boilerplate.

Internally, the incident channel stayed focused because the responder posted a running summary every few minutes rather than narrating every individual action. Anyone joining late could read the pinned summary and immediately understand the current state without scrolling through dozens of messages. We want to make that pinned-running-summary practice a standard expectation for any incident that runs longer than fifteen minutes.

## Detection and monitoring deep-dive

The latency alert worked exactly as designed, firing within seven minutes of the first affected request. What it could not do was tell the responder *why* latency had changed. Our dashboards carry a rollout-marker annotation, but it lives on a different panel than the latency graph, so connecting the two required the responder to mentally overlay two charts. That cognitive step is small when you are calm and enormous when you are eleven minutes into a page with executives watching the channel.

> The single highest-leverage monitoring change we can make is not another threshold alert. It is an alert that says, in plain language, "p99 latency for checkout-api rose 12x within 90 seconds of config rollout `cfg-4821`." That sentence collapses eleven minutes of investigation into a single glance.

We are scoping that correlation alert now. The rough design is to join the latency anomaly stream against the rollout event stream on service and time window, and to fire only when an anomaly begins within a few minutes of a rollout. False positives are a real risk — plenty of latency changes have nothing to do with rollouts — so the first version will be advisory rather than paging, and we will tune the correlation window based on a backtest against the last six months of incidents.

## Capacity and load modeling

A recurring theme across our last several incidents is that we reason about capacity in terms of steady-state averages and then get surprised by behavior under concurrency. Connection pools are the canonical example: average utilization can look perfectly healthy at fifty percent while the pool is intermittently exhausting during short bursts, and it is those bursts that produce the tail latency customers actually feel. Averages hide bursts, and bursts are where incidents live.

| Metric | Steady-state view | Burst reality |
|--------|-------------------|---------------|
| Pool utilization | ~50% | 100% during afternoon peaks |
| Acquire wait p50 | 4ms | 12ms |
| Acquire wait p99 | 40ms | 1800ms with the bad config |
| Request slots free | plenty | near zero under load |

The remediation here is partly tooling and partly culture. On tooling, we will add burst-aware panels that show pool utilization at the 99th percentile over short windows rather than as a rolling average. On culture, we want config reviews for any resource-pool parameter to explicitly ask "what does this do under peak concurrency?" rather than "is this value reasonable in isolation?" — because, as this incident showed, two individually reasonable values combined into an unreasonable system.

## Broader organizational lessons

Zooming out, the deepest lesson is about how related settings get reviewed independently. Our configuration system, our review tooling, and our mental models all treat each parameter as a standalone knob. Real systems do not work that way: parameters interact, and the interactions are where the surprises hide. We do not need a heavyweight process to fix this; we need our tooling to surface relationships so reviewers see them without having to already know them.

```yaml
# Proposed: group coupled parameters so reviewers see them together.
connection_pool:
  max_size: 64          # lowering this raises exhaustion probability
  acquire_timeout_ms: 250
  # WARNING: max_size and acquire_timeout_ms interact under load.
  # A small pool with a long timeout creates a reinforcing wait queue.
  # Validate any change to either under a realistic concurrency profile.
```

A simple grouping like the snippet above, plus an inline warning, would have made the combined risk visible at review time. It is a small change with outsized leverage, and it generalizes well beyond connection pools to any set of parameters whose safe values depend on one another.

## Open questions

A few things remain genuinely uncertain and we want to be honest about them rather than paper over them. First, we do not yet know whether the same coupled-parameter risk exists in other services that share this pool library; an audit is queued but not complete. Second, we are not certain the seven-minute alert latency is fast enough — for a 12x degradation, even seven minutes is a lot of customer pain, and we may want a faster, tighter anomaly detector for the checkout path specifically. Third, the rollback ergonomics problem almost certainly affects more than just config changes, and we want to understand the full surface area before committing to a fix shape. We will track each of these as follow-up investigations and report back in two weeks.

## Follow-up checkpoint

We will hold a fifteen-minute checkpoint two weeks from today to confirm each remediation item has either shipped or has a concrete, dated plan. The intent is not a status-theater meeting but a forcing function: items that have not moved get either re-prioritized or explicitly dropped with a reason, so nothing lingers in a permanent "planned" limbo. Historically our action items from reviews have had roughly a sixty percent completion rate, and the ones that fall through are almost always the ones without a named owner and a date — both of which every item in the table above now has. If you own one of these and the date is going to slip, the most useful thing you can do is say so early in this thread so we can adjust rather than discover the slip at the checkpoint.

A final note on tone: nobody made a careless mistake here. Two careful people made two reasonable changes that happened to interact badly, and our systems did not give them or the reviewer any way to see the interaction. That is a systems problem, not a people problem, and the remediation is deliberately aimed at the systems — load testing for config, coupled-parameter grouping, correlation alerting, and faster rollback — rather than at asking humans to be more careful. Asking people to be more careful is what you do when you have run out of real ideas, and we are not out of real ideas.

Thanks again for reading this far. Incident reviews are most valuable when they change behavior, so the real measure of this document is whether the five remediation items actually ship and whether the next config change of this kind gets caught before it reaches production.
