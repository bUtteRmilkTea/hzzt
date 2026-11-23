Work through plan/feedback-be-2-plan.md in this order: Phase F, then G, then H, then I, then J, then K.

Q5 is answered (yes): do G-T7, flip crypto with an instrument to AUTO, and never touch avg_buy_price, fees or manual_price (test it). Q10 is answered (PR #24 merged, migrations end at 00016).

PRE-FLIGHT (before Phase F): run `git branch --no-merged development`. On 2026-11-23 feat/be-b-wrapper-grouping, be-c, be-d, be-e and feat/be-networth-investments-series were NOT merged, so development stops at migration 00013 and has no grouping.go. If any are still unmerged, STOP and ask which base to use (plan section 10, Q10). Do not start Phase F until answered.

Base branch: development (the current branch). Branch each phase from development. Only if a phase needs an earlier phase that is not merged yet, branch it from that phase's branch and say so in the PR.

Branch names carry NO phase letter: F -> feat/ledger-correctness, G -> feat/crypto-cmc-pricing, H -> feat/fx-and-defaults, I -> feat/wallet-delete-impact, J -> feat/wrapper-single-item-test, K -> feat/growth-history-series (read section 11 of the plan: it adds Phase K and resolves Q5 and Q10).

Decisions already made: none yet. D-B1 to D-B7 are "Proposed, recommended" in the plan: implement the recommended option unless a Q below blocks it.
Blocked by an open question (do NOT guess, skip that task, leave it unticked, and list it in your report):
- the 5.5% data migration part of H-T4 (Q6)
- localized seed names (D-B3 / Q8): only add `system_key`, do not translate seeds
- any title-based reclassification of "Saldo awal" (Q7): only the explicit PATCH action

For each phase:
0. make a new branch with the format feat/[phase name], no letter
1. Write the failing test that proves the inferred cause FIRST (the plan says every cause is inferred), then implement fully. If the cause turns out to be wrong, stop and report before changing the design.
2. Run the tests (and add tests for what you built): make lint test test-e2e, regenerate sqlc and swagger when queries or handlers change. Make sure the app starts (docker compose up, GET /health).
   Run every new migration up and down on a dev database.
3. Only if everything passes: tick off the task in the plan and commit
   with a message like "[feat]: [branch name]". -m "[phase-t] [what you add]"

   if all sub phase alr done for a phase push it and make a pr to development branch
4. Then move on to the next phase.

Plan corrections: read section 10 of the plan first, it overrides earlier sections (migration numbers are examples: use the highest in internal/db/migrations + 1 at branch time; categoryIds and ?force already exist; error code is FX_RATE_UNAVAILABLE).
When a "verify" shows a plan assumption is wrong: if it changes a Decision (D-B*), stop and ask; otherwise add a dated note to the plan and continue.
Commit the plan file edits (ticks, notes) with the phase they belong to. Tick per task; push and open the PR when every task of the phase is ticked or listed as skipped/blocked (say which in the PR body). If you cannot find a cited file or symbol, say so, do not invent it.

Stop and ask me if:
- tests keep failing after a few honest attempts
- you need a secret, API key, or a decision only I can make
- a migration would change more rows than the plan expects (print the count first)
- a "verify" item in the plan shows the plan's assumption is wrong

API keys: CMC_API_KEY, MASSIVE_API_KEY and FREECURRENCY_API_KEY are read from .env only (already stored there). Tests use the fake CMC/stock/FX servers in test/e2e, never the real APIs. Never print, log, or put a key in a test, plan or commit.

Never commit .env or secrets. Don't skip phases or rewrite the plan silently (if the plan must change, add a dated note in the plan and tell me).
