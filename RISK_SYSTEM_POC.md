# Risk System scoring POC

This playground implements the current shape of the Risk System integration:

- `check_risk_system_pg` is one genuine async Temporal activity (`system.AsyncPayloadHandler`),
  reused for every re-ask. It only "calls RS" and records what came back **verbatim**, onto its
  own `process.scoring.risk_system.*` fields (`request_id`, `status`, `required_data_set`, `max_ltv`,
  `reject_reason`) — no interpretation. A verdict is delivered by directly completing the pending
  activity (`client.CompleteActivityByID`), which is what `cmd/testcli`'s `verdict` subcommand does.
- `check_risk_system_pg_verdict` (`internal/process/scoring/checkrisksystemverdict`) is a separate,
  ordinary **synchronous** activity that reads those raw fields back and is the one place that
  interprets them: `required_data_set` → `survey_type`, `status` → `$.status`, terminal timestamps,
  `max_ltv`. It has to be a separate, non-async activity because `check_risk_system_pg`'s async
  handler (`runtime.AsyncPayloadHandler = func(map[string]any) (map[common.HString]any, error)`) only
  ever receives the raw external payload, never current document state — and the underwriting
  eligibility gate below (`environment_check.result`/`product_type`) needs exactly that.
- **`trigger_seq` is the one re-ask counter.** `check_risk_system_pg` reads it as a required
  read, and every survey page — not just a multi-page survey's final page — bumps it by one, so
  **every page submission re-asks RS**, even a page whose own findings are first-time data.
  `seed_scoring_checkpoint_pg` seeds it to `0` once both intake checks pass, which also triggers the
  first call (with `checkrisksystem.intakeGate` requiring both checks `true`). Every other read is a
  rollback trigger too, so a later *change* to data RS already saw re-asks it on its own. See
  [What re-asks Risk System](#what-re-asks-risk-system) for why a counter is needed at all.
- There is **no `stage_token`** and no per-stage gate table any more: survey completion and
  underwriting eligibility are read straight off collected data (below). The `stage_token` schema
  field still exists (backward compatible) but nothing reads or writes it.
- The verdict's `required_data_set` maps onto a `survey_type` via a small table
  (`checkrisksystemverdict.surveyTypeByDataSet`). Only three values are valid today: `underwriting-v1`
  → `underwriting`, `survey-normal-v1` → `normal`, `survey-high-risk-v1` → `high_risk`. The earlier
  granular data sets (`CUSTOMER_VERIFICATION`/`ASSET_REVIEW`/`FINANCING`/`INCOME_REVIEW`) and the
  retired `FINAL_REVIEW` value are all rejected as independently selectable survey types and now fail
  safe like any other unrecognised value — they're only reachable as pages inside `normal`/`high_risk`.
- `underwriting` (`required_data_set=underwriting-v1`) is gated beyond the ordinary mapping: it's only
  reachable for a document that completed the `high_risk` survey (`process.environment_check.result`
  is present — only `high_risk`'s final page writes it) **and** whose
  `process.loan_structure.product_type` is `NDF4W` — never from `normal`, never for `NDF2W`
  (`survey.IsUnderwritingEligible`). `product_type` is part of what LPW's (simulated) call to RS
  includes (`checkrisksystem.requiredReadSet`), so a correctly-behaving RS should never request
  `underwriting-v1` outside that condition — but if it does anyway (bug, or a deliberately
  inconsistent test verdict, see TC4), `check_risk_system_pg_verdict` refuses to write
  `survey_type=underwriting` and returns an error instead, which Temporal retries indefinitely,
  surfacing as a visibly stuck/failing activity rather than a silent stall or an automatic loan
  rejection.
- `normal` and `high_risk` are multi-page `SURVEY` task outcomes: every page but the last is a
  partial completion (`WorkerTaskCompletionData.Action == "partial"`) that leaves the task open and
  mints a fresh task id for the next page; only the final page closes it. `normal` spans 4 pages
  (identity, asset, financing, income); `high_risk` spans 5 (the same identity/asset/financing pages
  plus its own income page, plus a final `environment_check` page). Whether a survey type is done is
  read straight off collected data — its final page's field (`income.verified_amount` for `normal`,
  `environment_check.result` for `high_risk`, `underwriting.confirmed` for `underwriting`). The first
  three pages are collected identically by both outcomes — this is what lets Risk System escalate a
  document from `normal` to `high_risk` mid-flow without re-collecting pages already submitted. See
  `tasking/survey.standardSurveyPages`.
- `calculate_risk_funding_pg` consumes the verdict's max LTV and caps the submitted LTV once
  financing data exists.

The upstream RS call is intentionally a local stub. The trigger records a request id
(`playground-rs-<uuid>`) so the planner and lock behavior can be exercised without an LGS proxy
contract.

## Scoring intake chain

Before any of the above runs, three ordinary (non-async) activities gate entry into scoring:

1. `check_submission_pg` — the document's first status transition (`$.status = "new"`); no read
   dependencies beyond the document id.
2. `check_customer_eligibility_pg` — age check (18–65) off `customer.birth_date`; writes
   `process.age_check_passed`, or rejects the document outright if the customer is out of range.
3. `check_duplicate_license_plate_pg` — rejects a `process.asset.license_plate` already on an open
   application; writes `process.duplicate_plate_check_passed`.

Once both checks pass, `seed_scoring_checkpoint_pg` seeds `trigger_seq=0` — and never again
(`shouldSeed` refuses once `trigger_seq` already exists; see `TestShouldSeedNeverReseeds`).
`check_risk_system_pg` then fires immediately (`checkrisksystem.intakeGate` requires both flags
present **and** `true`) — **before any survey runs**.

## Required data set selects the survey type

Risk System does not just gate progress — its `required_data_set` verdict field tells LORA **which
survey the user must complete next**. Internally RS derives this from its own risk-level calculation
(a blackbox to LORA, per the design tenet); LORA only consumes the resulting identifier.
`check_risk_system_pg` records that identifier verbatim; `check_risk_system_pg_verdict` then maps it
onto a survey type via a small declarative table (`checkrisksystemverdict.surveyTypeByDataSet`):

| `required_data_set` | `survey_type` | outcome |
|---|---|---|
| `underwriting-v1` | `underwriting` | single-page survey confirming `process.underwriting.confirmed` — only eligible once the `high_risk` survey is complete (`environment_check.result` present) for the `NDF4W` product; never from `normal`, never for `NDF2W` (see TC2, TC4) |
| `survey-normal-v1` | `normal` | 4-page survey: identity → asset → financing → income |
| `survey-high-risk-v1` | `high_risk` | 5-page survey: identity → asset → financing → income → environment_check |

`status=pending` maps internally to `$.status = "processing"` (`translateStatus`); `approved` and
`rejected` pass through unchanged and also stamp the matching `status_timestamps` field plus
`status_timestamps.terminal`.

### The multi-page survey outcomes

`normal` and `high_risk` are the only two ways a document reaches the survey pages below
`underwriting` — the single-page granular survey types (`identity`/`asset`/`financing`/`income`) that
used to be independently selectable no longer exist as entries in `surveyOutcomesByType`.

Each page's task completion writes that page's own findings **plus** `trigger_seq` (previous + 1) —
see `survey.SurveyPageCompletion`.

### What re-asks Risk System

`check_risk_system_pg` is asked once when intake passes (after the seed), then **after every survey
page**, because every page bumps `trigger_seq`. While it's pending it holds a read lock on
`trigger_seq`, which is in the survey's write set, so the survey can't open its next page until the
verdict lands (`FieldLock.CanSetWrite`). A verdict must follow every page to keep a multi-page survey
moving.

#### Why a counter is needed

This is SDK behaviour, not a playground choice. A step that has already run is put back on the
planner's run list only by `planner.Rollback` (or, for a task, by its own partial completion), and
`Rollback` is invoked only for fields in `DocSetResult.Updated` — a field that already had a value
and got a different one (`runtime/workflow.go` completion handling, `versioned_value.go`'s `Set`).
A first-time write lands in `NewlySet` instead, which `Rollback` never uses as a trigger. Most survey
pages only set data for the first time (`asset.condition`, `income.verified_amount`,
`environment_check.result`, `underwriting.confirmed`), so without a counter they would never re-ask
RS. `trigger_seq` is seeded to `0` precisely so page 1's write is already an update. `data-set` and
`data-forward`/`data-forward-override` workflow updates never call `Rollback` at all, so they can't
re-ask RS either — only activity/task completions can. The alternatives that were weighed (and why an
SDK "re-arm on first set" flag was rejected — it breaks planner convergence) are recorded in
[RISK_SYSTEM_RETRIGGER_OPTIONS.md](RISK_SYSTEM_RETRIGGER_OPTIONS.md).

The same rule applies downstream, with one more wrinkle: `calculate_risk_funding_pg` recomputes when
something it reads is *updated* — including through a later page's `trigger_seq` bump, via the
rollback impact walk — but **not after a terminal verdict**: the SDK skips rollback once the planner
is terminating. A cap RS sends or changes on the terminal (approve/reject) verdict itself is recorded
but never applied, so send it on an earlier, non-terminal verdict (TC1 sends it on the page-3
verdict).

Because `identity`/`asset`/`financing` are the same pages between `normal` and `high_risk`
(`survey.standardSurveyPages`), Risk System can escalate a document from `normal` to `high_risk` on
**any** verdict, including one sent mid-survey: `shouldCreateTask` only checks whether the *current*
`survey_type`'s completion field is present, so pages already collected simply stay collected, and
the task stays open to collect whatever the new outcome still needs
(`TestShouldCreateTaskContinuesAcrossSurveyTypeEscalation`,
`TestNormalAndHighRiskSharePagesBeforeDiverging`; TC3 below exercises this end to end).

An unrecognised `required_data_set` fails safe rather than guessing a survey type. This matters
because RS evolves on its own release cadence while an in-flight application can stay on an older
worker version. Since the verdict is now delivered as a raw Temporal activity completion rather than
a schema-validated data-forward update, there is no longer a synchronous client-side rejection point
built into the workflow itself — `cmd/testcli`'s `verdict` command validates client-side
(`checkrisksystemverdict.ValidStatus`/`ValidRequiredDataSet`) before ever calling
`CompleteActivityByID`, mirroring where LGS's proxy schema gate would sit in production; the
`check_risk_system_pg_verdict` activity's own `surveyTypeForDataSet` fail-safe (unit-tested directly by
`TestSurveyTypeForDataSet`) is what still protects the raw Temporal path, where an invalid value fails
that activity (and Temporal retries it) rather than being cleanly refused.

The `underwriting-v1` eligibility check (`survey.IsUnderwritingEligible`) is a second, distinct
fail-safe layered on top of the mapping table above: even a *recognised* `required_data_set` value can
still be rejected if the document's own state (`environment_check.result`, `product_type`) doesn't
back it up. See TC4.

## Local flow

1. Start LSS with the bundled playground schema available:

   ```
   cd lora-schema-service && go run cmd/http/main.go
   ```

2. If you don't already have Temporal and ArangoDB running, start them using the workspace
   development stack:

   ```
   cd lora-tools/docker/shared && docker compose up -d
   ```

3. Start this worker with `.env`:

   ```
   cd lora-process-worker-playground
   make env   # first time only, copies .env.example to .env
   go run ./cmd
   ```

   The gateway service does not need to be running: the SDK's system init only builds an HTTP client
   from `LORA_GATEWAY_BASE_SERVER_URL` and never calls it — no playground activity calls a proxy.

4. Submit an initial document through the SDK's built-in `data-set` update — the same one LGS calls
   in production for a fresh submission (`application/data_set.go`): `customer.name`, `customer.nik`,
   `customer.birth_date`, `process.asset.license_plate` (needed by the duplicate-plate check), the
   customer's originally requested `process.loan_structure.provisional_amount` / `ltv_submission`
   (needed by `calculate_risk_funding_pg`, and later revised by the financing survey page), and
   `process.loan_structure.product_type` (`NDF4W`/`NDF2W`, write-once — part of what LPW sends Risk
   System, and what the underwriting eligibility gate checks). `id` needs no submission: the SDK sets
   it from the workflow ID automatically at workflow start.
5. `check_submission_pg` → `check_customer_eligibility_pg` → `check_duplicate_license_plate_pg` run
   as before.
6. `seed_scoring_checkpoint_pg` seeds `trigger_seq=0` once both checks pass — **before any survey
   has run** — and `check_risk_system_pg` fires straight after.
7. Complete that pending activity with a verdict: `status=pending`,
   `required_data_set=survey-normal-v1`. `cmd/testcli verdict -id <id> -dataset survey-normal-v1` is
   the supported way to do this by hand — it discovers the pending activity's id and validates the
   payload first.
8. `check_risk_system_pg_verdict` maps that onto `survey_type=normal`; the `SURVEY` task opens,
   spanning 4 pages.
9. Page 1 (`identity`) — partial completion: `customer.birth_date`, `customer.name`, plus
   `trigger_seq=1`. The task stays open and mints a fresh task id for page 2; the `trigger_seq` bump
   re-asks RS. Verdict `survey-normal-v1` again keeps the survey going.
10. Repeat for page 2 (`asset`, `trigger_seq=2`) — RS is re-asked even though `asset.condition` is
    first-time data — and page 3 (`financing`, `trigger_seq=3`). Send the page-3 verdict with
    `max_ltv=0.6`: RS's lending cap.
11. Page 4 (`income`, `trigger_seq=4`) is the **final** page: it closes the task. Its bump re-runs
    `calculate_risk_funding_pg` under the cap — `effective_ltv=0.6`, `max_funding=60000` (same numbers
    as `TestCalculateFundingCapsSubmissionLTVAtRiskSystemMax`) — and re-asks RS.
12. Verdict `status=approved`, `required_data_set=survey-normal-v1`, `max_ltv=0.6`. The `normal` path
    terminates right here: it repeats `survey-normal-v1` rather than requesting `underwriting-v1`,
    since `underwriting` is reachable only from the `high_risk` path (`survey.IsUnderwritingEligible`)
    — `check_risk_system_pg_verdict` writes the terminal status and the workflow stops.

This is exactly `testcli tc tc1`; `tc2` runs the same shape but on `high_risk` from the first verdict
and continues on into the single-page `underwriting` survey; `tc3` starts on `normal` and gets
escalated to `high_risk` mid-flow, also reaching `underwriting`; `tc4` proves the underwriting gate
rejects an `NDF2W` applicant even after completing the `high_risk` survey; `tc5`/`tc6` prove rejection
is terminal — see the test cases below.

## Test cases

### Running a test case with `testcli`

`cmd/testcli` automates every scenario below end to end — it starts the workflow, sends each step's
`data-set` update, survey page/task completion, and verdict (activity completion) in order, and waits
for the worker's pending activities to settle between steps (so it never races ahead of what the
previous step triggered). It reads the same `.env` as the worker, so run it from a shell where the
worker's `.env` is present (or export the same `TEMPORAL_*` vars):

