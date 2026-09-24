# Risk System scoring POC

This playground implements the current shape of the Risk System integration:

- `check_risk_system_pg` is one genuine async Temporal activity (`system.AsyncPayloadHandler`),
  reused for every checkpoint. It only "calls RS" and records what came back **verbatim**, onto its
  own `process.scoring.risk_system.*` fields (`request_id`, `status`, `required_data_set`, `max_ltv`,
  `reject_reason`) — no interpretation. A verdict is delivered by directly completing the pending
  activity (`client.CompleteActivityByID`), which is what `cmd/testcli`'s `verdict` subcommand does.
- `apply_risk_system_verdict_pg` (`internal/process/scoring/applyrisksystemverdict`) is a separate,
  ordinary **synchronous** activity that reads those raw fields back and is the one place that
  interprets them: `required_data_set` → `survey_type`, `status` → `$.status`, terminal timestamps,
  `max_ltv`. It has to be a separate, non-async activity because `check_risk_system_pg`'s async
  handler (`runtime.AsyncPayloadHandler = func(map[string]any) (map[common.HString]any, error)`) only
  ever receives the raw external payload, never current document state — and the underwriting
  eligibility gate below (`stage_token`/`product_type`) needs exactly that.
- `trigger_seq` and `stage_token` are `check_risk_system_pg`'s required reads and the monotonic
  cursor. Every survey page write — not just a multi-page survey's final page — bumps both, so every
  page submission deterministically re-arms `check_risk_system_pg`.
- `stage_token` selects a deterministic data-availability gate table (`checkrisksystem.stageGates`),
  one entry per checkpoint.
- Payload fields are optional and use `OptionalIgnoreIfLocked`.
- The verdict's `required_data_set` maps onto a `survey_type` via a small table
  (`applyrisksystemverdict.surveyTypeByDataSet`). Only three values are valid today: `underwriting-v1`
  → `underwriting`, `survey-normal-v1` → `normal`, `survey-high-risk-v1` → `high_risk`. The earlier
  granular data sets (`CUSTOMER_VERIFICATION`/`ASSET_REVIEW`/`FINANCING`/`INCOME_REVIEW`) and the
  retired `FINAL_REVIEW` value are all rejected as independently selectable survey types and now fail
  safe like any other unrecognised value — they're only reachable as pages inside `normal`/`high_risk`.
- `underwriting` (`required_data_set=underwriting-v1`) is gated beyond the ordinary mapping: it's only
  reachable for a document that completed the `high_risk` survey (`stage_token ==
  "complete_high_risk_survey"`) **and** whose `process.loan_structure.product_type` is `NDF4W` — never
  from `normal`, never for `NDF2W` (`survey.IsUnderwritingEligible`). `product_type` is part of what
  LPW's (simulated) call to RS includes (`checkrisksystem.requiredReadSet`), so a correctly-behaving
  RS should never request `underwriting-v1` outside that condition — but if it does anyway (bug, or a
  deliberately inconsistent test verdict, see TC11), `apply_risk_system_verdict_pg` refuses to write
  `survey_type=underwriting` and returns an error instead, which Temporal retries indefinitely,
  surfacing as a visibly stuck/failing activity rather than a silent stall or an automatic loan
  rejection.
- `normal` and `high_risk` are multi-page `SURVEY` task outcomes: every page but the last is a
  partial completion (`WorkerTaskCompletionData.Action == "partial"`) that leaves the task open and
  mints a fresh task id for the next page; only the final page closes it. `normal` spans 4 pages
  (identity, asset, financing, income); `high_risk` spans 5 (the same identity/asset/financing pages
  plus its own income page, plus a final `environment_check` page). The first three pages are
  collected identically (same page name, same `stage_token`) by both outcomes — this is what lets
  Risk System escalate a document from `normal` to `high_risk` mid-flow without re-collecting pages
  already submitted. See `tasking/survey.standardSurveyPages`.
- `calculate_risk_funding_pg` consumes the verdict's max LTV and caps the submitted LTV once
  financing data exists.
