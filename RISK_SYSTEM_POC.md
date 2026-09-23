# Risk System scoring POC

This playground implements the preferred Option B shape:

- `check_risk_system_pg` is one non-deterministic trigger activity, reused for every checkpoint.
- `trigger_seq` is a required read path and monotonic re-trigger.
- `stage_token` selects a deterministic data-availability gate table, one entry per checkpoint.
- Payload fields are optional and use `OptionalIgnoreIfLocked`.
- `apply_risk_system_verdict_pg` translates the asynchronous verdict into playground status fields
  **and** maps the verdict's `required_data_set` onto the survey type LTW must run next.
- `data-forward-risk-system-scored` writes only the RS-owned namespace.
- `calculate_risk_funding_pg` consumes the verdict's max LTV and caps the submitted LTV once financing data exists.
- `seed_scoring_checkpoint_pg` pre-seeds the cursor (M3) at `post_submission` so the first Risk System
  call happens **before any survey runs**.
- `run_survey_pg` is now survey-type aware: it dispatches on `survey_type` to decide which findings
  to collect, then advances the cursor to the matching checkpoint.

The upstream RS call is intentionally a local stub. The trigger records a request id (`playground-rs-<trigger_seq>`) so the planner and lock behavior can be exercised without an LGS proxy contract.

## Required data set selects the survey type

Risk System does not just gate progress — its `required_data_set` verdict field tells LORA **which
survey the user must complete next**. Internally RS derives this from its own risk-level calculation
(a blackbox to LORA, per the design tenet); LORA only consumes the resulting identifier. `apply_risk_system_verdict_pg`
maps it onto a survey type via a small declarative table:

| `required_data_set` | `survey_type` | checkpoint after the survey runs |
|---|---|---|
| `CUSTOMER_VERIFICATION` | `identity` | `customer_verification` |
| `ASSET_REVIEW` | `asset` | `asset_review` |
| `FINANCING` | `financing` | `financing` |
| `INCOME_REVIEW` | `income` | `income_review` |
| `FINAL_REVIEW` | `final_review` | `final_review` |

An unrecognised `required_data_set` fails the activity rather than guessing a survey type — the same
fail-safe posture as an unrecognised verdict status. This matters because RS evolves on its own release
cadence while an in-flight application can stay on an older worker version. In practice a value outside
the document schema's `required_data_set` enum is rejected even earlier, at the data-forward write itself
(see TC3) — the mapping table's own fail-safe is the second layer, for the narrower gap where the schema
enum has already grown but a given worker's code hasn't caught up yet.

Because the mapping is a pure function of the verdict, any checkpoint (not just the first one) can be
told to redo an earlier survey type if RS decides it needs that data again — the chain does not hardcode
a fixed "stage N always leads to stage N+1" transition; RS drives it every time.

## Local flow

1. Start LSS with the bundled playground schema available:

   ```
   cd lora-schema-service && go run cmd/http/main.go
   ```

2. Start Temporal and ArangoDB using the workspace development stack:

   ```
   cd lora-tools/docker/shared && docker compose up -d
   ```

3. Start this worker with `.env`:

   ```
   cd lora-process-worker-playground
   make env   # first time only, copies .env.example to .env
   go run ./cmd
   ```

4. Submit an initial document containing `customer.name`, `customer.nik`, and `customer.birth_date`
   through the SDK's built-in `data-set` update — the same one LGS calls in production for a fresh
   submission (`application/data_set.go`). `id` needs no submission: the SDK sets it from the workflow ID
   automatically at workflow start.
5. `check_customer_eligibility_pg` / `check_name_denylist_pg` / `set_risk_rating_pg` run as before.
6. `seed_scoring_checkpoint_pg` pre-seeds `trigger_seq=0`, `stage_token=post_submission` once a risk
   rating exists — **before any survey has run**.
7. `check_risk_system_pg` fires immediately for `post_submission` (its gate has no required fields).
8. Send `data-forward-risk-system-scored` with the workflow/document id, `status=pending`, and
   `required_data_set=CUSTOMER_VERIFICATION`.
9. `apply_risk_system_verdict_pg` maps that onto `survey_type=identity`.
10. `run_survey_pg` runs the identity survey (birth date, name), advances the cursor to
    `customer_verification`, and bumps `trigger_seq`.
11. `check_risk_system_pg` fires again for `customer_verification`. Repeat steps 8–11 for each dataset
    in turn — `ASSET_REVIEW` → `asset` survey → `asset_review`, `FINANCING` → `financing` survey →
    `financing`, `INCOME_REVIEW` → `income` survey → `income_review`, `FINAL_REVIEW` → `final_review`
    survey → `final_review`.