```
go run ./cmd/testcli tc tc1 -id poc-lead-0001
```

Swap `tc1` for `tc2`, `tc3`, `tc4`, `tc5`, or `tc6`. Add `-wait 5s` if your Temporal server is slower than the default
2-second settle timeout. Steps printed as `NOTE: ...` are things the tool cannot verify itself (it
registers no query handler on the *document* workflow — the task-master workflow has one, used
internally to find a pending task's id, but it doesn't expose document fields), so check them by hand
via the Temporal UI or the ArangoDB document.

`testcli` also has lower-level subcommands for building your own sequence by hand:

```
go run ./cmd/testcli start    -id poc-lead-0001
go run ./cmd/testcli inject   -id poc-lead-0001
go run ./cmd/testcli verdict  -id poc-lead-0001 -dataset survey-normal-v1
go run ./cmd/testcli complete-survey -id poc-lead-0001 -type normal -outcome partial -seq 1 \
  -field '$.customer.birth_date=1985-03-15' -field '$.customer.name=Jane Smith'
go run ./cmd/testcli verdict -id poc-lead-0001 -dataset survey-normal-v1
go run ./cmd/testcli override -id poc-lead-0001 -field '$.process.loan_structure.ltv_submission=0.5'
```

`inject` sets previously-unset fields via the SDK's built-in `data-set` update (no worker-side
handler — every worker gets it for free); it sends the default identity + asset + loan-structure
fields unless `-bare` is given, plus any `-field path=value` overrides (repeatable) — `value` is
parsed as a number, then a bool, then falls back to a string. `override` uses this worker's own
`data-forward-override` handler to overwrite a field that's *already* set (`data-set` refuses that
outright). Neither update re-asks Risk System, even when it changes a value
`check_risk_system_pg` reads — workflow updates never call `planner.Rollback` (see
[Why a counter is needed](#why-a-counter-is-needed)).

`verdict` completes the pending `check_risk_system_pg` activity directly
(`client.CompleteActivityByID`) after validating `-status` (`pending|approved|rejected` — default
`pending`) and `-dataset` (`underwriting-v1|survey-normal-v1|survey-high-risk-v1`) client-side; it
fails outright if Risk System was not (re-)asked (no pending activity to complete — e.g. a page
submitted without bumping `trigger_seq`). Completing it only makes `check_risk_system_pg` record the
raw verdict — a separate
`check_risk_system_pg_verdict` activity then interprets it, and for `underwriting-v1` may itself fail
(and retry) if the document isn't actually eligible; see TC4.

`complete-survey` simulates a human submitting one page of the open `SURVEY` task via `-type
underwriting|normal|high_risk`. **It does not enforce page order** — `tasksim`'s handler tracks only
"is there a pending task", not which page it's on — so `-outcome final -seq N` sends
`survey.SurveyOutcomeFields`' canned *last*-page fields (plus `trigger_seq=N`) and closes the task
immediately, which is convenient for skipping straight to the end of a multi-page outcome but will
leave the earlier pages' fields unset unless you provided them another way (e.g. `inject`/`override`,
or the `-field` overrides shown above). `-outcome partial -seq N` sends the `-field` overrides plus
`trigger_seq=N` — reproducing exactly what an automated `tc` scenario's `surveyPageStep` does for one
page means passing that page's fields, as in the example above. **`-seq` must be the previous value +
1** (seeded `0`, so the Nth page submitted writes `N`): re-sending the same value is not an update, so
RS is not re-asked and the next `verdict` fails. `describe -id <id>` prints the workflow's pending
activities (and any failure on them) — useful for confirming a step settled.

Note on "the workflow stops" below: that means the *planner* stops scheduling activities once
`$.status` reaches a terminal value (`doc.SetTermination`). With nothing left pending, the Temporal
execution then closes (`describe` reports `status=Completed`, as TC1–TC3, TC5 and TC6 do). If a
document were terminated while a survey task was still open or queued, it would instead stay
`status=Running` with a pending `termination-activity`, because `tasksim` never answers early task
termination (`TerminationNotification` is a no-op) — none of the scripted scenarios do that. A
document stuck non-terminal (TC4) stays `Running`.

### Doing it by hand with the `temporal` CLI

Every command below is a Temporal CLI call against the running worker. Both the task queue it polls
and its workflow type are `lpw-playground-v0_1_1` (`framework.System.LoadDocumentSchema` derives this
from the schema file name and passes it to both `RegisterAsWorker` and `RegisterWorkflow` — see
`system.go:174-178,313-315`). `data-forward-override` is this worker's own registered update
(`internal/process/workflow/functions.go`, `override.go`); `data-set` is the SDK's built-in update.
Replace `<id>` with whatever workflow ID you start with; `data-set`/`data-forward-override` take a
flat field map directly, with no `document_id` wrapper (the SDK sets `$.id` from the workflow ID at
start, and `data-set` refuses to touch an already-set field).

Note: the system also derives `task_lpw-playground-v0_1_1` (`system.go:178`) as
`TemporalTaskQueue`, but that only backs a second internal client (`NewClientForQueue`) — it is not
the queue the worker itself listens on, so don't target it from `temporal workflow start`.

Start the workflow once per test case:

```
temporal workflow start --task-queue lpw-playground-v0_1_1 --type lpw-playground-v0_1_1 --workflow-id <id>
```

Submit the default fields every case starts from:

```
temporal workflow update execute --workflow-id <id> --name data-set --input '{
  "$.customer.name": "John Placeholder",
  "$.customer.nik": "3201010101010001",
  "$.customer.birth_date": "1990-05-20",
  "$.process.asset.license_plate": "B5678ABC",
  "$.process.loan_structure.provisional_amount": 90000,
  "$.process.loan_structure.ltv_submission": 0.75,
  "$.process.loan_structure.product_type": "NDF4W"
}'
```

After that, `check_submission_pg` → `check_customer_eligibility_pg` →
`check_duplicate_license_plate_pg` → `seed_scoring_checkpoint_pg` → `check_risk_system_pg` run on
their own — no more commands needed until the first verdict.

**There is no `data-forward-risk-system-scored` update to call any more.** `check_risk_system_pg` is
a genuine async Temporal activity, so a verdict is delivered by completing that pending activity
directly:

```
# 1. Find the pending check_risk_system_pg activity's id
temporal workflow describe --workflow-id <id> -o json \
  | jq -r '.pendingActivities[] | select(.activityType.name=="check_risk_system_pg") | .activityId'

# 2. Complete it with the verdict payload (check `temporal activity complete --help` for exact
#    flags on your installed CLI version)
temporal activity complete --workflow-id <id> --activity-id <id-from-step-1> --result '{
  "status": "<approved|rejected|pending>",
  "required_data_set": "<underwriting-v1|survey-normal-v1|survey-high-risk-v1>",
  "max_ltv": <number>,
  "reject_reason": "<only for status=rejected>"
}'
```

Unlike a data-forward update, nothing validates this payload against the document schema before it
lands — `checkrisksystemverdict.ValidStatus`/`ValidRequiredDataSet` only run inside `cmd/testcli`'s own
`verdict` command, not on this raw path — so an invalid `status`/`required_data_set` sent this way
fails `check_risk_system_pg_verdict` (which Temporal retries) once it rejects it, rather than being
cleanly refused up front. `cmd/testcli verdict` is the safer way to do this by hand.

To overwrite a field that's already set, use this worker's own `data-forward-override` update
instead — `data-set` above only accepts fields that are still unset:

```
temporal workflow update execute --workflow-id <id> --name data-forward-override --input '{
  "document_id": "<id>",
  "fields": {
    "$.customer.birth_date": "1991-01-01"
  }
}'
```

Submitting a survey page by hand requires first locating the task-master workflow (by scanning the
document workflow's history for `create_task_master`'s result) and then the pending task's id (via
the task-master's `pending-task` query) — `cmd/testcli complete-survey`/`tc` already does both; there
is no short raw-CLI equivalent worth hand-rolling.

This worker registers no query handler on the **document** workflow (confirmed: no
`workflow.SetQueryHandler` call exists for reading the document back — only the task-master workflow
has one, and it only exposes pending task ids), so inspect document state with `testcli describe -id
<id>` (pending activities and failures only) or by reading the ArangoDB document directly.

### TC1 — `survey-normal-v1` runs a 4-page multi-page survey to approval

Proves the `required_data_set=survey-normal-v1` mapping end to end: a single `SURVEY` task spans four
pages, each page a partial completion (except the last), every page's `trigger_seq` bump re-asks
`check_risk_system_pg`, and the LTV cap still applies.

1. Start the workflow and inject, as above.
2. Verdict (intake passed): `status=pending`, `required_data_set=survey-normal-v1`, `max_ltv=0`. →
   `survey_type=normal` → one `SURVEY` task opens, spanning four pages.
3. Page 1 (`identity`) — partial: `customer.birth_date=1985-03-15`, `customer.name=Jane Smith`,
   `trigger_seq=1`. Task stays open, mints a fresh task id for page 2. The bump re-asks RS; a
   `survey-normal-v1` verdict keeps the survey going.
4. Page 2 (`asset`) — partial: `asset.condition=fair`, `trigger_seq=2`. Same re-ask/verdict cycle —
   the bump re-asks RS even though `asset.condition` is first-time data.
5. Page 3 (`financing`) — partial: `loan_structure.provisional_amount=100000`,
   `loan_structure.ltv_submission=0.8`, `trigger_seq=3`. Verdict `survey-normal-v1`, `max_ltv=0.6`
   sets RS's lending cap.
6. Page 4 (`income`) — **final**: `income.verified_amount=15000000`, `trigger_seq=4`. The task
   closes; the bump re-runs `calculate_risk_funding_pg` under the cap (`effective_ltv=0.6`,
   `max_funding=60000`, matching `TestCalculateFundingCapsSubmissionLTVAtRiskSystemMax`) and re-asks
   RS.
7. Verdict: `status=approved`, `required_data_set=survey-normal-v1`, `max_ltv=0.6`. The `normal` path
   terminates directly here rather than requesting `underwriting-v1`, since `underwriting` is
   reachable only from the `high_risk` path (`survey.IsUnderwritingEligible`) — see TC2/TC3/TC4 for
   that path. → `status=approved`, `status_timestamps.approved` and `status_timestamps.terminal` set,
   `loan_structure.max_funding=60000`. The workflow stops (`status=Completed`).

### TC2 — `survey-high-risk-v1` runs a 5-page multi-page survey from the start

Proves the `required_data_set=survey-high-risk-v1` mapping: `normal`'s shared pages plus its own
income page and a fifth, final `environment_check` page, selected from the very first verdict,
followed all the way through to the single-page `underwriting` survey.

1. Start the workflow and inject, as above (`product_type=NDF4W`, the default).
2. Verdict (intake passed): `status=pending`, `required_data_set=survey-high-risk-v1`, `max_ltv=0` —
   Risk System flags this applicant high risk immediately. → `survey_type=high_risk` → one `SURVEY`
   task opens, spanning five pages.
3. Pages 1–4 (`identity`, `asset`, `financing`, `income` — the fourth non-final for `high_risk`),
   `trigger_seq=1..4`, each followed by a `survey-high-risk-v1` verdict.
4. Page 5 (`environment_check`) — **final**: `environment_check.result=good`, `trigger_seq=5`. The
   task closes.
5. Verdict: `status=pending`, `required_data_set=underwriting-v1`, `max_ltv=0.6`.
   `check_risk_system_pg_verdict` checks `survey.IsUnderwritingEligible` — `environment_check.result`
   is present and the product is `NDF4W`, so it writes `survey_type=underwriting`. → the single-page
   `underwriting` survey opens.
6. Complete it: `underwriting.confirmed=true`, `trigger_seq=6`. The bump re-runs the funding
   calculation under the cap (`max_funding=60000`) and re-asks RS.
7. Verdict: `status=approved`, `required_data_set=underwriting-v1`, `max_ltv=0.6`. →
   `status=approved`, terminal timestamps set, `loan_structure.max_funding=60000`. The workflow stops.

### TC3 — Risk System escalates a document from `normal` to `high_risk` mid-flow

Proves the design property behind sharing pages between the two outcomes
(`TestNormalAndHighRiskSharePagesBeforeDiverging`,
`TestShouldCreateTaskContinuesAcrossSurveyTypeEscalation`): a document already partway through
`normal` can be switched onto `high_risk` without losing or re-collecting the pages it already
submitted, and still reaches `underwriting` once the (now `high_risk`) survey completes.

1. Start the workflow and inject, as above (`product_type=NDF4W`, the default).
2. Verdict (intake passed): `status=pending`, `required_data_set=survey-normal-v1` — starts as a
   standard applicant. → `survey_type=normal`.
3. Page 1 (`identity`, `trigger_seq=1`) — verdict `survey-normal-v1` keeps it going.
4. Page 2 (`asset`, `asset.condition=fair`, `trigger_seq=2`).
5. Instead of another `survey-normal-v1` verdict, Risk System's asset-condition read comes back bad:
   send a verdict with `required_data_set=survey-high-risk-v1`. `survey_type` flips to `high_risk`;
   `shouldCreateTask` sees `high_risk`'s completion field (`environment_check.result`) is still
   absent and keeps the task open — pages 1–2 are **not** re-submitted. The task continues at
   `high_risk`'s page 3.
6. Pages 3–4 (`financing`, `income`; `trigger_seq=3..4`), each followed by a `survey-high-risk-v1`
   verdict.
7. Page 5 (`environment_check`) — **final**, `trigger_seq=5`.
8. Verdict: `status=pending`, `required_data_set=underwriting-v1`, `max_ltv=0.6` — the document did
   start on `normal`, but it has now completed `high_risk` (`environment_check.result` present), so
   `survey.IsUnderwritingEligible` holds → `survey_type=underwriting`.
9. Complete it: `trigger_seq=6`.
10. Verdict: `status=approved`. → `status=approved`, terminal timestamps set,
    `loan_structure.max_funding=60000`. The workflow stops.

### TC4 — the underwriting gate rejects an `NDF2W` applicant even via the `high_risk` path

Proves `survey.IsUnderwritingEligible`'s product half of the gate: completing the `high_risk` survey
is necessary but not sufficient — `underwriting-v1` is still refused for a non-`NDF4W` applicant, and
that refusal is a visible, retrying activity failure, not a silent stall or an automatic rejection.

1. Start the workflow and inject **with `product_type=NDF2W`** (`testcli`'s
   `injectIdentityStepWithProductType("NDF2W")` — `product_type` is `mutable:false`, so this must be
   set at the very first `data-set`, not overridden later).
2. Verdict (intake passed): `status=pending`, `required_data_set=survey-high-risk-v1`. →
   `survey_type=high_risk`.
3. Pages 1–5 run exactly as in TC2 (`trigger_seq=1..5`), ending with `environment_check.result`
   present.
4. Verdict: `status=pending`, `required_data_set=underwriting-v1`, `max_ltv=0.6`.
   `check_risk_system_pg` records this raw verdict without any validation — that succeeds.
   `check_risk_system_pg_verdict` then runs `survey.IsUnderwritingEligible`, which is `false` (the
   path is right, the product isn't), so it returns an error instead of writing
   `survey_type=underwriting`. Temporal retries that activity indefinitely per its default retry
   policy.
5. `testcli describe -id <id>` (or the Temporal UI) now shows a pending, repeatedly-failing
   `check_risk_system_pg_verdict` activity for this workflow — not a clean stop, and not
   `pendingActivities=0`.
6. Attempting `complete-survey -type underwriting` fails: `survey_type` never became
   `"underwriting"`, so no `SURVEY` task for it was ever created — there is nothing to complete.
7. The document stays at `status=processing` indefinitely, with `survey_type` still `high_risk` —
   this is the intended outcome for a genuine Risk-System/LORA disagreement: visible and
   diagnosable, not silently stuck and not an unearned rejection of the applicant.

### TC5 — Risk System rejects the applicant immediately, before any survey runs

Proves `status=rejected` is a genuine terminal outcome, not just a value that happens to pass through
unchanged: `document.IsTerminalStatus`/`doc.SetTermination` stop the planner the instant
`check_risk_system_pg_verdict` writes `$.status=rejected`, even though the same write also maps
`required_data_set` onto `survey_type=normal` — the `SURVEY` task for that survey type never gets a
chance to open.

1. Start the workflow and inject, as above.
2. Verdict (intake passed): `status=rejected`, `required_data_set=survey-normal-v1`, `max_ltv=0`,
   `reject_reason="provisional amount exceeds applicant's income capacity"`. `check_risk_system_pg`
   records this verbatim; `check_risk_system_pg_verdict` maps `survey-normal-v1` → `survey_type=normal`
   and `rejected` → `$.status=rejected` in the same write, then sets `status_timestamps.rejected` and
   `status_timestamps.terminal` (`document.SetStatusTimestamp` treats `approved`/`rejected`
   identically) and `$.status_reason` from `reject_reason`. → `doc.SetTermination` sees `$.status` is
   terminal and stops the planner right there: no `create_task_master`/`SURVEY` task ever runs, despite
   `survey_type=normal` being on the document. `loan_structure.max_funding` stays unset (`max_ltv=0`
   was never meant to cap anything for a rejected applicant). The workflow stops; no further activity
   runs.

### TC6 — Risk System rejects the applicant only after the normal survey completes

Proves the same terminal property as TC5 but reached the long way round — after a full 4-page
`normal` survey cycle identical to TC1's, so a rejection is exactly as reachable from the end of a
survey as it is up front, not a special early-exit path.

1. Steps 1–6 run exactly as TC1's steps 1–6, except no cap: inject, verdict `survey-normal-v1`, then
   pages 1–4 (`trigger_seq=1..4`) each followed by a `survey-normal-v1`/`status=pending` verdict.
   Page 4 is final and closes the task.
2. Verdict for the completed survey: `status=rejected` (instead of TC1's `approved`),
   `required_data_set=survey-normal-v1`, `max_ltv=0`,
   `reject_reason="verified income insufficient to support requested financing"`. →
   `status=rejected`, `status_reason` set from `reject_reason`, `status_timestamps.rejected` and
   `status_timestamps.terminal` set — the same terminal handling TC1's `approved` verdict gets. The
   survey task had already closed, so nothing is left pending and the workflow closes
   (`status=Completed`).

---

An earlier generation of scenarios (since renumbered away, so its names are not reused here) drove
per-checkpoint verdicts against the retired granular survey types (`CUSTOMER_VERIFICATION`/
`ASSET_REVIEW`/`FINANCING`/`INCOME_REVIEW`) directly, including rejection/re-ask/seed-guard/LTV-floor
cases, and have been retired along with those types; `cmd/testcli`'s `scenarios` map now defines
`tc1`–`tc6` (`tc5`/`tc6` script the rejection path those retired scenarios used to cover). The
remaining properties they proved still hold and are still unit-tested where no scripted scenario
covers them:

- An unrecognised `required_data_set` fails safe — `TestSurveyTypeForDataSet`
  (`checkrisksystemverdict` package).
- The seed never re-fires once progress has been made — `TestShouldSeedNeverReseeds`.
- The funding cap never inflates the customer's own submitted LTV, only lowers it —
  `TestCalculateFundingKeepsLowerSubmissionLTV`.
- Underwriting is unreachable outside a completed `high_risk` survey + `NDF4W`, and fails safe (no
  task opened) rather
  than assuming eligibility, on both the survey side (`TestUnderwritingRequiresHighRiskPath`,
  `TestUnderwritingRequiresNdf4wProduct`, `TestShouldCreateTaskDefendsAgainstIneligibleUnderwriting` in
  the `survey` package) and the verdict-interpretation side (`TestApplyVerdictRequiresHighRiskPath`,
  `TestApplyVerdictRequiresNdf4wProduct` in `checkrisksystemverdict`).

The schema source is `lpw-playground-v0_1_1.schema.json` in `lora-schema-service`; the worker
constants are generated from that bundled schema.

The tests cover the important planner mechanics directly: every read of `check_risk_system_pg`
(required and optional) is a rollback trigger, `trigger_seq` among them and `stage_token` not read at
all, the first call is gated on both intake checks passing, the seed fires exactly once and never
re-seeds after progress is made, every survey page payload bumps `trigger_seq` and never writes
`stage_token`,
`required_data_set` maps onto a survey type (or fails safe for an unknown value, including the
now-retired granular ones and the retired `FINAL_REVIEW`), each survey type completes on its own
final page's field and nothing earlier, `normal` and `high_risk` share their first three pages so a
document can escalate between them without re-collecting data, underwriting is gated to a completed
`high_risk` survey and the `NDF4W` product specifically (enforced in `check_risk_system_pg_verdict`,
since `check_risk_system_pg`'s async handler has no document read access to do it itself), and max
LTV caps the submitted LTV. The calculation's max-funding output is marked re-execution-neutral,
following LPW's post-scoring calculator pattern for derived outputs.