- `seed_scoring_checkpoint_pg` pre-seeds the cursor at `post_submission` once both intake checks
  pass, so the first Risk System call happens **before any survey runs**.

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

Once both checks pass, `seed_scoring_checkpoint_pg` seeds `trigger_seq=0` / `stage_token=post_submission`
— and never again (`shouldSeed` refuses once `trigger_seq` already exists; see
`TestShouldSeedNeverReseeds`). `check_risk_system_pg` then fires immediately for `post_submission`,
since that checkpoint's gate is empty.

## Required data set selects the survey type

Risk System does not just gate progress — its `required_data_set` verdict field tells LORA **which
survey the user must complete next**. Internally RS derives this from its own risk-level calculation
(a blackbox to LORA, per the design tenet); LORA only consumes the resulting identifier.
`check_risk_system_pg` records that identifier verbatim; `apply_risk_system_verdict_pg` then maps it
onto a survey type via a small declarative table (`applyrisksystemverdict.surveyTypeByDataSet`):

| `required_data_set` | `survey_type` | outcome |
|---|---|---|
| `underwriting-v1` | `underwriting` | single-page survey confirming `process.underwriting.confirmed` — only reachable from the `high_risk` path (`stage_token == complete_high_risk_survey`) for the `NDF4W` product; `normal` and `NDF2W` applicants terminate at their own path's completion verdict instead (see TC8, TC11) |
| `survey-normal-v1` | `normal` | 4-page survey: identity → asset → financing → income |
| `survey-high-risk-v1` | `high_risk` | 5-page survey: identity → asset → financing → income → environment_check |

`status=pending` maps internally to `$.status = "processing"` (`translateStatus`); `approved` and
`rejected` pass through unchanged and also stamp the matching `status_timestamps` field plus
`status_timestamps.terminal`.

### The multi-page survey outcomes

`normal` and `high_risk` are the only two ways a document reaches any of the checkpoints below
`underwriting` — the single-page granular survey types (`identity`/`asset`/`financing`/`income`) that
used to be independently selectable no longer exist as entries in `surveyOutcomesByType`.

Every page's task completion writes that page's own findings **plus** `trigger_seq` (incremented)
and `stage_token` (that page's checkpoint) — see `survey.SurveyPageCompletion`. Since those two
fields are `check_risk_system_pg`'s required reads, submitting *any* page re-arms it, not just the
last one; a verdict must follow every page to keep a multi-page survey moving, holding a write lock
on `survey_type` until it's delivered.

| checkpoint (`stage_token`) | reached after | gate field |
|---|---|---|
| `post_submission` | seeding | none — fires immediately |
| `customer_verification` | page 1 (`identity`) of `normal`/`high_risk` | `customer.birth_date` |
| `asset_review` | page 2 (`asset`) of `normal`/`high_risk` | `process.asset.condition` |
| `financing_confirmation` | page 3 (`financing`) of `normal`/`high_risk` | `process.loan_structure.ltv_submission` |
| `complete_normal_survey` | page 4/final (`income`) of `normal` | `process.income.verified_amount` |
| `income_confirmation` | page 4 (`income`) of `high_risk` (non-final) | `process.income.verified_amount` |
| `complete_high_risk_survey` | page 5/final (`environment_check`) of `high_risk` | `process.environment_check.result` |
| `underwriting` | the single-page `underwriting` survey (`high_risk` + `NDF4W` only) | `process.underwriting.confirmed` |

(`checkrisksystem.stageGates` also still defines a bare `financing` entry gated on
`ltv_submission` — a holdover from before `financing_confirmation` existed. No current survey
outcome advances `stage_token` to `financing` any more, so it isn't reachable through `normal` or
`high_risk` today.)

Because `identity`/`asset`/`financing` share the same page name and `stage_token` between `normal`
and `high_risk` (`survey.standardSurveyPages`), Risk System can escalate a document from `normal` to
`high_risk` on **any** verdict, including one sent mid-survey: `shouldCreateTask` only compares the
*current* `survey_type`'s `nextStage` against the *current* `stage_token`, so an already-satisfied
shared checkpoint is simply never re-asked, and the task stays open to collect whatever the new
outcome still needs (`TestShouldCreateTaskContinuesAcrossSurveyTypeEscalation`,
`TestNormalAndHighRiskSharePagesBeforeDiverging`; TC10 below exercises this end to end).

An unrecognised `required_data_set` fails safe rather than guessing a survey type. This matters
because RS evolves on its own release cadence while an in-flight application can stay on an older
worker version. Since the verdict is now delivered as a raw Temporal activity completion rather than
a schema-validated data-forward update, there is no longer a synchronous client-side rejection point
built into the workflow itself — `cmd/testcli`'s `verdict` command validates client-side
(`applyrisksystemverdict.ValidStatus`/`ValidRequiredDataSet`) before ever calling
`CompleteActivityByID`, mirroring where LGS's proxy schema gate would sit in production; the
`apply_risk_system_verdict_pg` activity's own `surveyTypeForDataSet` fail-safe (unit-tested directly by
`TestSurveyTypeForDataSet`) is what still protects the raw Temporal path, where an invalid value fails
that activity (and Temporal retries it) rather than being cleanly refused.

