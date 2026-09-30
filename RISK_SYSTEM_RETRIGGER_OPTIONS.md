# Re-triggering Risk System after survey submissions — options compared

Status: **comparison, no decision.** Each option that was worth building is implemented on its own
branch of this playground, so its pros and cons are what the code and the end-to-end runs showed,
not estimates. This note records how each branch works and how they compare, so a production
design can pick one on evidence.

## Branches

| # | Option | Branch | Commit | State |
|---|---|---|---|---|
| 0 | `stage_token` + `trigger_seq` cursor (original) | `with-stage-token-and-seq` (= `main`) | `7c52b8b` | implemented |
| 1 | `trigger_seq` counter only | `without-stage-token` | `eb55ba6` | implemented |
| 2 | A meaningful per-submission field | — | — | not implemented: same mechanism as option 1 |
| 3 | One RS step per stage; the most advanced one is re-asked | `split-per-stage` | uncommitted, on `c3825cc` | implemented |
| 4 | SDK hook: task completion re-queues named steps | — | — | not implemented |
| 5 | Per-stage RS results plus an aggregator | `with-stage-specific-write` | `c1cc2cb` | implemented |

The branches stack: `7c52b8b` → `eb55ba6` → `4702f0c` → `c1cc2cb`, each one commit on top of the
previous. `git diff <previous> <branch>` shows exactly what an option changes. Each branch carries
its own `RISK_SYSTEM_POC.md` and testcli scenarios for that design, and all four pass `go test ./...`
(checked 2026-09-27). Option 3 has since been reworked on `split-per-stage` to re-ask on changes
([history](#earlier-version-of-option-3)); `4702f0c` is the earlier version, which only triggered on
first appearance.

## Comparison

"Revise-only page" means a page that only changes fields the initial submission already set: identity
(`customer.name` / `birth_date`) and financing (`provisional_amount` / `ltv_submission`).

| | 0 · original | 1 · `trigger_seq` | 3 · per stage | 5 · per stage + aggregator |
|---|---|---|---|---|
| What asks RS again | a cursor bump on every page | a counter bump on every page, or a change to any field RS was sent | a stage's field appearing, or a change to anything already collected (asked through the most advanced stage) | a stage's field appearing, or a change to any field of an earlier stage |
| Cursor fields | 2 (`trigger_seq`, `stage_token`) | 1 (`trigger_seq`) | none | none |
| After a first-time page (asset, income, environment_check, underwriting) | asks | asks | asks | asks |
| After a revise-only page | asks | asks | asks | asks |
| Page re-submitted with a changed value | asks if the cursor is bumped | asks | asks | asks |
| Final decision fresh? | yes, if every submission bumps the cursor | yes, if every submission that adds data bumps `trigger_seq` (a changed value asks by itself) | yes; guaranteed by the read sets, as long as every survey path collects the stages in `All`'s order ([issues](#known-issues-with-option-3)) | yes; guaranteed by the read sets, whatever the page layout |
| Task side (LTW / testcli) must supply | the next `trigger_seq` and the page's checkpoint name | the next `trigger_seq` | nothing extra | nothing extra |
| Schema fields added | 2 (`trigger_seq`, `stage_token`) | 1 (`trigger_seq`) | 1 (`risk_system.most_advanced_stage`) | 25 (`risk_system_stage.<stage>.*`) |
| RS-related steps | 3 (seed, RS, verdict) | 3 (seed, RS, verdict) | 6 (5 RS, 1 verdict), plus a second funding step | 10 (5 stages, 5 aggregators), plus a second funding step |
| Checked before RS is asked | initial checks, plus the current checkpoint's field present (`stageGates`) | initial checks only | initial checks, every field collected up to the stage present, and no later stage answered yet | initial checks, the stage's own fields present, and no later stage answered yet |
| How survey progress is tracked | RS's `required_data_set` picks the survey type; it is done when `stage_token` equals that type's `nextStage` | RS's `required_data_set` picks the survey type; it is done when its final page's field is present | as option 1 | as option 1 |
| How underwriting is reached | RS asks for it (`required_data_set=underwriting-v1`); the verdict step refuses unless `stage_token == complete_high_risk_survey` and `NDF4W` | RS asks for it (`required_data_set=underwriting-v1`); the verdict step refuses unless `environment_check.result` is present and `NDF4W` | as option 1 | as option 1 |
| RS calls in TC1 / TC2 | 5 / 7 | 5 / 7 | 5 / 7 | 5 / 7 |
| End-to-end evidence | none recorded in this note | TC1–TC6 passed (2026-09-26) | TC1–TC6 passed (2026-09-30) | TC1–TC8 passed (2026-09-27) |

Notes on the table:

- **Bumps and changes count only through a completion.** A bump, or a changed value, re-arms RS
  only when it is written by a task or activity completion. The same write through the `data-set`,
  `data-forward` or `data-forward-override` workflow updates re-arms nothing (see the SDK
  constraint below).
- **Schema fields are counted against a design with no re-trigger mechanism.** The page fields and the
  `risk_system.*` verdict fields are in every option, so they aren't counted. The playground schema
  still has `stage_token` and `trigger_seq` on every branch, but options 3 and 5 don't use them.
- **Underwriting is RS's call.** In every option the underwriting survey starts only because RS
  answers with `required_data_set=underwriting-v1`. LORA's eligibility check is a backstop: when it
  refuses, the verdict step fails and retries visibly (TC4). It doesn't reject the loan, and it
  never starts underwriting on its own.

What every branch shares:

- **A cap RS sends only on the terminal verdict is never applied.** The SDK skips rollback once the
  planner is terminating, so `calculate_risk_funding_pg` doesn't re-run. Options 1, 3 and 5 send the
  cap on an earlier, non-terminal verdict. The original TC1 sends `0.65` after page 3 and `0.6` on
  the terminal verdict, and expects `max_funding=60000`. That expectation assumes the terminal cap
  is applied, which the option 1 run showed it isn't.
- **Out-of-band edits never ask RS.** `data-set` and `data-forward` / `data-forward-override`
  updates never call `Rollback`, so whoever makes such an edit must also ask RS.
- **One RS call pending at a time.** A pending RS step read-locks fields that are in the survey's
  write set, so the next page waits for the verdict (`FieldLock.CanSetWrite`).

### Cost of adding a checkpoint

Say a new survey page is added whose data RS must see. Every option needs the same base work:

- the page's own schema field(s);
- the page's form in LTW;
- the fields in the survey task step's write set, so the task completion can write them;
- the fields added to what the RS step(s) read, so RS is sent them;
- RS itself handling the new data.

The table lists only what each option needs on top of that.

| | 0 · original | 1 · `trigger_seq` | 3 · per stage | 5 · per stage + aggregator |
|---|---|---|---|---|
| New page that sets a field for the first time | a checkpoint name on the page, and a `stageGates` entry for its gate field | nothing more | a new `DataSet` in `checkrisksystem.All` **at its position in the flow**, one more RS step registered, and its name added to `most_advanced_stage`'s enum | a new `DataSet` in `checkrisksystem.All` **at its position in the flow**, plus a stage step and an aggregator step registered |
| New page that only revises existing fields | the same as above (the gate field is already present) | nothing more | nothing more: the fields already belong to a stage or the submission, so a change re-asks the most advanced stage | nothing more: the fields already belong to a stage, so a change re-triggers the most advanced stage |
| If it becomes a survey's final page | update that survey type's `nextStage`; for `high_risk`, also `underwritingHighRiskStageToken` | update that survey type's `completionField`; for `high_risk`, also the field `IsUnderwritingEligible` checks | as option 1 | as option 1 |
| Extra schema fields | none | none | none (one enum value) | 5 (`risk_system_stage.<new>.*`) |
| Task side (LTW) | must send the new checkpoint name, so LTW and the worker release together | nothing: it keeps bumping `trigger_seq` | nothing | nothing |
| Effect on existing steps | none | none | every later stage now requires the new stage's field, so none of them runs on a path that skips the new page | every later stage now reads the new stage's fields, and every aggregator reads its 5 answer fields, so aggregators re-run more often |
| What a missed step does | a missing `stageGates` entry, or an LTW that ships before the worker, gives an unknown `stage_token`: RS is silently never asked again | — | a missing `DataSet` means RS is never asked after that page's first submission; a `DataSet` in the wrong position leaves later stages unrunnable (a silent stall) or lets the wrong stage step aside | a `DataSet` in the wrong position lets the wrong stage decide, and later calls miss the new data. `TestEveryRiskSystemFieldIsOwnedByOneStage` catches a field no stage owns, but only once the field is added to the test's list |

In short:
- **Option 1** is the cheapest: nothing beyond the base work, and nothing on the task side.
- **Option 0** costs a checkpoint name and a gate entry per page, and the new checkpoint name ties
  the LTW and worker releases together.
- **Option 3** covers both kinds of page with no task-side change and one bookkeeping field, but
  the new stage has to go in the right place in the flow, and every later stage then depends on
  it.
- **Option 5** covers both kinds of page with no task-side change, at the price of 5 schema fields
  and 2 steps per stage. The new stage also has to go in the right place in the flow.

## Why this is hard: the SDK constraint

(All paths are in `lora-process-sdk/framework/runtime`.)

- A step that has already run returns to the planner's run list in only two ways:
  - `planner.Rollback`, called from `workflow.go` activity-completion handling;
  - its own partial completion (`AddToRunList`), which only a task step gets.
- `Rollback` is called only for `DocSetResult.Updated`: a field that already had a value and got a
  different one (`versioned_value.go` `Set`). A first-time write lands in `NewlySet` and never
  triggers it.
  - `Rollback` also needs the field's *prior* writer to start its impact walk
    (`MustGetIdWhoSetPriorValue`).
- `data-set` and `data-forward` / `data-forward-override` workflow updates never call `Rollback`
  (`workflow.go` update handling).
- `TriggerRollback: true` on an optional read only adds the path to `RollbackTriggerPaths()`, so it is
  subject to the same Updated-only rule.

Most survey pages only set data for the first time, so a design with no cursor and a single RS step
never asks RS after them. An experiment with that design (not kept on any branch, verified
2026-09-26) asked RS only at the initial call and after the identity and financing pages. The terminal
verdicts after each survey's final page were unreachable, TC1–TC3 ended in `status=processing`, and
a cap RS sent for the first time took effect only when the loan structure next changed. Each option
below is a different way around this rule.

**The Updated-only rule is load-bearing, not an oversight.** An experiment with an opt-in "re-arm on
first set" SDK flag deadlocked tc1. The planner kept rolling back between `max_funding` and the survey
and never converged, and Temporal reported `TMPRL1101` (non-yielding goroutine). The likely
mechanism:

1. There is a read/write cycle. `calculate_risk_funding_pg` writes `max_funding`, which the survey
   reads with `TriggerRollback`. The survey writes `ltv_submission` / `provisional_amount`, which the
   calculator requires. `impactedSteps` pulls both into each rollback, because it works by
   reachability, not by whether values changed.
2. The calculator does not retain its data on rollback and is the first writer of `max_funding`.
   `RevertToValuePriorTo` therefore resets `max_funding` to unset, so every re-run writes it as a
   fresh first-time set.
3. The flag turns each such first-time set into a new re-arm, and the cycle never ends. `max_funding`
   is `SetReExecutionNeutral` precisely so that updates to it don't re-arm anything. A trigger on
   newly-set fields bypasses that filter.

Under Updated-only, a write after a revert-to-unset is not an update, and replaying a step with the
same input writes the same values. So a replayed step cannot restart the chain.

## Options

### 0. `stage_token` + `trigger_seq` cursor — branch `with-stage-token-and-seq`

- **How it works:**
  - `seed_scoring_checkpoint_pg` seeds `trigger_seq=0` and `stage_token=post_submission` once both
    initial checks pass, so RS is asked before any survey runs.
  - Every page writes its findings plus `trigger_seq = previous + 1` and `stage_token = <that page's
    checkpoint>` (`customer_verification`, `asset_review`, `financing_confirmation`,
    `complete_normal_survey`, `income_confirmation`, `complete_high_risk_survey`, `underwriting`).
  - Both are required reads of the one `check_risk_system_pg` step, so every page re-arms it.
    They are only triggers and gates, not data for RS. In production neither is part of the payload
    LORA sends RS, and RS works out the stage from the data it receives. (In the playground they
    show up in the activity input only because every required read does.)
  - **Per-page field gate.** The step's precondition (`stageGate`) requires both initial checks to
    be `true`, then looks up `stage_token` in `checkrisksystem.stageGates` and waits until that
    checkpoint's field is present: `customer.birth_date` for `customer_verification`,
    `asset.condition` for `asset_review`, `ltv_submission` for `financing_confirmation`,
    `income.verified_amount` for `complete_normal_survey` and `income_confirmation`,
    `environment_check.result` for `complete_high_risk_survey`, `underwriting.confirmed` for
    `underwriting`. `post_submission` has no gate field.
  - The other fields RS is sent are non-triggering optional reads.
  - `stage_token` also carries survey progress. A survey type is done when `stage_token` equals its
    `nextStage`, and underwriting requires `stage_token == complete_high_risk_survey`.
- **Pros:**
  - RS is asked after every page, including revise-only and final pages.
  - Only one RS step and one verdict step, writing one set of `risk_system.*` fields.
  - The gate stops RS being asked about a checkpoint whose data isn't in the document, for example
    if the task side sends the cursor without the page's findings.
  - Progress is recorded explicitly, not inferred from the data. A survey counts as done, and
    underwriting as eligible, only once the page that finishes it has been submitted. A field set
    some other way (for example in the initial submission through `data-set`) doesn't count.
  - The document names the checkpoint RS was last asked about, which is easy to read in a debugger.
- **Cons:**
  - Two fields with no business meaning. The task side must supply both, including the worker's
    checkpoint names, which couples LTW to the worker's stage table.
  - `stageGates` repeats the survey's page layout and has already drifted: its `financing` entry
    can't be reached by any survey.
  - An unknown `stage_token` fails silently. `stageGate` returns false, so RS is never asked and
    nothing errors. That happens with a mistyped name, or with an LTW that is newer than the worker
    and sends a checkpoint the worker doesn't know.
  - Underwriting eligibility depends on a string, and the verdict step has to read back its own
    `survey_type` so it doesn't reject the verdict that follows the underwriting page (the
    underwriting submission moves `stage_token` off `complete_high_risk_survey`).
  - The page fields don't re-trigger RS themselves. A changed value re-triggers RS only if the
    cursor is bumped too.
  - A single RS step in the `risk_system.*` → `survey_type` → `stage_token` loop is re-impacted
    after every verdict. It relies on history fast-forward (unchanged input, same verdict) not to
    open a second call.

### 1. `trigger_seq` counter only — branch `without-stage-token`

- **Change from option 0:** `stage_token` is dropped everywhere. What it did is now read from
  collected data:
  - the initial gate is `checkrisksystem.initialGate`;
  - a survey type is done once its final page's field is present (`income.verified_amount`,
    `environment_check.result`, `underwriting.confirmed`);
  - underwriting requires `environment_check.result`.
  Every field RS is sent is also a rollback-triggering optional read (`rearmOnChange`).