12. Once the financing survey has run, `calculate_risk_funding_pg` caps the submitted LTV at the Risk
    System max LTV and calculates max funding.
13. Send a final verdict with `status=approved` (or `rejected`) and `required_data_set=FINAL_REVIEW`;
    `apply_risk_system_verdict_pg` writes the terminal status and the workflow stops.

## Test cases

### Running a test case with `testcli`

`cmd/testcli` automates every scenario below end to end — it starts the workflow, sends each step's
`data-set` / `data-forward-override` / `data-forward-risk-system-scored` update in order, and waits for
the worker's pending activities to settle between steps (so it never races ahead of what the previous
step triggered). It reads the same `.env` as the worker, so run it from a shell where the worker's `.env`
is present (or export the same `TEMPORAL_*` vars):

```
go run ./cmd/testcli tc tc1 -id poc-lead-0001
```

Swap `tc1` for any of `tc2`..`tc7`. Add `-wait 5s` if your Temporal server is slower than the default
2-second settle timeout (a still-pending activity after the timeout isn't necessarily wrong — TC3
expects to see exactly that). Steps printed as `NOTE: ...` are things the tool cannot verify itself (it
registers no query handler), so check them by hand — Temporal UI history or the ArangoDB document.

`testcli` also has lower-level subcommands for building your own sequence by hand, which is what the raw
`temporal` CLI commands below are equivalent to:

```
go run ./cmd/testcli start    -id poc-lead-0001
go run ./cmd/testcli inject   -id poc-lead-0001
go run ./cmd/testcli verdict  -id poc-lead-0001 -dataset FINANCING -max-ltv 0.6
go run ./cmd/testcli override -id poc-lead-0001 -field '$.process.loan_structure.ltv_submission=0.5'
```

`inject` sets previously-unset fields via the SDK's built-in `data-set` update (no worker-side handler —
every worker gets it for free); it sends the default identity fields (name/nik/birth_date) unless
`-bare` is given, plus any `-field path=value` overrides (repeatable) — `value` is parsed as a number,
then a bool, then falls back to a string. `override` uses this worker's own `data-forward-override`
handler to overwrite a field that's *already* set (`data-set` refuses that outright) — that's what
TC6/TC7 need for their out-of-band corrections. `verdict` defaults `-status` to `pending`; pass
`-status approved|rejected` and `-reject-reason` for the terminal call. `describe -id <id>` prints the
workflow's pending activities (and any failure on them) — useful for confirming a step settled, or for
watching TC3's stuck attempt.

Note on "the workflow stops" below and in TC1/TC2: that means the *planner* stops scheduling activities
once `$.status` reaches a terminal value (`doc.SetTermination`) — `describe` will keep reporting
`status=Running` indefinitely, since the underlying Temporal workflow execution is designed to stay open
for the life of the document, not to close itself. `pendingActivities=0` is what confirms nothing more
will run.

### Doing it by hand with the `temporal` CLI

Every command below is a Temporal CLI call against the running worker. Both the task queue it polls and
its workflow type are `lpw-playground-v0_1_1` (`framework.System.LoadDocumentSchema` derives this from
the schema file name and passes it to both `RegisterAsWorker` and `RegisterWorkflow` — see
`system.go:174-178,313-315`). `data-set` is the SDK's built-in update (`runtime.WorkflowUpdateSet`,
registered for every worker); the other two update names and their JSON shapes come straight from
`internal/process/workflow/functions.go`, `override.go`, and `risk_system.go`. Replace `<id>` with
whatever workflow ID you start with; `document_id` is only part of the `override` and `verdict` payloads
(the worker checks it matches the workflow ID) — `data-set` takes the flat field map directly, with no
wrapper.

Note: the system also derives `task_lpw-playground-v0_1_1` (`system.go:178`) as `TemporalTaskQueue`, but
that only backs a second internal client (`NewClientForQueue`) — it is not the queue the worker itself
listens on, so don't target it from `temporal workflow start`.

Start the workflow once per test case:

```
temporal workflow start --task-queue lpw-playground-v0_1_1 --type lpw-playground-v0_1_1 --workflow-id <id>
```

Submit the identity fields every case starts from, via the built-in `data-set` update — a flat
path-to-value map, no wrapper, and note `$.id` is absent: the SDK already set it from the workflow ID
when the workflow started, and `data-set` refuses to touch an already-set field:

```
temporal workflow update execute --workflow-id <id> --name data-set --input '{
  "$.customer.name": "John Placeholder",
  "$.customer.nik": "3201010101010001",
  "$.customer.birth_date": "1990-05-20"
}'
```