The `underwriting-v1` eligibility check (`survey.IsUnderwritingEligible`) is a second, distinct
fail-safe layered on top of the mapping table above: even a *recognised* `required_data_set` value can
still be rejected if the document's own state (`stage_token`, `product_type`) doesn't back it up. See
TC11.

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
6. `seed_scoring_checkpoint_pg` pre-seeds `trigger_seq=0`, `stage_token=post_submission` once both
   checks pass — **before any survey has run**.
7. `check_risk_system_pg` fires immediately for `post_submission` (its gate has no required fields).
8. Complete that pending activity with a verdict: `status=pending`,
   `required_data_set=survey-normal-v1`. `cmd/testcli verdict -id <id> -dataset survey-normal-v1` is
   the supported way to do this by hand — it discovers the pending activity's id and validates the
   payload first.
9. `check_risk_system_pg`'s async handler maps that onto `survey_type=normal`; the `SURVEY` task
   opens, spanning 4 pages.
10. Page 1 (`identity`) — partial completion: `customer.birth_date`, `customer.name`, plus
    `trigger_seq=1`/`stage_token=customer_verification`. The task stays open and mints a fresh task
    id for page 2; the cursor advance re-arms `check_risk_system_pg`.
11. Verdict for the re-armed trigger (`survey-normal-v1` again) completes it and keeps the normal
    survey going. Repeat for page 2 (`asset` → `asset_review`) and page 3 (`financing` →
    `financing_confirmation`).
12. Page 4 (`income`) is the **final** page: it closes the task and advances the cursor to
    `complete_normal_survey`.
13. Verdict for `complete_normal_survey`: `status=approved`, `required_data_set=survey-normal-v1`,
    `max_ltv=0.6` — this is the verdict that actually caps the loan. `calculate_risk_funding_pg`
    fires and computes `effective_ltv=0.6`, `max_funding=60000` (same numbers as
    `TestCalculateFundingCapsSubmissionLTVAtRiskSystemMax`). The `normal` path terminates right here:
    it repeats `required_data_set=survey-normal-v1` rather than requesting `underwriting-v1`, since
    `underwriting` is reachable only from the `high_risk` path
    (`survey.IsUnderwritingEligible`) — `apply_risk_system_verdict_pg` writes the terminal status and
    the workflow stops. There is no step 14 for the `normal` path.

This is exactly `testcli tc tc8`; `tc9` runs the same shape but on `high_risk` from the first verdict
and continues on into the single-page `underwriting` survey (see TC9 below); `tc10` starts on `normal`
and gets escalated to `high_risk` mid-flow, also reaching `underwriting`; `tc11` proves the
underwriting gate rejects an `NDF2W` applicant even after completing the `high_risk` survey — see the
test cases below.

## Test cases

### Running a test case with `testcli`

