# Scheduler — Traffic & Capacity

- Dispatcher: every 5s does one `ZMPOP max 5` (≤5 from one bucket) → 5 jobs / 5s = **1 job/s/task** = 60/min = 3,600/hr = 86,400/day
- Scale: N tasks (atomic ZMPOP) → N jobs/s → 10 tasks = 600/min, 100 tasks = 6,000/min
- Bucket drain: a minute with M due-jobs drains in M/N sec → keep `N ≥ peak_minute_jobs / MaxExecutionDelay_sec` (M=600, delay=300s → N≥2), within ~30-bucket (30 min) poll window
- Scheduler ingest: ≤5 msgs/s receive; ~5–10ms/job (1 ZADD + 1 PutItem) → ~100–200 jobs/s/task batched, ~5 jobs/s if 1 job/msg
- Executor: ~5 jobs/s/task; <1 if conditional (sequential 5s HTTP timeout)
- DynamoDB: on-demand ~4,000 WCU ÷ 3 writes/job ≈ 1,300 jobs/s (autoscales)
- Redis: schedule keys all use hash tag `{1}` → single shard, ~10⁴–10⁵ ops/s ceiling regardless of cluster size

## Can we handle 1M/min (16,667 jobs/s) with resources?

- Target: 1,000,000 / 60 = **16,667 jobs/s**
- DynamoDB: 16,667 × 3 writes/job = **~50,000 WCU** needed (default 4,000 → pre-warm on-demand or provisioned)
- Redis: 16,667 × (1 ZADD + ~1 ZMPOP share) ≈ 20k–30k ops/s → exceeds single shard → drop `{1}`, shard by minute (~4–8 shards)
- Dispatcher: fix to `count`=500, tick=1s = 500 jobs/s/task → **~34 tasks** for 16,667/s
- Bucket drain: 1M in one bucket; need `N ≥ 1,000,000 / 300s ≈ 3,334/s` to beat 300s `MaxExecutionDelay` → 16,667/s drains in ~60s ✓
- Verdict: **achievable** (sits inside ~10k–30k/s post-fix ceiling), but **not as-is** — requires all 3: dispatcher batch/tick fix + Redis re-shard + DynamoDB ~50k WCU
</content>