After that, `check_customer_eligibility_pg` → `check_name_denylist_pg` → `set_risk_rating_pg` →
`seed_scoring_checkpoint_pg` → `check_risk_system_pg` run on their own — no more commands needed until
the first verdict.

Send a verdict with:

```
temporal workflow update execute --workflow-id <id> --name data-forward-risk-system-scored --input '{
  "document_id": "<id>",
  "status": "<approved|rejected|pending>",
  "required_data_set": "<CUSTOMER_VERIFICATION|ASSET_REVIEW|FINANCING|INCOME_REVIEW|FINAL_REVIEW>",
  "max_ltv": <number>,
  "risk_type": "",
  "reject_reason": "<only for status=rejected>",
  "scored_at": "2026-09-23T00:00:00Z"
}'
```

To overwrite a field that's already set (TC6, TC7), use this worker's own `data-forward-override` update
instead — `data-set` above only accepts fields that are still unset:

```
temporal workflow update execute --workflow-id <id> --name data-forward-override --input '{
  "document_id": "<id>",
  "fields": {
    "$.customer.birth_date": "1991-01-01"
  }
}'
```

This worker registers no query handler (confirmed: no `workflow.SetUpdateHandler`/`SetQueryHandler` call
exists for reading the document back), so inspect the resulting state with `testcli describe -id <id>`
(pending activities and failures only) or by reading the ArangoDB document directly — either way, the
fields named after each step below are what to check.

### TC1 — happy path through all five checkpoints to approval

Proves the full chain end to end, including the LTV cap and the terminal transition.

1. Start the workflow and inject, as above.
2. Verdict for `post_submission`: `status=pending`, `required_data_set=CUSTOMER_VERIFICATION`, `max_ltv=0`.
   → `survey_type=identity` → identity survey overwrites `birth_date` to `1985-03-15` and `name` to
   `Jane Smith`, `trigger_seq=1`, `stage_token=customer_verification`.
3. Verdict for `customer_verification`: `status=pending`, `required_data_set=ASSET_REVIEW`, `max_ltv=0`.
   → `survey_type=asset` → `asset.condition=fair`, `trigger_seq=2`, `stage_token=asset_review`.
4. Verdict for `asset_review`: `status=pending`, `required_data_set=FINANCING`, `max_ltv=0`.
   → `survey_type=financing` → `loan_structure.provisional_amount=100000`,
   `loan_structure.ltv_submission=0.8`, `trigger_seq=3`, `stage_token=financing`.
5. Verdict for `financing`: `status=pending`, `required_data_set=INCOME_REVIEW`, `max_ltv=0.6` — this is
   the verdict that actually caps the loan. `calculate_risk_funding_pg` fires and computes
   `effective_ltv=0.6`, `max_funding=60000` (same numbers as
   `TestCalculateFundingCapsSubmissionLTVAtRiskSystemMax`). Then `survey_type=income` →
   `income.verified_amount=15000000`, `trigger_seq=4`, `stage_token=income_review`.
6. Verdict for `income_review`: `status=pending`, `required_data_set=FINAL_REVIEW`, `max_ltv=0.6`.
   → `survey_type=final_review` → `final_review.confirmed=true`, `trigger_seq=5`,
   `stage_token=final_review`.
7. Verdict for `final_review`: `status=approved`, `required_data_set=FINAL_REVIEW`, `max_ltv=0.6`.
   → `status=approved`, `status_timestamps.approved` and `status_timestamps.terminal` set,
   `loan_structure.max_funding=60000`. The workflow stops; no further activity runs.

### TC2 — rejected at final review

Same as TC1 through step 6, then:

7. Verdict for `final_review`: `status=rejected`, `required_data_set=FINAL_REVIEW`, `max_ltv=0.6`,
   `reject_reason=INCOME_INSUFFICIENT`.
   → `status=rejected`, `status_reason=INCOME_INSUFFICIENT`, `status_timestamps.rejected` and
   `status_timestamps.terminal` set. The workflow stops the same way approval does — rejection is not a
   dead end that needs special-casing.

### TC3 — a data set outside the schema enum fails safe

Proves the fail-safe posture from §"Required data set selects the survey type" — but the enforcement
point is earlier than the mapping table itself, which is worth being precise about since it changes what
this test case actually demonstrates.