`cmd/testcli` automates every scenario below end to end — it starts the workflow, sends each step's
`data-set` update, survey page/task completion, and verdict (activity completion) in order, and waits
for the worker's pending activities to settle between steps (so it never races ahead of what the
previous step triggered). It reads the same `.env` as the worker, so run it from a shell where the
worker's `.env` is present (or export the same `TEMPORAL_*` vars):

```
go run ./cmd/testcli tc tc8 -id poc-lead-0001
```

Swap `tc8` for `tc9`, `tc10`, or `tc11`. Add `-wait 5s` if your Temporal server is slower than the default
2-second settle timeout. Steps printed as `NOTE: ...` are things the tool cannot verify itself (it
registers no query handler on the *document* workflow — the task-master workflow has one, used
internally to find a pending task's id, but it doesn't expose document fields), so check them by hand
via the Temporal UI or the ArangoDB document.

`testcli` also has lower-level subcommands for building your own sequence by hand:

```
go run ./cmd/testcli start    -id poc-lead-0001
go run ./cmd/testcli inject   -id poc-lead-0001
go run ./cmd/testcli verdict  -id poc-lead-0001 -dataset survey-normal-v1
go run ./cmd/testcli complete-survey -id poc-lead-0001 -type normal -outcome partial \
  -field '$.customer.birth_date=1985-03-15' -field '$.customer.name=Jane Smith' \
  -field '$.process.scoring.trigger_seq=1' -field '$.process.scoring.stage_token=customer_verification'
go run ./cmd/testcli verdict -id poc-lead-0001 -dataset underwriting-v1 -max-ltv 0.6
go run ./cmd/testcli override -id poc-lead-0001 -field '$.process.loan_structure.ltv_submission=0.5'
```

`inject` sets previously-unset fields via the SDK's built-in `data-set` update (no worker-side
handler — every worker gets it for free); it sends the default identity + asset + loan-structure
fields unless `-bare` is given, plus any `-field path=value` overrides (repeatable) — `value` is
parsed as a number, then a bool, then falls back to a string. `override` uses this worker's own
`data-forward-override` handler to overwrite a field that's *already* set (`data-set` refuses that
outright).

`verdict` completes the pending `check_risk_system_pg` activity directly
(`client.CompleteActivityByID`) after validating `-status` (`pending|approved|rejected` — default
`pending`) and `-dataset` (`underwriting-v1|survey-normal-v1|survey-high-risk-v1`) client-side; it
fails outright if Risk System was never asked about the current stage (no pending activity to
complete). Completing it only makes `check_risk_system_pg` record the raw verdict — a separate
`apply_risk_system_verdict_pg` activity then interprets it, and for `underwriting-v1` may itself fail
(and retry) if the document isn't actually eligible; see TC11.

`complete-survey` simulates a human submitting one page of the open `SURVEY` task via `-type
underwriting|normal|high_risk`. **It does not enforce page order** — `tasksim`'s handler tracks only
"is there a pending task", not which page it's on — so `-outcome final -seq N` sends
`survey.SurveyOutcomeFields`' canned *last*-page fields (plus the cursor) and closes the task
immediately, which is convenient for skipping straight to the end of a multi-page outcome but will
leave the earlier pages' fields unset unless you provided them another way (e.g. `inject`/`override`,
or the `-field` overrides shown above). `-outcome partial` sends only `-field` overrides, with no
cursor fields added automatically — reproducing exactly what an automated `tc` scenario's
`surveyPageStep` does for one page means including `trigger_seq`/`stage_token` in `-field` yourself,
as in the example above. `describe -id <id>` prints the workflow's pending activities (and any
failure on them) — useful for confirming a step settled.

Note on "the workflow stops" below: that means the *planner* stops scheduling activities once
`$.status` reaches a terminal value (`doc.SetTermination`) — `describe` will keep reporting
`status=Running` indefinitely, since the underlying Temporal workflow execution is designed to stay
open for the life of the document, not to close itself. `pendingActivities=0` is what confirms
nothing more will run.

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
lands — `applyrisksystemverdict.ValidStatus`/`ValidRequiredDataSet` only run inside `cmd/testcli`'s own
`verdict` command, not on this raw path — so an invalid `status`/`required_data_set` sent this way
fails `apply_risk_system_verdict_pg` (which Temporal retries) once it rejects it, rather than being
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