- **How it works:**
  - The seed step writes only `trigger_seq=0`. Without that prior value, page 1's write would be a
    first-time set.
  - Each page writes `trigger_seq = previous + 1`, which is a real update, so RS is asked again after
    every page.
- **Why it converges:** only the seed (once; its read set is empty, so it is never impacted) and the
  survey (which retains its data) write `trigger_seq`. It changes only on a real human submission,
  and a replayed survey writes the same number.
- **Pros:**
  - RS is asked after every submission, including a re-submitted page and a final page that only
    revises fields. A changed field asks RS again even without a bump.
  - It is the smallest change from option 0: one RS step, one verdict step, and one schema field
    instead of two.
  - The task side supplies only a number; it doesn't need to know the worker's checkpoints.
  - Underwriting eligibility and survey completion follow the data, so the stage table and its
    drift are gone.
- **Cons:**
  - A counter with no business meaning. The task side has to supply the next value, because a task
    completion can't read the document.
  - Freshness depends on every submission that adds data bumping `trigger_seq`. The bump has to be
    written by a task or activity completion. The same bump through `data-set`, `data-forward` or
    `data-forward-override` re-arms nothing.
  - Losing `stage_token` also loses what it gave option 0:
    - There is no per-page field gate, so RS is asked on any bump, whatever data has arrived.
    - Progress is inferred from the data. A final-page field that is present counts as done, however
      it got there. For example, an `environment_check.result` set in the initial submission would mark `high_risk`
      done and make the applicant eligible for underwriting.
