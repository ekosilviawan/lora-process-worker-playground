# Re-asking Risk System after first-time survey data — design options

Status: **decided — option 5 (per-stage Risk System results plus an aggregator) is implemented**
(2026-09-27). Option 1 (`trigger_seq` counter, `eb55ba6`) and then option 3 (one RS step per data
set) were implemented first and replaced. Option 3 was dropped because it can leave the final
decision stale (see [Known issues with option 3](#known-issues-with-option-3)); option 5 fixes that
without a counter. Neither `trigger_seq` nor `stage_token` is read or written any more. This note
records why an "updates only" design without any counter was not enough, and which alternatives
were weighed, so the choice can be revisited.

## The problem (updates-only design, since replaced)

In the updates-only design, Risk System (RS) is asked at intake, after the identity page and after
the financing page. It is **never** asked after pages that only set data for the first time: asset,
income, environment_check and underwriting. In practice:

- The terminal verdicts after a survey's final page are unreachable: approval after `income`,
  `underwriting-v1` after `environment_check`, and approval after the underwriting page.
- TC1–TC3 end in `status=processing`, and `underwriting` isn't reachable in any scripted scenario.
- A cap RS sends for the first time (`ltv_max`) takes effect only when the loan structure next changes.

Verified end to end on 2026-09-26, before option 1 was implemented.

## The SDK constraint behind it

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

### 1. `trigger_seq` counter — implemented first, since replaced by option 3

- **How it works:** `trigger_seq` goes back into `check_risk_system_pg`'s required reads.
  - The survey writes `trigger_seq = previous + 1` on every page, which is a real update, so RS is
    re-asked after every page.
  - A slim seed step writes `trigger_seq = 0` once both intake checks pass. Without that prior value,
    page 1's write would be a first-time set.
  - `stage_token` stays retired: its jobs are already done from data (`intakeGate`, each survey's
    final-page field, and `environment_check.result` for underwriting eligibility).
- **Why it converges:** only the seed (once; its read set is empty, so it is never impacted) and the
  survey (which retains its data) write `trigger_seq`. It changes only on a real human submission,
  and a replayed survey writes the same number.
- **Mutual exclusion:** while RS is pending it holds a read lock on `trigger_seq`, which is in the
  survey's write set. This blocks the next page until the verdict lands (`FieldLock.CanSetWrite`),
  the same mechanism as production's M2 contract.
- **Cost:** a counter that carries no business meaning. The task side (testcli, or LTW in production)
  supplies the next value, because a task completion can't read the document.
- **Evidence:** the original cursor design used the same mechanism. After implementation, TC1–TC6
  passed end to end with their full endings (approval, underwriting, rejection after the survey).
  The run also showed a cap RS sends on the *terminal* verdict is never applied, because the SDK
  skips rollback once the planner is terminating; TC1 sends its cap on the page-3 verdict for that
  reason.
- **Work:** re-add the read, the seed step and the survey write, restore testcli's `-seq` and full
  scenarios, and update the tests and docs. No SDK or schema change.

### 2. A meaningful per-submission field

- **How it works:** the same mechanism as option 1, but the value means something on its own, for
  example `survey.last_submitted_at` or `survey.last_page`.
- **Cost:** a new optional LSS schema field (backward compatible) plus a seed or first value.
- **Caveat:** a timestamp must come from the submission payload, not the worker clock, or it breaks
  workflow determinism.
- **Checked:** the SDK maintains no per-submission document field today. The only internal field it
  writes is `$.internal.taskmaster.id`, which is set once.

### 3. Split RS into one step per data set — implemented, then replaced by option 5

- **How it works:** one RS step per data set, each gated on its page's field: `check_risk_system_pg`
  (intake), `_asset` (`asset.condition`), `_income` (`income.verified_amount`),
  `_environment_check` (`environment_check.result`) and `_underwriting` (`underwriting.confirmed`).
  A step that has never run becomes runnable as soon as its field first appears, with no counter and
  no `Rollback`. This is the purest form of data triggering.
- **Why it converges — no RS step has a rollback trigger path.** `impactedSteps` pulls in any step
  whose required reads, or `TriggerRollback` optional reads, overlap the fields written along the
  chain, and the survey writes every page field. So each step's data-set field is a *precondition*
  path instead: read-locked (`lock_map.go` `Lock`) but never in `RollbackTriggerPaths()`, and not
  part of the activity input (`workflow.go` `doActivityExec`). Everything RS is sent is a
  non-triggering optional read.
  - Each RS step therefore runs exactly once and is never re-queued, so it never fast-forwards
    through history onto an older verdict. That was this option's stated fragility.
  - The steps all write the same `risk_system.*` fields, so their write locks serialize them. At
    most one is pending, and the latest write is always the latest call; the verdict step just reads
    the current values.
- **Mutual exclusion:** while an RS step is pending it read-locks its page field, which is in the
  survey's write set, so the next page waits for the verdict (`FieldLock.CanSetWrite`). After a
  page, the RS step (`Normal`) wins the scheduling round over the survey (`Lazy`).
- **Costs actually paid:**
  - The identity and financing pages only revise fields the intake data-set already set, so they
    have no first-time field and **don't ask RS**. Giving them one would take two new schema fields
    (option 2's cost).
  - Re-submitting a page with a changed value doesn't ask RS. A trigger on the page field would
    reintroduce the replay risk above. See
    [Known issues with option 3](#known-issues-with-option-3) for why this is more than a cost.
  - A cap RS sends for the first time is newly set, so it re-ran nothing in
    `calculate_risk_funding_pg`; under option 1, `trigger_seq` walks had re-run it. It is split the
    same way: `calculate_risk_funding_pg_capped` requires `ltv_max`, so it runs when the cap first
    appears. Both calculation steps are pure and both read the cap, so whichever runs last is
    correct and a replay is harmless. A cap sent only on the terminal verdict is still never applied.
  - The same rule reached the verdict step. An RS call that repeats the previous `status` and
    `required_data_set` but sends a cap for the first time (`max_ltv` newly set) updated only
    `request_id`, which nothing read, so `check_risk_system_pg_verdict` never re-ran. The first
    end-to-end tc1 run showed this: the asset verdict's cap was applied only on the terminal verdict,
    too late, leaving `max_funding=80000`. The verdict step now reads `request_id` as a required read.
    Every call mints a new one, so every call re-runs the verdict step against the latest values. That
    is how "the verdict step has to tell which call is the latest".
  - testcli's `verdict` matches the RS steps by name (`checkrisksystem.ActivityNames()`), never
    `check_risk_system_pg_verdict`.
  - An SDK constraint shaped the gating. `LockMap.Lock` read-locks required precondition paths and
    optional reads separately, so a field in both panics the workflow task
    (`ErrFieldLockAlreadyHeldById`). The data-set fields are therefore *optional* precondition paths,
    which are de-duplicated against reads, and the precondition function requires them to be present
    (`TestRequiredPreconditionPathsAreNotAlsoReads`).
- **Shared fields:** one `_income` step serves both surveys' income page, because
  `income.verified_amount` first appears once whichever survey collects it.
- **Evidence (2026-09-26):** TC1–TC6 passed end to end with their full endings, checked against
  each workflow's Temporal history:
  - TC1–TC3 ended approved with `max_funding=60000` and `status=Completed`;
  - TC4 left `check_risk_system_pg_verdict` failing and retrying, and the underwriting submission
    was rejected;
  - TC5 and TC6 ended rejected with `status=Completed`.

  In every run, each `check_risk_system_pg*` activity was scheduled exactly once for its data set
  (TC2/TC3: intake, asset, income, environment_check, underwriting), and at most one RS call was
  ever pending. There was no `TMPRL1101`. The two calculation steps re-ran a few times each but
  always converged on the same value (TC1: 67500 at intake → 54000 when the cap appeared → 60000
  after the financing page).

#### Known issues with option 3

The root cause: option 3 triggers RS on **data first appearing**, not on **a submission
happening**. A step that has already run is re-queued only by `planner.Rollback`, and an RS step
with a rollback trigger could replay a stale verdict (see above), so no RS step can ever react to a
change. Every issue below follows from that.

1. **The final decision can be stale — risk of business loss.** The verdict that approves or
   rejects is only as fresh as the last RS call, and a call happens only on a page that sets a field
   for the first time.
   - In this playground every survey ends on such a page: income for `normal`,
     environment_check for `high_risk`, and the single-page `underwriting`. That is why TC1–TC6 are
     correct.
   - That is a property of this playground's page layout, which the worker can't enforce. A
     production flow whose last page only revises existing fields would approve or reject on data
     RS never saw. A final review page, a financing confirmation page, or a later restructuring of
     pages would all do it. Nothing fails and nothing is logged, so the loan is simply decided on
     stale data.
2. **A revised page doesn't call RS.** A surveyor who goes back and re-submits an earlier page
   changes the document without asking RS. Because every RS call sends the whole current
   document, the next page that does call RS carries the revision. If no such page follows, the
   revision is never seen (issue 1).
3. **Mid-flow decisions act on stale data.** Until the next call, the flow keeps following the
   verdict RS gave for the old data:
   - Escalation arrives late. Asset condition revised from `fair` to `poor` after the asset verdict
     is seen only at the income page, which is `normal`'s final page. RS can still escalate to
     `high_risk` there, and the survey reopens for environment_check, but a page later than it would
     under option 1.
   - A cap RS set from the old data keeps driving `max_funding`, which the underwriting verificator
     reads to the customer.
4. **Pages that only revise intake data never call RS.** In this playground those are identity and
   financing. Giving them a first-time field would take new schema fields, and still would not fix
   issue 2.
5. **Out-of-band corrections never call RS, in every option.** `data-set` and
   `data-forward`/`data-forward-override` updates never call `Rollback`. Whoever makes such an edit
   must also ask RS.

**What fixes it:**
- **Trigger on submission, not on first appearance.**
  - Option 1 (`trigger_seq`) calls RS after every page submission, including a re-submitted page
    and a final page that only revises fields. It is already implemented at `eb55ba6`.
  - Option 2 is the same mechanism with a field that has business meaning.
  - Option 4 makes the same trigger an explicit SDK event.
  - All three depend on the task side (LTW, or testcli here) reporting every submission.
- **Defense in depth, for any option: refuse a stale terminal verdict.** Record a fingerprint of the
  data each RS call sent. `check_risk_system_pg_verdict` then refuses to write `approved`/`rejected`
  if the document's RS inputs have changed since, and fails visibly the way TC4 does. A stale verdict
  then becomes a diagnosable stall rather than a wrong decision. Not implemented.

### 4. Narrow SDK hook: task completion re-queues named steps

- **How it works:** an explicit edge on the step, "when this task records a partial or final
  completion, re-queue these steps". It fires only on human submissions, not on data propagation,
  so it avoids the calculator ↔ survey loop.
- **Cost:** a shared-SDK change, and the design becomes event-driven rather than data-driven.

### 5. Per-stage RS results plus an aggregator — chosen, implemented

Option 3's staging, but each stage is triggered by data **changing** as well as appearing, and
records its answer on its **own** fields. An aggregator combines the stage answers into the one
verdict.

- **How it works:**
  - **Stages** (`checkrisksystem`): `check_risk_system_pg` (intake), `_asset`, `_income`,
    `_environment_check` and `_underwriting`.
    - A stage's own data-set fields are **required** reads, so it first runs when its data
      appears.
    - Every field of every **earlier** data set is a rollback-triggering optional read. So each
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
      collected so far, and it is re-asked on any change, so its answer is always the freshest.
    - Output: the aggregated verdict record (`process.scoring.risk_system.*`) plus its
      interpretation (`survey_type`, `$.status`, timestamps, `ltv_max`, the underwriting gate).
    - Why one per stage: a stage's first answer is newly set and re-arms nothing, so only an
      aggregator that *requires* it runs. The other stages' answers are rollback-triggering,
      wait-if-locked reads, so later answers re-run every aggregator. They also stop an aggregator
      from deciding while any stage call is pending.
- **Why the final decision stays fresh:** freshness is a property of the read sets, not of the page
  layout. Every field RS is sent belongs to one data set, and the most advanced stage reads all of
  them with rollback triggers (`TestEveryRiskSystemFieldIsOwnedByOneStage`,
  `TestEveryReadIsARollbackTrigger`). So any change arriving through an activity or task completion
  asks RS again before a decision can be made: a revised page (TC7), or a submission that only
  updates existing fields (TC8), including one on a last page.
- **Why it converges:** the per-stage outputs remove option 3's replay hazard.
  - A stage re-queued by the reachability walk with an unchanged input fast-forwards through
    history, which restores only *its own* answer, for *that* input, into *its own* fields
    (`TestStageOutputsAreDisjoint`). Identical values aren't updates, so nothing cascades.
  - Aggregators are pure over the stage answers, so every re-run and every replay writes the same
    value.
  - Stages and aggregators retain data on rollback, so no rollback resets a field to unset (the
    tc1-deadlock mechanism).
- **Mutual exclusion:** a pending stage read-locks the fields it sends, all in the survey's write
  set, so the next page waits for the answer. With stepping aside, one call is pending at a time.
- **Costs:**
  - New optional LSS schema fields (`risk_system_stage.*`, additive and backward compatible), and
    ten steps instead of two.
  - **Aggregators re-run often:** 1–15 aggregator executions per scenario (TC5: 1, TC1: 6, TC7/TC8: 15), all writing identical
    values. This is cheap, synchronous work.
  - **RS answers are reused for unchanged input.** A re-queued stage whose input hasn't changed
    reuses RS's earlier answer instead of calling RS. That's correct only if RS answers the same
    input the same way; if RS depends on changing external data, the stages would need
    `SetNonDeterministic()`, at the price of redundant calls.
  - **The rule is an RS contract assumption.** "Most advanced stage decides" assumes each RS call
    judges everything sent to it, not only its own data set.
  - A cap sent only on the terminal verdict is still never applied (as before), and out-of-band
    `data-set`/`data-forward` edits still never call RS (every option).
- **Evidence (2026-09-27):** TC1–TC8 passed end to end, checked against each workflow's Temporal
  history:
  - TC1–TC3 and TC7 ended approved with `max_funding=60000`;
  - TC4 left `check_risk_system_pg_verdict_environment_check` failing and retrying, and the
    underwriting submission was rejected;
  - TC5 and TC6 ended rejected;
  - TC7's revised asset page asked RS again at once and escalated to `high_risk`;
  - TC8's update-only submission asked RS, and its revised `provisional_amount=120000` reached the
    final decision (`max_funding=72000`, not 60000).

  Every run made exactly one RS call per verdict (e.g. TC1: intake ×2, asset ×2, income ×1), never
  had more than one RS call pending, and produced no `TMPRL1101` or workflow-task failure.

### Ruled out

- **Seeding placeholder values into every page field** so that first writes become updates. Survey
  completion and underwriting eligibility depend on whether fields are present, so both would break
  silently.
- **Re-triggering through `data-forward` from the task side.** It never calls `Rollback`.
- **An opt-in "re-arm on first set" SDK flag.** It breaks convergence (the tc1 deadlock above).

## Decision

Option 5 is implemented. It keeps option 3's data-driven staging but triggers on changes as well as
first appearances, which option 3's shared verdict fields made unsafe. Revisit it if RS's answers
are not deterministic for identical input (then force real calls), or if RS judges each data set in
isolation (then the aggregation rule must change). Option 1 remains the simpler fallback: one
counter and one verdict, at the cost of depending on the task side to report every submission.
