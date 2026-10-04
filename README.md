# edbg: Event-Driven Debugging CLI for AWS

`edbg` is a terminal tool for developers who build on AWS serverless and event-driven services: **Lambda, Step Functions, SQS, EventBridge** and **X-Ray**. It answers the questions you ask while developing or debugging:

- *Is anything broken right now?* `edbg dash` shows a live dashboard of the whole account.
- *What is this function / workflow / queue doing?* `edbg watch` gives one live feed across services, whatever triggered them.
- *Which events are flowing on my bus?* `edbg tail` live-tails EventBridge.
- *Why did this execution fail?* `edbg trace-sfn` prints a timeline of every state.
- *What's sitting in my dead-letter queue?* `edbg dlq-inspect` peeks without consuming, then optionally redrives.
- *How did one request travel across services?* `edbg traces` and `edbg trace` read X-Ray.

It uses your normal AWS credentials, runs locally, and is read-only except for `tail`/`watch --events` (temporary resources it removes on exit) and `dlq-inspect --redrive` (asks first).

```
╭─ edbg dash ── dev · us-west-2 ───── last 1h ───────────── data to 10:41 · 540 metrics/refresh ≈ $0.33/h ─╮
╭─ Lambda invocations ─────╮╭─ Lambda errors ──────────╮╭─ Step Functions ─────────╮╭─ Dead-letter queues ─────╮
│ 12.4k  ▲8%               ││ 41  0.3%  ▲              ││ 1 failed  14 started     ││ 9 msgs  new              │
│ ▁▂▃▅▇▆▅▃▄▅▆▇█▇▅▃▂▃▅▆▇█▇▅ ││ ▁▁▁▁▂▁▁▁▁▇█▃▁▁▁▁▂▁▁▁▁▁▁▁ ││ ▁▁▁▁▁▁▁▁▁▁▁▁█▁▁▁▁▁▁▁▁▁▁▁ ││ ▁▁▁▁▁▁▁▁▁▂▃▅▇█▇▅▅▅▅▅▅▅▅▅ │
╰──────────────────────────╯╰──────────────────────────╯╰──────────────────────────╯╰──────────────────────────╯
 1 Lambda 37 ✖   2 Step Functions 3 ✖   3 Queues 12 ✖   4 EventBridge 5
   NAME                                     INVOKES  ERRORS   ERR%  THROTTL  P95 MS TREND
▸✖ club-insights-generator                     1.2k      38    3.1        0    2210 ▁▂▂▃▇█▅▂▁▁▁▂▃▅▇█
   org-wide-publisher                           830       0      -        0     410 ▃▃▄▃▃▄▃▃▃▄▃▃▄▃▃▄
   … 812 idle · i to show
╭─ recent errors · club-insights-generator ──────────────────────────────────────────────────────────────────╮
│ 10:41:58  ✖ ERROR TypeError: Cannot read properties of undefined (reading 'id')                            │
╰────────────────────────────────────────────────────────────────────────────────────────────────────────────╯
```

---

## Contents