- **Evidence (2026-09-26):** TC1–TC6 passed end to end with their full endings (approval,
  underwriting, rejection after the survey). The same run found that a cap sent on the terminal
  verdict is never applied, so TC1 sends its cap on the page-3 verdict.

### 2. A meaningful per-submission field — not implemented

- **How it works:** the same mechanism as option 1, but the value means something on its own, for
  example `survey.last_submitted_at` or `survey.last_page`.
- **Why it isn't a branch:** the planner behaviour would be identical to option 1's; only the
  field's name and meaning change. Option 1's branch covers the mechanism.
- **Cost:** a new optional LSS schema field (backward compatible) plus a seed or first value.
- **Caveat:** a timestamp must come from the submission payload, not the worker clock, or it breaks
  workflow determinism.
- **Checked:** the SDK maintains no per-submission document field today. The only internal field it
  writes is `$.internal.taskmaster.id`, which is set once.

### 3. One RS step per stage; the most advanced one is re-asked — branch `split-per-stage`

- **How it works:** one RS step per stage, each first run when its page's field appears:
  `check_risk_system_pg` (initial), `_asset` (`asset.condition`), `_income`
  (`income.verified_amount`), `_environment_check` (`environment_check.result`) and `_underwriting`
  (`underwriting.confirmed`). The seed step is removed.
  - **Read sets carry the trigger rule** (`checkrisksystem.readSet`). Everything already collected
    when a stage runs is a **required** read: the submission's fields plus the data-set fields of
    that stage and every stage before it in `checkrisksystem.All`. A required read gates the stage
    and is always a rollback trigger, so the stage first runs when its own field appears and is
    re-queued when anything it already sent changes. Later stages' fields are sent as
    non-triggering optional reads. A field is never both.
  - **Only the most advanced stage re-asks.** `impactedSteps` re-queues every stage that reads a
    changed field, not only the most advanced one. So each stage writes its own name to
    `risk_system.most_advanced_stage` with its answer, and its precondition (`notSuperseded`) makes it
    step aside once a later stage in `All` has answered. The most advanced stage already sends every
    earlier stage's data, so each change asks RS exactly once.
  - `most_advanced_stage` only moves forward (a stage runs only when it is at least as advanced as
    the recorded one). It is a precondition path, so recording a new stage re-queues nothing, and it
    is marked re-execution-neutral as a guard. A name `All` doesn't know supersedes nothing, so an
    unknown value costs an extra call rather than leaving nobody to ask.