1. Start, inject, and send the verdict for `post_submission` with `required_data_set=COLLATERAL_REVIEW`
   (not in the schema's `required_data_set` enum).
2. The update call itself is rejected synchronously with `data forward validator fail` — every
   structured data-forward handler validates each field against the document schema before writing
   anything (`runtime/workflow.go` — `f.Validator(v)`), so this never reaches
   `apply_risk_system_verdict_pg`'s own `surveyTypeForDataSet` fail-safe at all. Confirmed against the
   live worker: `testcli tc tc3` reports the update as rejected, not as a stuck/retrying activity.
3. `apply_risk_system_verdict_pg`'s own fail-safe (`surveyTypeForDataSet`, unit-tested directly by
   `TestSurveyTypeForDataSet`) is a second, deeper layer that only matters in a narrower window: LSS
   ships schema-first (CLAUDE.md's Schema-First order), so the document schema's enum can be widened with
   a new `required_data_set` value *before* every worker instance has redeployed with a matching
   `surveyTypeByDataSet` entry. That gap isn't reproducible through this CLI without actually editing the
   schema (which would need a live LSS + a stale worker binary), so the unit test is the right place to
   exercise it, not this scenario.
4. Send a corrected verdict with `required_data_set=CUSTOMER_VERIFICATION`; the chain proceeds as in TC1.

### TC4 — Risk System re-asks for an earlier data set

Proves the chain has no hardcoded "stage N leads to stage N+1" transition — RS's answer alone decides
what runs next, every time (§"Required data set selects the survey type").

1. Run TC1 through step 3 (`stage_token=asset_review`).
2. Instead of asking for `FINANCING`, send a verdict for `asset_review` with
   `required_data_set=CUSTOMER_VERIFICATION` (simulating RS finding a discrepancy and wanting identity
   data re-confirmed).
3. `apply_risk_system_verdict_pg` maps this to `survey_type=identity` again; `run_survey_pg` re-runs the
   identity survey and moves `stage_token` back to `customer_verification`, even though the cursor was
   already past it. `check_risk_system_pg` fires again for `customer_verification` — its gate
   (`birth_date` present) is already satisfied, so it runs immediately.

### TC5 — the pre-survey checkpoint truly runs before any survey

Proves the "checkpoint before survey" ordering claim, not just the gate table in isolation (that part is
covered by `TestStageGateFiresImmediatelyForPostSubmission`).

1. Start and inject only — do not send a verdict yet.
2. Once `seed_scoring_checkpoint_pg` and `check_risk_system_pg` have run (`stage_token=post_submission`,
   a `risk_system.request_id` is set), read the document back. `customer.birth_date` must still be the
   injected placeholder (`1990-05-20`), `customer.name` must still be `John Placeholder`, and no
   `asset`, `income`, or `final_review` fields exist yet — `run_survey_pg` has not executed, because it
   requires `survey_type`, which only `apply_risk_system_verdict_pg` can write.
3. Only after sending the `post_submission` verdict does `birth_date` change to `1985-03-15`.

### TC6 — the seed never re-fires once progress has been made

Proves the guard tested in isolation by `TestNotYetSeededNeverReseeds` also holds end to end.

1. Run TC1 through step 3 (`trigger_seq=2`, `stage_token=asset_review`).
2. Send a `data-forward-override` that changes `customer.birth_date` directly (e.g. to `1991-01-01`),
   simulating an out-of-band correction — `data-set` would refuse this since the field is already set.
   `set_risk_rating_pg` recomputes `risk_rating` off the new birth date.
3. Confirm `trigger_seq` and `stage_token` are unchanged (still `2` / `asset_review`) —
   `seed_scoring_checkpoint_pg`'s precondition (`notYetSeeded`) keeps it from ever running again once
   `trigger_seq` exists, so a later risk-rating recompute cannot reset scoring progress back to
   `post_submission`.

### TC7 — the cap never inflates the customer's own request

Proves `calculate_risk_funding_pg` takes the *lower* of the two LTVs, not RS's max outright.

1. Run TC1 through step 4 (`stage_token=financing`), but at the financing survey step imagine
   `loan_structure.ltv_submission=0.5` instead of `0.8` (adjust the hardcoded survey constant, or send a
   `data-forward-override` on top of it afterward for the test).
2. Send the verdict for `financing` with `max_ltv=0.6` (higher than what the customer submitted).
3. `effective_ltv` must be `0.5`, not `0.6`, and `max_funding=50000` — matches
   `TestCalculateFundingKeepsLowerSubmissionLTV`.

The schema source is `lpw-playground-v0_1_1.schema.json` in `lora-schema-service`; the worker constants
are generated from that bundled schema.

The tests cover the important planner mechanics directly: required cursor paths appear in rollback
triggers, optional payload paths do not, the checkpoint-before-survey seed fires exactly once and never
re-seeds after progress is made, `required_data_set` maps onto a survey type (or fails safe for an
unknown value), each survey type's outcome advances the cursor to the right checkpoint, and max LTV
caps the submitted LTV. The calculation's max-funding output is marked re-execution-neutral, following
LPW's post-scoring calculator pattern for derived outputs.