- [Install](#install)
- [Quick start](#quick-start)
- [Which command do I use?](#which-command-do-i-use)
- [Commands](#commands)
  - [dash](#edbg-dash): developer dashboard
  - [watch](#edbg-watch): live feed across services
  - [tail](#edbg-tail): live EventBridge events
  - [trace-sfn](#edbg-trace-sfn): Step Functions execution timeline
  - [dlq-inspect](#edbg-dlq-inspect): peek at and redrive dead-letter queues
  - [traces / trace](#edbg-traces-and-edbg-trace): X-Ray
  - [cleanup](#edbg-cleanup): remove leftovers after a crash
  - [doctor](#edbg-doctor): check credentials and connectivity
- [Global flags and output formats](#global-flags-and-output-formats)
- [How it works](#how-it-works)
- [IAM permissions](#iam-permissions)
- [Cost](#cost)
- [Security notes](#security-notes)
- [Private VPCs and X-Ray](#private-vpcs-and-x-ray)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)
- [Development](#development)

---

## Install

**From source** (Go 1.24 or newer):

```bash
git clone https://github.com/gayanMadurapperuma/edbg.git
cd edbg
go build -o edbg .
./edbg --version
```

Or install straight into `$GOPATH/bin`:

```bash
go install github.com/gayanMadurapperuma/edbg@latest
```

**Prebuilt binaries:** tagged releases (`v*`) publish macOS, Linux and Windows binaries through GoReleaser on the GitHub Releases page.

**Shell completion:**

```bash
edbg completion zsh  > "${fpath[1]}/_edbg"    # zsh
edbg completion bash > /etc/bash_completion.d/edbg
edbg completion fish > ~/.config/fish/completions/edbg.fish
```

### Credentials and region

edbg uses the standard AWS SDK credential chain: environment variables, `~/.aws/credentials`, `~/.aws/config` profiles, SSO, and instance or container roles. Choose the account and region with flags or environment variables:

```bash
edbg dash --profile dev --region us-west-2
# or
export AWS_PROFILE=dev AWS_REGION=us-west-2
edbg dash
```

For SSO profiles, run `aws sso login --profile dev` first.

---

## Quick start

```bash
edbg doctor                          # 1. can edbg reach every service it needs?
edbg dash                            # 2. what's healthy, what's broken, across the account
edbg dash --name 'orders*'           #    ...narrowed to your service
edbg watch --all --name 'orders*'    # 3. live logs + executions + queue depth for it
edbg trace-sfn <execution-arn>       # 4. drill into one failed execution
edbg dlq-inspect orders-dlq          # 5. see what landed in the DLQ
```

---

## Which command do I use?

| I want to… | Command |
|---|---|
| See at a glance what's failing, slow or backed up | `edbg dash` |
| Follow Lambda logs, Step Functions runs and queue depth live, in one place | `edbg watch` |
| See the actual EventBridge events (payloads) on a bus | `edbg tail` |
| Understand why one Step Functions execution failed | `edbg trace-sfn` |
| Inspect or replay messages from a dead-letter queue | `edbg dlq-inspect` |
| Follow one request across API Gateway, Lambda, DynamoDB… | `edbg traces`, then `edbg trace` |
| Fix "my tail crashed and left a rule behind" | `edbg cleanup` |
| Check whether a VPC, proxy or IAM problem blocks edbg | `edbg doctor` |

**`dash`, `watch` and `tail` compared:**

| | `dash` | `watch` | `tail` |
|---|---|---|---|
| Shows | Numbers and trends per resource | Log lines, execution results, queue changes | Raw EventBridge events |
| Source | CloudWatch metrics | CloudWatch Logs, Step Functions API, SQS attributes (+ optional EventBridge) | EventBridge → temporary SQS queue |
| Scale | Hundreds of resources | Up to `--max-sources` per kind (default 20) | One bus |
| Freshness | Metrics arrive 1–3 minutes late | About 2 seconds | About 1 second |
| Creates AWS resources | No | Only with `--events` | Yes (temporary, removed on exit) |
| Sees activity no matter what triggered it | Yes | Yes | Only what crosses the bus |

---

## Commands

### `edbg dash`

A full-screen dashboard built from CloudWatch metrics. It covers every Lambda function, Step Functions state machine, SQS queue and EventBridge rule that matches `--name`.

```bash
edbg dash                                  # whole account in this region
edbg dash --name 'orders*' --window 1h     # one service, last hour
edbg dash --refresh 2m                     # refresh less often (cheaper)
edbg dash --once                           # print a snapshot and exit
edbg dash --once -o json | jq '.lambda[] | select(.errors > 0) | .name'
```

**Layout**

- **Cards:** Lambda invocations, Lambda errors (with error %), Step Functions failures, and queue backlog. The backlog card becomes **Dead-letter queues** when any DLQ has messages. Each card has a sparkline for the window and a ▲▼ trend against the previous window of the same length. A card turns red when its value is bad.
- **Chart:** a Braille line chart of the selected row (traffic in blue, failures in red), or of all resources on the tab when nothing is selected. It appears only on terminals at least 34 rows tall.
- **Tabs:** `1 Lambda`, `2 Step Functions`, `3 Queues`, `4 EventBridge`, each with its active count. A ✖ marks a tab that has problems.
- **Table:** rows that need attention come first: errors and failures, then dead-letter messages, message age and throttling, then traffic. Idle resources fold into one line.
- **Log pane:** opened with `enter`. It shows at most `--log-lines` lines.

| Tab | Columns |
|---|---|
| Lambda | invocations, errors, error %, throttles, p95 duration (ms), trend |
| Step Functions | started, succeeded, failed, timed out, trend |
| Queues | visible messages, age of oldest message, sent, received, trend (DLQs with messages are red) |
| EventBridge | matched events, target invocations, failed invocations, trend |

**Keys**

| Key | Action |
|---|---|
| `↑` `↓` / `j` `k`, `PgUp` `PgDn`, `g` `G` | Move the selection |
| `tab` / `shift+tab`, `←` `→`, `1`–`4` | Switch service tab |
| `enter` | Lambda: the newest error lines. Step Functions: the latest executions, failures first, with error and cause. Queues and rules: a summary plus the command to dig further |
| `a` | In the log pane: switch between all lines and errors only |
| `l` | Stream the selected Lambda's logs live (press again to stop) |
| `/` | Filter rows by name (`enter` keeps the filter, `esc` clears it) |
| `i` | Show or hide idle resources |
| `w` | Cycle the window: 5m → 15m → 1h → 3h → 24h |
| `r` | Refresh now |
| `?` | Help |
| `esc` | Close the log pane, help or filter |
| `q`, `ctrl+c` | Quit |
| Mouse | Wheel scrolls; clicking selects a row or tab |

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--name` | (all) | Only resources whose name matches this pattern. `*` and `?` wildcards, case-insensitive, applied to every service |
| `--window` | `15m` | Time window for numbers, sparklines and chart |
| `--refresh` | `1m` | How often to re-query metrics (minimum `30s`) |
| `--log-lines` | `15` | Lines in the log pane |
| `--bus` | `default` | Event bus whose rules are shown |
| `--once` | `false` | Print one snapshot and exit. Implied by `-o json` or when stdout is not a terminal |
| `--top` | `10` | With `--once`: rows per service |

**Good to know**

- Metrics are per minute and arrive 1–3 minutes late, so the dashboard shows **trends**. Use `enter` or `l`, or `edbg watch`, for second-by-second detail.
- New functions, queues and state machines are picked up automatically every 5 minutes.
- The header shows how many metrics each refresh requests and the estimated cost per hour. See [Cost](#cost).
- On terminals smaller than 80×12 it asks you to resize or use `--once`.

---

### `edbg watch`

One chronological live feed across Lambda logs, Step Functions executions, SQS queue depth and, optionally, EventBridge events. It watches the **resources themselves**, so it doesn't matter what triggered them: SQS, EventBridge, a schedule, API Gateway, the console or another Lambda.

```bash
edbg watch --lambda 'orders-*' --sfn 'OrderFlow*' --sqs 'orders-*'
edbg watch --lambda 'orders-api' --since 1h          # replay the last hour, then keep following
edbg watch --all --name 'orders*'                    # everything named orders*, plus EventBridge
edbg watch --all --name 'orders*' --events=false     # same, without the temporary EventBridge rule
edbg watch --logs '/ecs/orders-*'                    # any log group (ECS, API Gateway, Express workflows)
edbg watch --sfn 'OrderFlow*' -o json | jq .         # one JSON object per line
```

Example feed:

```
10:41:50  lambda  orders-api            {"level":"INFO","message":"order 812 accepted"}
10:41:50  lambda  orders-api            ✔ done 212 ms · 93 MB · cold start 336 ms
10:41:52  sfn     OrderFlow             ▶ started 6f2c…
10:41:58  lambda  orders-charge         ✖ ERROR PaymentDeclined: card_declined
10:42:01  sfn     OrderFlow             ✖ FAILED 6f2c… (9.1s) States.TaskFailed: PaymentDeclined
10:42:05  sqs     orders-dlq            visible 3 (+1) · DLQ received messages
```

| Source | What you see | How |
|---|---|---|
| `--lambda <pattern>` | Log lines with errors in red and warnings in yellow. The `REPORT` line becomes `done 212 ms · 93 MB · cold start 336 ms`. `START`/`END` lines are hidden unless you pass `--verbose` | Polls `/aws/lambda/<fn>` with `FilterLogEvents` |
| `--logs <pattern>` | Any log group, matched by full name | Same as above |
| `--sfn <pattern>` | `▶ started`, `✔ SUCCEEDED (23.7s)`, `✖ FAILED` with error and cause | Polls `ListExecutions`; failure details from `GetExecutionHistory` |
| `--sqs <pattern>` | Visible, in-flight and delayed counts when they change. Growth in a queue named `*dlq*` or `*dead*` is shown as an error | Polls `GetQueueAttributes`; **never receives messages** |
| `--events` (or `--source`, `--detail-type`, `--pattern`) | EventBridge events, as in `tail` | Temporary rule and queue, like `tail` |
| `--all` | All of the above for every function, standard state machine and queue matching `--name`, plus EventBridge | Rescans for new resources every `--rescan` |

Keys: `p` pause or resume (events are buffered), `c` clear, `q` quit.

| Flag | Default | Description |
|---|---|---|
| `--lambda`, `--logs`, `--sfn`, `--sqs` | | Name patterns (repeatable, `*` and `?`) |
| `--all` | `false` | Watch every resource kind; narrow with `--name` |
| `--name` | | With `--all`: the name pattern for every kind |
| `--events` | `false` | Include EventBridge (implied by `--all` unless `--events=false`) |
| `--bus`, `--source`, `--detail-type`, `--pattern`, `--compact`, `--include-cloudtrail` | | EventBridge options, as in `tail` |
| `--since` | `0` | Also show logs and executions from this far back |
| `--verbose` | `false` | Include Lambda `START`/`END` lines |
| `--max-sources` | `20` | Refuse to watch more than this many resources of one kind |
| `--interval` | `2s` | Poll interval for logs and executions (queues poll at 2.5×) |
| `--rescan` | `30s` | Look for newly created resources this often (`0` disables) |

**Tip:** patterns are case-sensitive in `watch`, but case-insensitive in `dash`. When a broad pattern hits `--max-sources`, use `dash` to find the busy resources, then `watch` those.

---

### `edbg tail`

Live-tails events on an EventBridge bus. edbg creates a temporary SQS queue and a rule with your pattern, prints each matching event, and deletes both when you quit.

```bash
edbg tail                                            # everything on the default bus (minus CloudTrail noise)
edbg tail --source my.app --detail-type OrderPlaced
edbg tail --bus orders-bus --pattern '{"detail":{"status":["FAILED"]}}'
edbg tail --source aws.states                        # Step Functions status-change events
edbg tail --compact                                  # one line per event
edbg tail -o json | jq .detail                       # pipe-friendly
```

In a terminal each event is a framed, colored JSON block. Keys: `p` pause or resume (buffered), `c` clear, `q` quit.

| Flag | Default | Description |
|---|---|---|
| `--bus` | `default` | Event bus name |
| `--source` | | Match this `source` |
| `--detail-type` | | Match this `detail-type` |
| `--pattern` | | Raw EventBridge event pattern (JSON); overrides `--source` and `--detail-type` |
| `--compact` | `false` | One line per event |
| `--include-cloudtrail` | `false` | With no filter, also show `AWS API Call via CloudTrail` events (very noisy) |

Test it from another terminal:

```bash
aws events put-events --entries '[{"Source":"my.app","DetailType":"OrderPlaced","Detail":"{\"id\":1}"}]'
```

`tail` only sees events that reach the bus. Lambda invocations, raw SQS messages, and Step Functions runs whose status events go elsewhere won't appear. Use `watch` or `dash` for those.

---

### `edbg trace-sfn`

A timeline of one Step Functions execution: the execution status, then every history event with its offset from the start, the state it entered or exited, and `error=… cause=…` for failures (shown in red).

```bash
edbg trace-sfn arn:aws:states:us-west-2:123456789012:execution:OrderFlow:6f2c...
edbg trace-sfn <execution-arn> --failed-only     # only failed / timed-out / aborted events
edbg trace-sfn <execution-arn> --follow          # keep polling until the execution finishes
```

| Flag | Description |
|---|---|
| `--failed-only` | Show only failure events |
| `--follow` | Poll a running execution until it finishes |

To find execution ARNs, open the Step Functions tab in `edbg dash` and press `enter`, or use `edbg watch --sfn <name>`.

---

### `edbg dlq-inspect`

Peeks at messages in a queue (usually a DLQ) **without consuming them**, and can optionally move them back to their source queue.

```bash
edbg dlq-inspect orders-dlq                 # by name
edbg dlq-inspect https://sqs.us-west-2.amazonaws.com/123456789012/orders-dlq
edbg dlq-inspect orders-dlq --max 50
edbg dlq-inspect orders-dlq --redrive       # asks for confirmation, then StartMessageMoveTask
edbg dlq-inspect orders-dlq -o json | jq -r .body
```

| Flag | Default | Description |
|---|---|---|
| `--max` | `10` | Maximum messages to show |
| `--redrive` | `false` | Move messages back to the source queue (asks first) |

Messages are received with a zero visibility timeout and de-duplicated. Peeking does not delete messages, but **it does increase each message's `ApproximateReceiveCount`**. On a queue that has its own redrive policy, that can push messages toward *its* DLQ.

---

### `edbg traces` and `edbg trace`

Read AWS X-Ray, if your services have tracing enabled.

```bash
edbg traces                         # recent traces (last 15m)
edbg traces --errors --since 1h     # only traces with errors, faults or throttling
edbg traces --service orders-api
edbg trace 1-6543a1b2-0123456789abcdef01234567    # timeline of every hop in one trace
```

| `traces` flag | Default | Description |
|---|---|---|
| `--since` | `15m` | Look back this far |
| `--errors` | `false` | Only traces with errors, faults or throttles |
| `--service` | | Only traces that touched this service |
| `--limit` | `20` | Maximum traces listed |

If nothing comes back, edbg tells you whether the search found no matches or the trace ID doesn't exist (often because tracing isn't enabled). See [Private VPCs and X-Ray](#private-vpcs-and-x-ray).

---

### `edbg cleanup`

Deletes `edbg-tail-*` rules and queues left behind when `tail` or `watch --events` was killed (`kill -9`, closed laptop, network loss). A normal exit, including Ctrl+C, already cleans up.

```bash
edbg cleanup
edbg cleanup --bus orders-bus
```

SQS can keep listing a deleted queue for up to about 60 seconds.

---

### `edbg doctor`

Makes one cheap read-only call to each service edbg uses, all in parallel with a per-service timeout, and prints `OK` or `FAIL` with the reason. It exits with status 1 if any check fails. Use it first in a new account, behind a proxy, or inside a VPC.

```bash
edbg doctor
edbg doctor --timeout 10s
```

```
Region: us-west-2

OK    sts (credentials)          arn:aws:sts::123456789012:assumed-role/dev/me
OK    sqs (tail, dlq-inspect)
OK    events (tail)
OK    states (trace-sfn)
OK    logs (watch --lambda)
OK    monitoring (dash metrics)
OK    lambda (dash)
FAIL  xray (trace, traces)       … context deadline exceeded
```

An `AccessDenied` result names the missing IAM action. A timeout usually means a missing route or VPC endpoint.

---

## Global flags and output formats

| Flag | Description |
|---|---|
| `--profile <name>` | AWS shared config profile |
| `--region <name>` | AWS region (overrides the profile and environment) |
| `-o, --output text\|json` | `json` prints machine-readable output: one object per line for streams (`tail`, `watch`, `dlq-inspect`), a single document for `dash --once` |
| `--no-color` | Disable color. Color is also off when `NO_COLOR` is set, with `-o json`, or when stdout isn't a terminal |
| `--version` | Print the version |

**Exit codes:** `0` on success or a normal quit, `1` on an error (printed as `Error: …` with an IAM hint for access-denied errors), `130` when interrupted with Ctrl+C.

**JSON examples**

```bash
# Functions with errors in the last hour
edbg dash --once --window 1h -o json | jq -r '.lambda[] | select(.errors > 0) | "\(.name) \(.errors)"'

# Only error lines from a live watch
edbg watch --lambda 'orders-*' -o json | jq -c 'select(.level == "error")'

# Event payloads only
edbg tail --source my.app -o json | jq .detail
```

---

## How it works

```
                 ┌──────────────── edbg (your machine, your credentials) ────────────────┐
                 │                                                                        │
  dash  ─────────┤  ListFunctions / ListStateMachines / ListQueues / ListRules (discover) │
                 │  GetMetricData, ≤500 series per call, idle resources gated             │
                 │                                                                        │
  watch ─────────┤  FilterLogEvents (logs)   ListExecutions (sfn)   GetQueueAttributes    │
                 │  polled every 2s, de-duplicated, merged into one time-ordered feed     │
                 │                                                                        │
  tail  ─────────┤  EventBridge rule ──▶ temporary SQS queue edbg-tail-<id> ──▶ long-poll │
                 │  queue policy only allows that rule (aws:SourceArn); deleted on exit   │
                 │                                                                        │
  trace-sfn, dlq-inspect, traces/trace: direct read API calls                             │
                 └────────────────────────────────────────────────────────────────────────┘
```

- **`dash`** discovers resources and then fetches two windows of metrics (the current one and the one before, for trends) in batches of 500. An idle function cannot have errors, so for idle resources it requests only a "gate" metric (Lambda invocations; queue visible and sent; state machine started and failed; rule matches). The other metrics are fetched only for active resources. This keeps large accounts cheap.
- **`watch`** runs one poller per source. Log polling overlaps the previous poll by 5 seconds and de-duplicates by event ID, so late-ingested lines aren't lost. Step Functions polling remembers each execution's status and reports changes. Queue polling compares depth with the last reading.
- **`tail`** builds the event pattern, creates the queue first, then the rule, the target and a queue policy limited to that rule. It long-polls for 20 seconds at a time and tears everything down in reverse order on exit. The names start with `edbg-tail-` so `cleanup` can find orphans.
- **Never touched:** edbg never sends messages, starts executions, invokes functions or changes your resources. The only exceptions are the `tail` resources and an explicit `--redrive`.

---

## IAM permissions

**Read-only (everything except `tail`, `watch --events` and `--redrive`):**

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "EdbgReadOnly",
      "Effect": "Allow",
      "Action": [
        "sts:GetCallerIdentity",
        "lambda:ListFunctions",
        "cloudwatch:GetMetricData",
        "logs:DescribeLogGroups",
        "logs:FilterLogEvents",
        "states:ListStateMachines",
        "states:ListExecutions",
        "states:DescribeExecution",
        "states:GetExecutionHistory",
        "sqs:ListQueues",
        "sqs:GetQueueUrl",
        "sqs:GetQueueAttributes",
        "sqs:ReceiveMessage",
        "events:ListRules",
        "events:ListEventBuses",
        "xray:GetTraceSummaries",
        "xray:BatchGetTraces"
      ],
      "Resource": "*"
    }
  ]
}
```

`sqs:ReceiveMessage` is only for `dlq-inspect` peeking. Leave it out if you don't use that command.

**Add for `tail`, `watch --events` and `cleanup`:** these actions are limited to edbg's own resources.

```json
{
  "Sid": "EdbgTail",
  "Effect": "Allow",
  "Action": [
    "sqs:CreateQueue", "sqs:SetQueueAttributes", "sqs:GetQueueAttributes",
    "sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:DeleteQueue",
    "events:PutRule", "events:PutTargets", "events:RemoveTargets", "events:DeleteRule"
  ],
  "Resource": [
    "arn:aws:sqs:*:*:edbg-tail-*",
    "arn:aws:events:*:*:rule/edbg-tail-*",
    "arn:aws:events:*:*:rule/*/edbg-tail-*"
  ]
}
```

**Add for `dlq-inspect --redrive`:** `sqs:StartMessageMoveTask` on the DLQ, plus `sqs:SendMessage` on the source queue.

Command by command:

| Command | Actions |
|---|---|
| `dash` | `lambda:ListFunctions`, `states:ListStateMachines`, `sqs:ListQueues`, `events:ListRules`, `cloudwatch:GetMetricData`; log pane: `logs:FilterLogEvents`, `states:ListExecutions`, `states:GetExecutionHistory` |
| `watch` | `logs:DescribeLogGroups`, `logs:FilterLogEvents`, `states:ListStateMachines`, `states:ListExecutions`, `states:GetExecutionHistory`, `sqs:ListQueues`, `sqs:GetQueueAttributes` (+ tail actions with `--events`) |
| `tail` | the `EdbgTail` statement |
| `trace-sfn` | `states:GetExecutionHistory`, `states:DescribeExecution` |
| `dlq-inspect` | `sqs:GetQueueUrl`, `sqs:ReceiveMessage`, `sqs:GetQueueAttributes` (+ `sqs:StartMessageMoveTask` for `--redrive`) |
| `traces`, `trace` | `xray:GetTraceSummaries`, `xray:BatchGetTraces` |
| `cleanup` | `events:ListRules`, `events:RemoveTargets`, `events:DeleteRule`, `sqs:ListQueues`, `sqs:DeleteQueue` |
| `doctor` | one read call per service, listed above |

If a permission is missing, `dash` keeps working and shows a note for the affected service. `watch` stops only that source and says why.

---

## Cost

edbg makes API calls billed to your account. Approximate us-east-1 list prices; check current pricing for your region.

| Command | What is billed | Typical cost |
|---|---|---|
| `dash` | `GetMetricData`: about **$0.01 per 1,000 metrics requested** | Idle resources cost 1–2 metrics per refresh, active ones up to 4. Example: 461 functions + 18 queues + 12 state machines ≈ 540 metrics/refresh ≈ **$0.33/hour** at the 60s default. With `--name` narrowed to one service: cents per hour. The header shows the live estimate |
| `watch` | CloudWatch Logs `FilterLogEvents` and other API calls (no per-request charge for most); data scanned by Logs Insights is **not** used | Negligible for normal use |
| `tail` | EventBridge custom events about $1 per million; SQS about $0.40 per million requests | Idle tail ≈ $0.002/day (20s long polls) |
| `trace-sfn`, `dlq-inspect`, `traces`, `cleanup`, `doctor` | Standard API calls | Negligible |

To reduce `dash` cost, narrow with `--name` or use `--refresh 2m` or `5m`. Use `--once` in scripts instead of leaving the dashboard open.

---

## Security notes

- **Credentials in payloads are masked.** Fields named `sessionToken`, `secretAccessKey`, `accessKeyId`, `password`, `authorization` and `x-amz-security-token` are printed as `"****"` in `tail`, `watch` and JSON output. This matters for CloudTrail events, which include temporary credentials from `AssumeRole` responses. Other sensitive data in your own event payloads is printed as-is, so take care when sharing terminal output.
- **CloudTrail events are off by default.** When tailing with no filter, `AWS API Call via CloudTrail` events are excluded (`--include-cloudtrail` turns them back on).
- **Temporary resources are scoped.** The `tail` queue policy only accepts messages from its own rule (`aws:SourceArn`). Resources are named `edbg-tail-<id>`, deleted on exit, and removable with `edbg cleanup`.
- **Local only.** edbg sends nothing anywhere except AWS APIs, using your credentials.
- **Be careful in production.** Everything except `tail`, `watch --events` and `--redrive` is read-only. In production accounts, prefer `dash` and `watch` without `--events`.

---

## Private VPCs and X-Ray

**VPC:** edbg calls AWS public API endpoints from wherever it runs. If your account only allows private access, run it inside the VPC (bastion, CloudShell VPC environment, VPN, or a dev container in the VPC). Create **interface VPC endpoints** with private DNS for the services you use:

| Endpoint | Used by |
|---|---|
| `sts` | all (credentials) |
| `monitoring` | `dash` |
| `lambda` | `dash` |
| `logs` | `watch`, `dash` log pane |
| `states` | `watch --sfn`, `trace-sfn`, `dash` |
| `sqs` | `tail`, `watch --sqs`, `dlq-inspect`, `dash` |
| `events` | `tail`, `watch --events`, `cleanup`, `dash` |
| `xray` | `traces`, `trace` |

Then run `edbg doctor`. Timeouts point to the missing endpoint.

**X-Ray** is opt-in per service: Lambda **Active tracing**, API Gateway tracing, Step Functions tracing, or the X-Ray SDK or ADOT in containers. Without it, `traces` and `trace` have nothing to show, but every other command works.

---

## Limitations

- **Metric delay:** `dash` metrics are per minute and arrive 1–3 minutes late. It shows trends, not the last few seconds.
- **Silent functions:** a Lambda function that writes no logs shows nothing in `watch`. Its numbers still appear in `dash`.
- **Express workflows** have no execution list. `watch --sfn` skips them with a note; use `watch --logs` on their log group. `dash` does show their metrics.
- **Queue contents:** `watch --sqs` and `dash` show counts, not message bodies. Use `dlq-inspect` to see messages.
- **`tail` scope:** it only sees events that reach the chosen bus.
- **Polling delay:** `watch` adds about 2 seconds of delay. Many log groups can approach CloudWatch Logs rate limits; `--max-sources` guards against that.
- **Pattern matching:** names are case-sensitive in `watch` and case-insensitive in `dash`.
- **Single account and region:** edbg reads one account and one region at a time (`--profile`, `--region`).
- **Peeking side effect:** peeking with `dlq-inspect` increases `ApproximateReceiveCount`.

---

## Troubleshooting

| Symptom | Fix |
|---|---|
| `Error: … AccessDenied …` | The message names the missing action. Add it from [IAM permissions](#iam-permissions) |
| `doctor` shows timeouts | Missing route or VPC endpoint, or a proxy. See [Private VPCs](#private-vpcs-and-x-ray) |
| `dash` shows `0 active` everywhere | Nothing ran in the window. Try `--window 24h`, or press `i` to see idle resources. Check `--name`: `orders-dev-*` won't match `orders-stateless-…` |
| `watch` says `patterns matched 455 Lambda log groups (limit 20)` | Narrow the pattern, raise `--max-sources`, or use `dash` to find the busy functions first |
| `watch --lambda` shows nothing | The function has no log group yet (it never ran), or it isn't logging. Check that the name matches the function, not the log group |
| `tail` shows nothing | Check `--bus`, and send a test event (see [tail](#edbg-tail)). The event may be a CloudTrail event; add `--include-cloudtrail` |
| A `tail` was killed and left resources | `edbg cleanup` |
| Terminal looks broken after a crash | Run `reset` |
| Braille chart shows boxes | Use a font with Braille glyphs (the default fonts on macOS, Windows Terminal and most Linux systems have them) |
| `traces`: "not found … opt-in" | X-Ray isn't enabled for that service, or the trace is older than the 30-day retention |

---

## Development

```bash
go build -o edbg .
go vet ./...
go test -race ./...
golangci-lint run          # config in .golangci.yml (v2)
```

**Project layout**

```
main.go                     entry point → cmd.Execute()
cmd/
  root.go                   global flags, signal handling, exit codes, friendly errors
  dash.go                   dash command, --once text/JSON output
  dash_metrics.go           resource discovery, GetMetricData batching and gating, snapshot model
  dash_model.go             Bubble Tea model: keys, mouse, refresh loop, log pane, live stream
  dash_view.go              cards, tables, chart panel (Lip Gloss)
  dash_chart.go             sparklines, Braille line chart, number formatting
  watch.go, watch_*.go      watch command and its log / Step Functions / SQS pollers
  tail.go, tail_view.go     tail session (temporary queue and rule), event rendering, redaction
  trace_sfn.go, dlq.go, xray.go, cleanup.go, doctor.go, glob.go
internal/
  awsclient/                AWS config loading, small client interfaces (api.go) for testing
  output/                   color helpers, JSON pretty printing
```

**Testing approach:** every AWS call goes through small interfaces in `internal/awsclient/api.go`, which the real SDK clients satisfy. Tests use fakes that embed the interface and override only the methods they need, so the whole suite runs without an AWS account. The dashboard model is tested by sending key messages to `Update` and checking `View` output.

**CI and releases:** GitHub Actions runs vet, tests and golangci-lint on every push and pull request (`.github/workflows/ci.yml`). Pushing a `v*` tag builds and publishes binaries with GoReleaser (`.github/workflows/release.yml`, `.goreleaser.yaml`), with the version injected via `-ldflags "-X github.com/gayanMadurapperuma/edbg/cmd.version=…"`.

**Dependencies:** AWS SDK for Go v2, Cobra, Bubble Tea and Lip Gloss (dashboard), `golang.org/x/term` (raw-mode keys in `tail`/`watch`). The `go` directive is held at 1.24. When adding modules, check that `go get` doesn't raise it.