- **Why it converges:** the steps all write the same `risk_system.*` fields, so their write locks
  serialize them: at most one is pending, and the latest write is the latest call.
  - The only stage that can re-run is the one that recorded the current verdict. If its input is
    unchanged, it fast-forwards through history onto its own answer for that input, so a replay
    can't restore an older stage's verdict.
  - The stages retain their data on rollback, so no rollback resets `risk_system.*` to unset (the
    tc1-deadlock mechanism).
- **Changes it forced elsewhere:**
  - A cap RS sends for the first time is newly set, so it re-runs nothing in
    `calculate_risk_funding_pg`. A second step, `calculate_risk_funding_pg_capped`, requires
    `ltv_max` and runs when the cap first appears. Both calculation steps are pure and both read the
    cap, so whichever runs last is correct and a replay is harmless.
  - The verdict step reads `request_id` as a required read. Every call mints a new one, so every
    call re-runs the verdict step against the latest values. Without it, a call that repeats the
    previous `status` and `required_data_set` but sends a cap for the first time left
    `max_funding=80000` in the first tc1 run.
  - testcli's `verdict` matches the RS steps by name (`checkrisksystem.ActivityNames()`), never
    `check_risk_system_pg_verdict`.
  - One `_income` step serves both surveys' income page, because `income.verified_amount` first
    appears once whichever survey collects it.
