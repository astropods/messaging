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
