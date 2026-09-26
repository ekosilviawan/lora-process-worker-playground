# Re-asking Risk System after first-time survey data — design options

Status: **decided — option 1 (`trigger_seq` counter) is implemented** (2026-09-26). `stage_token`
stays retired, and `trigger_seq` is the only cursor field. This note records why an "updates only"
design without any counter was not enough, and which alternatives were weighed, so the choice can be
revisited.

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

### 1. `trigger_seq` counter — chosen, implemented

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

### 3. Split RS into one step per data set

- **How it works:** one RS step per data set, each requiring its page's field (`asset.condition`,
  `income.verified_amount`, ...). A step that has never run becomes runnable as soon as its field
  first appears, with no counter and no `Rollback`. This is the purest form of data triggering.
- **Costs:**
  - Several steps write the same `risk_system.*` fields, so history replay could restore an older
    verdict, and the verdict step has to tell which call is the latest.
  - testcli's verdict lookup must match several activity names.
  - Shared fields (both surveys' income page) and re-submission need special handling.
- **Verdict:** feasible, but the most fragile option.

### 4. Narrow SDK hook: task completion re-queues named steps

- **How it works:** an explicit edge on the step, "when this task records a partial or final
  completion, re-queue these steps". It fires only on human submissions, not on data propagation,
  so it avoids the calculator ↔ survey loop.
- **Cost:** a shared-SDK change, and the design becomes event-driven rather than data-driven.

### Ruled out

- **Seeding placeholder values into every page field** so that first writes become updates. Survey
  completion and underwriting eligibility depend on whether fields are present, so both would break
  silently.
- **Re-triggering through `data-forward` from the task side.** It never calls `Rollback`.
- **An opt-in "re-arm on first set" SDK flag.** It breaks convergence (the tc1 deadlock above).

## Decision

Option 1 is implemented. Move to option 2 if a counter with no business meaning becomes
unacceptable; the mechanism is identical. Revisit option 3 only if the requirement becomes "no
cursor-like field at all".