### TC8 — `survey-normal-v1` runs a 4-page multi-page survey to approval

Proves the `required_data_set=survey-normal-v1` mapping end to end: a single `SURVEY` task spans four
pages, each page a partial completion (except the last), every page's cursor advance re-arms
`check_risk_system_pg`, and the LTV cap still applies.

1. Start the workflow and inject, as above.
2. Verdict for `post_submission`: `status=pending`, `required_data_set=survey-normal-v1`,
   `max_ltv=0`. → `survey_type=normal` → one `SURVEY` task opens, spanning four pages.
3. Page 1 (`identity`) — partial: `customer.birth_date=1985-03-15`, `customer.name=Jane Smith`,
   `trigger_seq=1`, `stage_token=customer_verification`. Task stays open, mints a fresh task id for
   page 2. This re-arms `check_risk_system_pg`; a `survey-normal-v1` verdict completes it and keeps
   the survey going.
4. Page 2 (`asset`) — partial: `asset.condition=fair`, `trigger_seq=2`, `stage_token=asset_review`.
   Same re-arm/verdict cycle.
5. Page 3 (`financing`) — partial: `loan_structure.provisional_amount=100000`,
   `loan_structure.ltv_submission=0.8`, `trigger_seq=3`, `stage_token=financing_confirmation`. Same
   re-arm/verdict cycle.
6. Page 4 (`income`) — **final**: `income.verified_amount=15000000`, `trigger_seq=4`,
   `stage_token=complete_normal_survey`. The task closes.
7. Verdict for `complete_normal_survey`: `status=approved`, `required_data_set=survey-normal-v1`,
   `max_ltv=0.6` — `calculate_risk_funding_pg` fires and computes `effective_ltv=0.6`,
   `max_funding=60000` (matches `TestCalculateFundingCapsSubmissionLTVAtRiskSystemMax`). The `normal`
   path terminates directly here rather than requesting `underwriting-v1`, since `underwriting` is
   reachable only from the `high_risk` path (`survey.IsUnderwritingEligible`) — see TC9/TC10/TC11 for
   that path. → `status=approved`, `status_timestamps.approved` and `status_timestamps.terminal` set,
   `loan_structure.max_funding=60000`. The workflow stops; no further activity runs.

### TC9 — `survey-high-risk-v1` runs a 5-page multi-page survey from the start

Proves the `required_data_set=survey-high-risk-v1` mapping: `normal`'s four shared pages plus a
fifth, final `environment_check` page, selected from the very first verdict, followed all the way
through to the single-page `underwriting` survey.

1. Start the workflow and inject, as above (`product_type=NDF4W`, the default).
2. Verdict for `post_submission`: `status=pending`, `required_data_set=survey-high-risk-v1`,
   `max_ltv=0` — Risk System flags this applicant high risk immediately. →
   `survey_type=high_risk` → one `SURVEY` task opens, spanning five pages.
3. Pages 1–3 (`identity` → `customer_verification`, `asset` → `asset_review`, `financing` →
   `financing_confirmation`) run exactly as in TC8 — these three pages are collected identically by
   `normal` and `high_risk` (`standardSurveyPages`) — each partial completion re-arming
   `check_risk_system_pg`, each followed by a `survey-high-risk-v1` verdict.
4. Page 4 (`income`) — partial (not final, unlike `normal`): `income.verified_amount=15000000`,
   `trigger_seq=4`, `stage_token=income_confirmation`. Task stays open for the fifth page.
5. Page 5 (`environment_check`) — **final**: `environment_check.result=good`, `trigger_seq=5`,
   `stage_token=complete_high_risk_survey`. The task closes.