- **Pros:**
  - RS is asked after every submission that changes data, including revise-only pages and
    re-submitted pages, with no cursor for the task side to supply.
  - One schema field, and only one RS step per stage plus one verdict step (6, against option 5's
    10).
  - Freshness of the final decision is enforced by the read sets, not by the task side's behaviour.
  - A later answer can't be overwritten by the replay of an earlier one.
- **Cons:** `All`'s order is load-bearing, and RS is assumed to answer the same input the same way.
  See [Known issues with option 3](#known-issues-with-option-3).
- **Evidence (2026-09-30):** TC1–TC6 passed end to end with their full endings, checked against
  each workflow's Temporal history and the stored document:
  - TC1–TC3 ended approved with `max_funding=60000` and `status=Completed`;
  - TC4 left `check_risk_system_pg_verdict` failing and retrying, and the underwriting submission
    was rejected;
  - TC5 and TC6 ended rejected with `status=Completed`.

  Every run made exactly one RS call per verdict (TC1: initial ×2, asset ×2, income ×1; TC2 and TC3:
  7 calls), never had more than one RS call pending, and produced no `TMPRL1101` or workflow-task
  failure. After each financing page, `check_risk_system_pg` was re-queued but stepped aside and only
  `_asset` asked. `most_advanced_stage` ended on the last stage that answered.

#### Known issues with option 3

1. **`All`'s order must match every survey path.** A stage requires every earlier stage's field and
   is ranked by its position. If a path differs from that order — say a repeat-order customer's
   survey has no asset page, or collects income before asset — then:
   - a path that skips an earlier stage's page never runs any later stage: RS is not asked again,
     and the workflow stalls in `status=processing` with no error;
   - a path that collects pages out of order gets no RS call after the early page, then two calls in
     a row once the missing field arrives;
   - a field collected earlier than `All` says may not be read by the stage that is most advanced on
     that path, so changing it later asks nobody and the final decision can be stale.

   Every survey path in this playground collects the stages in `All`'s order. Genuinely different
   flows would need one ordered stage list per path, with the steps gated on a discriminator known
   before the survey starts.
2. **RS answers are reused for unchanged input.** A re-queued stage whose input hasn't changed
   reuses RS's earlier answer instead of calling RS. That's correct only if RS answers the same input
   the same way; otherwise the stages would need `SetNonDeterministic()`, at the price of redundant
   calls. The same assumption as option 5.
3. **A resubmission with identical values asks nothing** (it isn't an update). Options 0 and 1 ask
   RS on every submission regardless.

**Defense in depth for any option (not implemented):** refuse a stale terminal verdict. Record a
fingerprint of the data each RS call sent. The verdict step then refuses to write
`approved`/`rejected` if the document's RS inputs have changed since, and fails visibly the way TC4
does. A stale verdict then becomes a diagnosable stall rather than a wrong decision.

#### Earlier version of option 3

`4702f0c` built option 3 with **no rollback trigger paths at all**: each step's data-set field was an
optional precondition path, everything RS was sent a non-triggering optional read, so each step ran
exactly once, when its field first appeared (TC1: 3 RS calls). That was the strongest convergence
argument, but it triggered only on data *appearing*: revise-only and re-submitted pages never asked
RS, so a flow whose last page only revised fields would approve or reject on data RS never saw.
Its end-to-end run (TC1–TC6, 2026-09-26) passed only because every survey in this playground ends on
a first-time field. The current version replaces that with the read-set rule above.

### 4. SDK hook: task completion re-queues named steps — not implemented

- **How it works:** an explicit edge on the step, "when this task records a partial or final
  completion, re-queue these steps". It fires only on human submissions, not on data propagation,
  so it avoids the calculator ↔ survey loop.
- **Pros:** triggers on submission like options 0 and 1, with no cursor field for the task side to
  supply.
- **Cons:** a shared-SDK change that every worker inherits, and the design becomes event-driven
  rather than data-driven. It was not built because options 1 and 5 reach the same behaviour without
  changing the SDK.

### 5. Per-stage RS results plus an aggregator — branch `with-stage-specific-write`

Option 3's staging and triggers (data appearing or changing), but each stage records its answer
on its **own** fields, and an aggregator combines the stage answers into the one verdict. Option 3
instead keeps one shared set of `risk_system.*` fields and a single `most_advanced_stage` field for
stepping aside.

- **How it works:**
  - **Stages** (`checkrisksystem`): `check_risk_system_pg` (initial), `_asset`, `_income`,
    `_environment_check` and `_underwriting`.
    - A stage's own data-set fields are **required** reads, so it first runs when its data
      appears.
    - Every field of every **earlier** stage is a rollback-triggering optional read. So each
      call sends everything collected up to that stage, and any later change to that data asks
      again.
    - Each stage writes RS's raw answer to `process.scoring.risk_system_stage.<stage>.*`: all five
      fields on every answer (`max_ltv` 0 and `reject_reason` "" when RS sends none), so after the
      first answer each later one is an update, never a newly-set write.
    - A stage **steps aside** once a later stage has answered: its precondition checks the later
      stages' `request_id`s. The aggregator uses the later answer anyway, and that stage already
      sends the earlier data, so each change calls RS exactly once.
  - **Aggregators** (`checkrisksystemverdict`): one per stage, all computing the same pure function.
    - Rule: the **most advanced stage that has answered decides**. Its call sent everything
      collected so far, and it is re-triggered on any change, so its answer is always the freshest.
    - Output: the aggregated verdict record (`process.scoring.risk_system.*`) plus its
      interpretation (`survey_type`, `$.status`, timestamps, `ltv_max`, the underwriting gate).
    - Why one per stage: a stage's first answer is newly set and re-arms nothing, so only an
      aggregator that *requires* it runs. The other stages' answers are rollback-triggering,
      wait-if-locked reads, so later answers re-run every aggregator. They also stop an aggregator
      from deciding while any stage call is pending.
  - Option 3's `calculate_risk_funding_pg_capped` step stays.
- **Why the final decision stays fresh:** freshness is a property of the read sets, not of the page
  layout. Every field RS is sent belongs to one stage, and the most advanced stage reads all of
  them with rollback triggers (`TestEveryRiskSystemFieldIsOwnedByOneStage`,
  `TestEveryReadIsARollbackTrigger`). So any change arriving through an activity or task completion
  asks RS again before a decision can be made: a revised page (TC7), or a submission that only
  updates existing fields (TC8), including one on a last page.
- **Why it converges:** the per-stage outputs remove the replay hazard of shared verdict fields.
  - A stage re-queued by the reachability walk with an unchanged input fast-forwards through
    history, which restores only *its own* answer, for *that* input, into *its own* fields
    (`TestStageOutputsAreDisjoint`). Identical values aren't updates, so nothing cascades.
  - Aggregators are pure over the stage answers, so every re-run and every replay writes the same
    value.
  - Stages and aggregators retain data on rollback, so no rollback resets a field to unset (the
    tc1-deadlock mechanism).
- **Pros:**
  - RS is asked after every submission that changes data, including revise-only pages and
    re-submitted pages, with no cursor for the task side to supply.
  - Freshness of the final decision is enforced by the worker's read sets, not by the page layout
    or by the task side's behaviour.
  - A later answer can't be overwritten by the replay of an earlier one.
  - The only branch whose scenarios re-submit an earlier page (TC7) end to end.
- **Cons:**
  - The most machinery: 25 new optional LSS schema fields (additive and backward compatible) and
    ten RS-related steps instead of two or three.
  - **Aggregators re-run often:** 1–15 aggregator executions per scenario (TC5: 1, TC1: 6,
    TC7/TC8: 15), all writing identical values. This is cheap, synchronous work.
  - **RS answers are reused for unchanged input.** A re-queued stage whose input hasn't changed
    reuses RS's earlier answer instead of calling RS. That's correct only if RS answers the same
    input the same way; if RS depends on changing external data, the stages would need
    `SetNonDeterministic()`, at the price of redundant calls.
  - **The rule is an RS contract assumption.** "Most advanced stage decides" assumes each RS call
    judges everything sent to it, not only its own stage's data. If RS judges each stage's data in
    isolation, the aggregation rule has to change.
  - A resubmission with identical values asks nothing (it isn't an update). Options 0 and 1 ask RS
    on every submission regardless.
- **Evidence (2026-09-27):** TC1–TC8 passed end to end, checked against each workflow's Temporal
  history:
  - TC1–TC3 and TC7 ended approved with `max_funding=60000`;
  - TC4 left `check_risk_system_pg_verdict_environment_check` failing and retrying, and the
    underwriting submission was rejected;
  - TC5 and TC6 ended rejected;
  - TC7's revised asset page asked RS again at once and escalated to `high_risk`;
  - TC8's update-only submission asked RS, and its revised `provisional_amount=120000` reached the
    final decision (`max_funding=72000`, not 60000).

  Every run made exactly one RS call per verdict (e.g. TC1: initial ×2, asset ×2, income ×1), never
  had more than one RS call pending, and produced no `TMPRL1101` or workflow-task failure.

### Ruled out

- **Seeding placeholder values into every page field** so that first writes become updates. Survey
  completion and underwriting eligibility depend on whether fields are present, so both would break
  silently.
- **Re-triggering through `data-forward` from the task side.** It never calls `Rollback`.
- **An opt-in "re-arm on first set" SDK flag.** It breaks convergence (the tc1 deadlock above).

## Trade-offs in short

- **Options 0 and 1 trigger on a counter bump.** RS is asked on every page because each submission
  bumps `trigger_seq`. They are simple (one RS step, one or two schema fields) but depend on every
  submission that adds data bumping the counter through a task or activity completion. Option 1
  re-triggers RS the same way with one field instead of two, and doesn't couple the task side to the
  worker's checkpoint names. Option 0's second field buys:
  - a per-page field gate before RS is asked;
  - survey progress recorded explicitly rather than inferred from the data.
  The price is the stage table, which repeats the page layout and fails silently on an unknown
  checkpoint.
- **Options 3 and 5 trigger on appearance and on change.** Both keep the final decision fresh
  through their read sets with nothing from the task side, and both assume RS gives the same input
  the same answer and judges everything it is sent. Option 3 does it with one schema field and six
  steps, sharing one set of verdict fields and letting earlier stages step aside. Option 5 does it
  with 25 schema fields and ten steps, one answer record per stage plus aggregators. In both,
  `All`'s order must match every survey path.
- **Options 2 and 4 are variants.** Option 2 is option 1 with a field that means something, and
  option 4 is option 1's behaviour implemented in the SDK rather than through a field.

Two gaps remain whichever option is chosen, because both come from the SDK, not from the design:

- **Edits made outside a task or activity completion never ask RS.** A field changed through the
  `data-set`, `data-forward` or `data-forward-override` workflow updates (a back-office correction, say, or an
  LGS push) doesn't re-run any step, because the SDK calls `Rollback` only after an activity or task
  completion. RS keeps its old verdict until some later submission asks it again. Whatever makes
  such an edit must also ask RS, or the edit must go through a task.
- **A cap RS sends only on its final approve/reject verdict never reaches `max_funding`.** Once
  `$.status` is terminal the planner stops scheduling steps. `ltv_max` is recorded, but
  `calculate_risk_funding_pg` doesn't run again, so `max_funding` keeps the value from the previous
  cap. RS has to send the cap on an earlier, non-terminal verdict.