6. Verdict for `complete_high_risk_survey`: `status=pending`, `required_data_set=underwriting-v1`,
   `max_ltv=0.6` — same LTV cap as TC8: `effective_ltv=0.6`, `max_funding=60000`.
   `apply_risk_system_verdict_pg` checks `survey.IsUnderwritingEligible("complete_high_risk_survey",
   "NDF4W")` — both conditions hold, so it writes `survey_type=underwriting`. →
   the single-page `underwriting` survey opens.
7. Complete it: `underwriting.confirmed=true`, `trigger_seq=6`, `stage_token=underwriting`.
8. Verdict for `underwriting`: `status=approved`, `required_data_set=underwriting-v1`, `max_ltv=0.6`. →
   `status=approved`, terminal timestamps set, `loan_structure.max_funding=60000`. The workflow stops.

### TC10 — Risk System escalates a document from `normal` to `high_risk` mid-flow

Proves the design property behind sharing pages between the two outcomes
(`TestNormalAndHighRiskSharePagesBeforeDiverging`,
`TestShouldCreateTaskContinuesAcrossSurveyTypeEscalation`): a document already partway through
`normal` can be switched onto `high_risk` without losing or re-collecting the pages it already
submitted, and still reaches `underwriting` once the (now `high_risk`) survey completes.

1. Start the workflow and inject, as above (`product_type=NDF4W`, the default).
2. Verdict for `post_submission`: `status=pending`, `required_data_set=survey-normal-v1`,
   `max_ltv=0` — starts as a standard applicant. → `survey_type=normal`.
3. Page 1 (`identity`) — partial, `trigger_seq=1`, `stage_token=customer_verification`. Verdict for
   `survey-normal-v1` keeps it going.
4. Page 2 (`asset`) — partial, `asset.condition=fair`, `trigger_seq=2`, `stage_token=asset_review`.
5. Instead of another `survey-normal-v1` verdict, Risk System's asset-condition read comes back bad:
   send a verdict with `required_data_set=survey-high-risk-v1`. `survey_type` flips to `high_risk`
   while `stage_token` is still `asset_review`. Because `identity`/`asset`/`financing` are the same
   pages (same name, same `stage_token`) under both outcomes, `shouldCreateTask` sees `stage_token`
   (`asset_review`) is not `high_risk`'s `nextStage` (`complete_high_risk_survey`) and keeps the task
   open — pages 1–2 are **not** re-submitted. The task continues at `high_risk`'s page 3.
6. Page 3 (`financing`) — partial, `trigger_seq=3`, `stage_token=financing_confirmation`. Verdict for
   `survey-high-risk-v1` keeps it going.
7. Page 4 (`income`) — partial, `trigger_seq=4`, `stage_token=income_confirmation`. Verdict for
   `survey-high-risk-v1` keeps it going.
8. Page 5 (`environment_check`) — **final**, `trigger_seq=5`, `stage_token=complete_high_risk_survey`.
9. Verdict for `complete_high_risk_survey`: `status=pending`, `required_data_set=underwriting-v1`,
   `max_ltv=0.6` — `effective_ltv=0.6`, `max_funding=60000`. This document did start on `normal`, but
   by the time this verdict lands `stage_token=complete_high_risk_survey` (it escalated at step 5), so
   `survey.IsUnderwritingEligible` still holds → `survey_type=underwriting`.
10. Complete it: `trigger_seq=6`, `stage_token=underwriting`.
11. Verdict for `underwriting`: `status=approved`. → `status=approved`, terminal timestamps set,
    `loan_structure.max_funding=60000`. The workflow stops.

### TC11 — the underwriting gate rejects an `NDF2W` applicant even via the `high_risk` path

Proves `survey.IsUnderwritingEligible`'s product half of the gate: completing the `high_risk` survey
is necessary but not sufficient — `underwriting-v1` is still refused for a non-`NDF4W` applicant, and
that refusal is a visible, retrying activity failure, not a silent stall or an automatic rejection.

1. Start the workflow and inject **with `product_type=NDF2W`** (`testcli`'s
   `injectIdentityStepWithProductType("NDF2W")` — `product_type` is `mutable:false`, so this must be
   set at the very first `data-set`, not overridden later).
2. Verdict for `post_submission`: `status=pending`, `required_data_set=survey-high-risk-v1`,
   `max_ltv=0`. → `survey_type=high_risk`.
3. Pages 1–5 run exactly as in TC9, ending with `stage_token=complete_high_risk_survey`.
4. Verdict for `complete_high_risk_survey`: `status=pending`, `required_data_set=underwriting-v1`,
   `max_ltv=0.6`. `check_risk_system_pg` records this raw verdict without any validation — that
   succeeds. `apply_risk_system_verdict_pg` then runs `survey.IsUnderwritingEligible(
   "complete_high_risk_survey", "NDF2W")`, which is `false` (the path is right, the product isn't), so
   it returns an error instead of writing `survey_type=underwriting`. Temporal retries that activity
   indefinitely per its default retry policy.
5. `testcli describe -id <id>` (or the Temporal UI) now shows a pending, repeatedly-failing
   `apply_risk_system_verdict_pg` activity for this workflow — not a clean stop, and not
   `pendingActivities=0`.
6. Attempting `complete-survey -type underwriting` fails: `survey_type` never became `"underwriting"`,
   so no `SURVEY` task for it was ever created — there is nothing to complete.
7. The document stays at `status=processing` indefinitely, with `survey_type` still `high_risk` and
   `stage_token` still `complete_high_risk_survey` (confirm via the ArangoDB document) — this is the
   intended outcome for a genuine Risk-System/LORA disagreement: visible and diagnosable, not silently
   stuck and not an unearned rejection of the applicant.

---

TC1–TC7 (per-checkpoint verdicts against `CUSTOMER_VERIFICATION`/`ASSET_REVIEW`/`FINANCING`/
`INCOME_REVIEW`, the rejection/re-ask/seed-guard/LTV-floor scenarios) drove the granular survey
types directly and have been retired along with them; `cmd/testcli`'s `scenarios` map now defines
`tc8`–`tc11`. The properties they proved still hold and are still unit-tested where the scripted
scenario itself doesn't cover them any more:

- Rejection is a terminal state, not a dead end — `translateStatus`/`document.SetStatusTimestamp`
  treat `approved`/`rejected` identically once mapped.
- An unrecognised `required_data_set` fails safe — `TestSurveyTypeForDataSet`
  (`applyrisksystemverdict` package).
- The seed never re-fires once progress has been made — `TestShouldSeedNeverReseeds`.
- The funding cap never inflates the customer's own submitted LTV, only lowers it —
  `TestCalculateFundingKeepsLowerSubmissionLTV`.
- Underwriting is unreachable outside `high_risk` + `NDF4W`, and fails safe (no task opened) rather
  than assuming eligibility, on both the survey side (`TestUnderwritingRequiresHighRiskPath`,
  `TestUnderwritingRequiresNdf4wProduct`, `TestShouldCreateTaskDefendsAgainstIneligibleUnderwriting` in
  the `survey` package) and the verdict-interpretation side (`TestApplyVerdictRequiresHighRiskPath`,
  `TestApplyVerdictRequiresNdf4wProduct` in `applyrisksystemverdict`).

The schema source is `lpw-playground-v0_1_1.schema.json` in `lora-schema-service`; the worker
constants are generated from that bundled schema.

The tests cover the important planner mechanics directly: required cursor paths appear in rollback
triggers, optional payload paths do not, the checkpoint-before-survey seed fires exactly once and
never re-seeds after progress is made, `required_data_set` maps onto a survey type (or fails safe for
an unknown value, including the now-retired granular ones and the retired `FINAL_REVIEW`), each
multi-page survey's pages advance the cursor to the right checkpoint one page at a time, `normal` and
`high_risk` share their first three pages' identity so a document can escalate between them without
re-collecting data, underwriting is gated to the `high_risk` path and the `NDF4W` product specifically
(enforced in `apply_risk_system_verdict_pg`, since `check_risk_system_pg`'s async handler has no
document read access to do it itself), and max LTV caps the submitted LTV. The calculation's
max-funding output is marked re-execution-neutral, following LPW's post-scoring calculator pattern for
derived outputs.
