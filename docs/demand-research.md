# herdr-loop demand mining — findings

Methodology: `gh` CLI ground truth only (issue lists, `gh issue view --comments`, `gh api graphql` for Discussions, README fetches). Three background subagents ran in parallel (herdr core + awesome-herdr + Discussions / 9 herdr orchestration-plugin repos / wider multi-agent ecosystem) — all three completed successfully, contrary to an earlier assumption mid-task that they'd died; I supplemented the herdr-core stream myself once its findings landed late. Everything below is read, not scraped. Design pillars referenced: **(1)** TOML loop manifests (slots+rules+predicates), **(2)** event-driven state reconciliation, **(3)** git worktree per slot, **(4)** blocked-agent escalation by default, **(5)** per-kind concurrency caps, **(6)** file-based handoff, **(7)** parse-time loop-termination validation, **(8)** resume after crash.

**Important methodology finding**: herdrdev/herdr's own issue-bot triages *all* feature requests out of Issues into Discussions ("issues are only for reproducible bugs... feature requests belong in discussions"). 863 Discussions exist. Anyone mining only `gh issue list` on herdr core will structurally miss the demand signal — it lives in Discussions.

**Not covered** (ran out of scope before team-lead's stop-and-ship instruction): sarmientoF/herdr-pr-loop, SecretAardvark/pi-overseer, machine-machine/herdr-factory-loop-skill — all three surfaced via awesome-herdr's "Orchestrate" category as close analogs, none actually mined for issues/README. Flagging rather than guessing at their content.

---

## Ranked demand list

### 1. Reliable inter-agent message delivery (queue-if-busy, confirm delivery)
- **Want**: `agent.prompt`/send should queue when the target is mid-turn and confirm delivery, instead of typing into a live turn and silently losing the text.
- **Evidence**: [herdr discussion #2401](https://github.com/herdrdev/herdr/discussions/2401) "agent.send: queue messages server-side and deliver when the target agent is idle" (1 upvote, direct quote: *"there is no reliable way for one agent to get a message to another... two senders wa[nt to send at once]"*). Corroborating: [herdr issue #2306](https://github.com/herdrdev/herdr/issues/2306) "agent.prompt into a blocked agent reports agent_prompted but drops the text." Same failure class outside herdr: [AltanS/collie #54](https://github.com/AltanS/collie/issues/54) "Reply guard stalls on busy panes, text accumulates unsubmitted, no recovery path," [collie #34](https://github.com/AltanS/collie/issues/34) "PWA send renders in pane input but never reaches buffer," [dcolinmorgan/herdr-remote #17](https://github.com/dcolinmorgan/herdr-remote/issues/17) "Telegram bot not sending enter to codex."
- **Who wants it**: One herdr discussion author (multi-agent orchestrator builder) + herdr issue reporter, plus 3 independent single-reporter cases in the adjacent ecosystem — same failure class, no cross-repo coordination, so read as a recurring structural gap, not a loud minority.
- **Design coverage**: our file-based handoff (pillar 6) *avoids* this exact failure mode by design (no pane-typing race) — this is validation the choice is right, not a gap. Worth stating explicitly in docs/marketing: "we don't type into panes for handoff because that's exactly what breaks elsewhere."
- **Effort / timing**: N/A — already covered by design intent. **Verify in v1** that file-handoff truly has no race-prone fallback path.

### 2. Per-slot progress + append-only log surfaced to the orchestrator
- **Want**: workers should be able to publish structured numeric progress and an append-only log, not just free-text status, so a controller watching N parallel agents can tell how far along each one is.
- **Evidence**: [herdr discussion #713](https://github.com/herdrdev/herdr/discussions/713) "Published agent progress + sidebar log feed for orchestration" (2 upvotes, 2 distinct commenters). Quote: *"I run a multi-agent orchestration setup (one orchestrator pane driving N workers over the socket API)... I'm currently on cmux and evaluating a move to herdr... a worker can only publish a free-text custom_status. There's no way to publish numeric progress or an append-only log. With 5-10 workers running in parallel, the orchestrator can't tell how far along each one is."* Second commenter (Golden-Pigeon): *"+1 on this. I'm hitting the same wall from the plugin side."* Related: [herdr discussion #2529](https://github.com/herdrdev/herdr/discussions/2529) "Silence agent-done notifications for orchestrator-driven workers" — 2 distinct users, notification noise from the same controller-drives-workers pattern.
- **Who wants it**: 2 independent users on #713 (one explicitly says this is *"what's holding me back from switching my orchestrator from cmux to herdr"* — a real herdr-adoption blocker), 2 more on #2529 — 4 distinct people total describing the same shape of pain (watching N parallel workers from one control point).
- **Design coverage**: NOT currently planned. Our TOML manifest tracks slot state internally but nothing here proposes a progress/log surface a human (or another tool) can watch live.
- **Effort / timing**: **Medium**. Real design decision (schema for progress events, where they surface — herdr sidebar plugin API vs our own CLI/TUI). **v1** if herdr-loop wants to be watchable mid-run rather than a black box until convergence; otherwise v1.1.

### 3. Escalation must never destroy uncommitted work
- **Want**: timeout/escalation paths must not force-delete a worktree that has uncommitted-but-real work in it.
- **Evidence**: [sean1588/herdr-orchestrator issue #34](https://github.com/sean1588/herdr-orchestrator/issues/34) (owner's own dogfood report, one of 5 linked issues #34-38). Quote: *"no-PR escalation --force-removes the worktree, discarding uncommitted work... destroyed a completed-but-uncommitted implementation."* Same issue also states: *"the tool tends to stall silently at a timeout instead of saying why."*
- **Who wants it**: One person — but it's the maintainer of the closest prior-art project, reporting a real production data-loss incident against a design (worktree-per-task, timeout-based escalation) structurally identical to ours.
- **Design coverage**: our "blocked-agent escalation by default, never blind auto-approve" (pillar 4) covers *approval* bypass, not *teardown safety*. This is a distinct invariant we don't currently state: escalation/timeout paths must always commit-or-stash before any worktree teardown.
- **Effort / timing**: **Small** — an invariant + a guard, not new architecture. **v1**, non-negotiable given it's a documented real-world data-loss incident in the closest analog.

### 4. Escalation needs a stated reason, not a silent timeout
- **Want**: when an agent is escalated to a human, say *why* (stalled on X, predicate Y never became true) rather than just reporting a bare timeout.
- **Evidence**: same [herdr-orchestrator #34](https://github.com/sean1588/herdr-orchestrator/issues/34) thread. Quote: *"the tool tends to stall silently at a timeout instead of saying why — every item below is either that, or a data-safety gap."* Cross-referenced pattern: [herdr discussion #1635](https://github.com/herdrdev/herdr/discussions/1635) "Direction: detecting Pi 'blocked' state for human-input tools" — pane reports **working** or **idle** instead of **blocked** when actually waiting on human input; [herdr discussion #1346](https://github.com/herdrdev/herdr/discussions/1346) same for OpenCode; [BloopAI/vibe-kanban #3227](https://github.com/BloopAI/vibe-kanban/issues/3227) session stuck "running" after WebSocket close is misclassified as still-alive; [herdr issue #509](https://github.com/herdrdev/herdr/issues/509) "'workflow' runs show idle while sub-agents are still working."
- **Who wants it**: 1 direct (orchestrator maintainer) + at least 4 independent corroborating reports of the general "status lies about what's actually happening" failure across herdr, vibe-kanban.
- **Design coverage**: our escalation-by-default (pillar 4) triggers correctly per the design doc, but nothing specifies escalation messages must carry a structured reason (which predicate failed / which rule fired / how long stalled). Gap.
- **Effort / timing**: **Small**. **v1** — cheap to build in from the start (attach reason to every escalation event), expensive to retrofit once the event schema is frozen.

### 5. Quota/usage-limit-aware auto-resume
- **Want**: detect a provider's "you've hit your usage limit, resets at HH:MM" message, wait it out, and auto-resume instead of requiring a human to notice and retype "continue."
- **Evidence**: [herdr discussion #724](https://github.com/herdrdev/herdr/discussions/724) "Auto-resume agent on provider usage-limit reset" (2 upvotes). Quote: *"When a model hits its session/usage limit mid-run, the agent stops and I have to manually reset it when I'm back."* One corroborating comment: *"I came here to add this feature to the list. This is going to be super helpful."*
- **Who wants it**: 2 distinct people on the herdr discussion. Precedent that this is a real recurring pattern: firegnu/herdr-loop-lab's exit-code design reserves code `3` specifically for "quota-hit, resumable" — an independent project already treats quota exhaustion as a first-class loop-pause condition, not a crash.
- **Design coverage**: our "resume after crash" (pillar 8) is framed around process/machine crash. Quota exhaustion is a distinct, common, non-crash pause condition that needs the same resume machinery triggered by a different signal (parsed provider message, not process death).
- **Effort / timing**: **Medium** (needs per-harness output parsing to detect the specific quota message format, which drifts). **v1.1** — real want, but per-harness parsing is exactly the kind of high-maintenance surface better proven after v1 ships with the harnesses that already have clean status signals.

### 6. `.claude.json`/shared-config concurrent-write corruption — locking or atomic writes
- **Want**: cross-process file lock or atomic temp-file+rename writes for any config file multiple concurrent sessions touch, instead of read-modify-write races.
- **Evidence**: [anthropics/claude-code #29217](https://github.com/anthropics/claude-code/issues/29217) "Race condition: .claude.json corrupted by concurrent writes from multiple sessions" — 11 comments, 8+ independent reporters (sstklen, DomenicoDomotz, callmesomesh, tsantoso79, nemekath, ProductOfAmerica and others), one built a third-party fix tool (`cozempic`, `pip install cozempic`, doctor/--fix mode). Related: `anthropics/claude-code #85236` "MCP OAuth: per-process refresh lock lets concurrent sessions race token refresh." Quote: *"The `.claude.json` file write operation is not atomic... Same fix applies: atomic writes (temp file + rename) for all shared config files, not just `.claude.json`."*
- **Who wants it**: Largest reporter count in the entire sweep (8+ distinct people), but **this is upstream Claude Code demand, not herdr-ecosystem demand** — zero herdr/herdr-plugin issues mention credential/config races (explicitly searched, zero hits — `gh search issues` across agentbox/orchestrator/herdr-plus). Issue auto-closed by stale-bot after 7 days, unresolved.
- **Design coverage**: our per-kind concurrency caps (pillar 5) exist explicitly to avoid credential races. This is real, well-evidenced upstream — but be honest that no herdr *user* has asked for this; the demand signal is "Claude Code itself has an unfixed known bug," and our cap is a mitigation for a documented failure mode, not a response to user requests within our own ecosystem.
- **Effort / timing**: pillar already in design at no extra cost. Keep it — just don't market it as "users asked for this," market it as "we designed around a known unfixed upstream bug."

### 7. Event-handler ordering guarantee (or explicit non-guarantee) on the plugin event bus
- **Want**: either guarantee subscriber execution order for the same herdr event, or document clearly that there's none, so plugins don't assume it.
- **Evidence**: [persiyanov/herdr-reviewr #5](https://github.com/persiyanov/herdr-reviewr/issues/5): *"Both reviewr and herdr-plus subscribe to `worktree.created`, and herdr runs the handlers in no guaranteed order."*
- **Who wants it**: 1 reporter, but it's a real architectural landmine for exactly our design (pillar 2, event-driven reconciliation).
- **Design coverage**: directly actionable — confirms our reconciliation logic must be idempotent and order-independent by construction, can't assume "our handler runs before/after X." Not a feature to build, a constraint to design against.
- **Effort / timing**: **Small** (a design constraint, not new code). **v1**, immediately.

### 8. Config-drift / silent-success failure class (settable field silently never read; "started" reported when it didn't actually bind)
- **Evidence**: [madarco/agentbox #301](https://github.com/madarco/agentbox/issues/301) `relay.port` documented and settable but silently never read; [#302](https://github.com/madarco/agentbox/issues/302) start command reports success when the relay failed to bind.
- **Who wants it**: 1 reporter each, but same shape as finding #4 above (status lies about ground truth) — recurring pattern class across totally unrelated projects.
- **Design coverage**: not a feature request, a testing discipline note — our loop manifest's parse-time validation (pillar 7) should extend to runtime: verify a slot actually reached the state it claims, don't trust an exit code alone.
- **Effort / timing**: **Small**, fold into existing pillar 7/8 testing, not separate.

### 9. Escalation/permission-prompt notification centralization across concurrent agents
- **Want**: one place to see/answer "which of my N agents needs approval right now" instead of tabbing through every terminal.
- **Evidence**: [anthropics/claude-code #70591](https://github.com/anthropics/claude-code/issues/70591) "[FEATURE] Centralized Permission & Approval Notifications for Multi-Agent Workflows" — multiple distinct commenters, quote: *"As the number of concurrent agents increases, terminal switching becomes a significant source of friction."* One user built a hook-based stdout-aggregation workaround, another (AminDhouib) built a third-party push notifier. Herdr-native precedent: [herdr discussion #1970](https://github.com/herdrdev/herdr/discussions/1970) "Prompt Reply — answer a blocked agent's permission prompt from the macOS notification itself" (Show and tell, working implementation already exists as a plugin).
- **Who wants it**: multiple distinct commenters upstream + one working herdr plugin already built by a third party — real, proven demand with existing partial solutions.
- **Design coverage**: our "escalation by default, never blind auto-approve" (pillar 4) creates escalation events but doesn't currently specify *where they surface*. If escalations only print to a per-slot log, this pain recurs inside herdr-loop.
- **Effort / timing**: **Small-Medium** — piggyback on herdr's existing OS-notification integration (#1970 proves the plumbing exists) rather than building a new channel. **v1** for the notification hook, v1.1 for a dedicated dashboard if wanted.

### 10. Resume must preserve original launch flags (not just session ID)
- **Want**: `resume_agents_on_restore` should restore the exact flags a pane was launched with (e.g. `--dangerously-skip-permissions`), not just the session ID — currently the flag is silently dropped and the agent starts asking for permission again on every restart.
- **Evidence**: [herdr discussion #2027](https://github.com/herdrdev/herdr/discussions/2027) "Resume drops the flags an agent pane was launched with" (2 upvotes). Quote includes the exact error a user hits trying to work around it client-side: *"Cannot set permission mode to bypassPermissions because the session was not launched with --dangerously-skip-permissions."*
- **Who wants it**: 1 reporter, unresolved, no maintainer response.
- **Design coverage**: directly relevant to pillar 8 (resume after crash) — if herdr-loop launches agents with specific flags per slot, our resume path must not go through a lossy channel like herdr's native restore. Worth an explicit test case.
- **Effort / timing**: **Small** — verification/test-case item against our own resume path, not new feature work. **v1**.

### 11. Bounded self-heal/retry loop as a first-class primitive (`on_fail: goto + max_loops`)
- **Evidence**: bon5co/bermuda README (`docs/flows.md`, fetched via `gh api repos/bon5co/bermuda/contents/README.md`): *"a reviewer's `on_fail: {goto: patch, max_loops: 2}` re-runs the step that caused what it rejected, bounded and told why."* Also closed issue [bermuda #43](https://github.com/bon5co/bermuda/issues/43) "flows: loopback for self-heal when an adversarial step fails" (already shipped). Independent precedent: razajamil/herdr-factory `max_bounces` (default 6, per-belt override); sean1588/herdr-orchestrator names "loops terminate — every cycle has a retry cap or a timeout" as a named safety invariant (#6).
- **Who wants it**: no open issue thread (feature already shipped in response to internal/self-identified need), but **three independent projects converged on the same primitive** (bounded goto-loop with a cap) — strong signal this is table-stakes, not differentiating.
- **Design coverage**: this is effectively what our parse-time loop-termination validation (pillar 7) already targets. **Not a gap — validation that the pillar is correctly prioritized.** But also means it's *not* a differentiator; three prior-art projects already do it.
- **Effort / timing**: already in scope, no change.

---

## TOP 5 things to build that we are NOT currently planning

1. **Structured escalation reason on every escalation event** (item #4) — cheap now, expensive to retrofit; four independent projects show "status/reason lies or is silent" as a recurring failure class.
2. **Never-destroy-uncommitted-work invariant on any teardown/escalation path** (item #3) — small guard, backed by a real documented data-loss incident in the closest prior-art project.
3. **Per-slot progress/log publishing surfaced to a controller** (item #2) — the only finding with a user explicitly saying a missing feature blocked their adoption of herdr itself; 4 independent people want this shape of visibility.
4. **Idempotent/order-independent reconciliation as an explicit design constraint** (item #7) — herdr's own event bus has no subscriber-order guarantee; must design pillar 2 around that from day one, not discover it in production.
5. **Escalation-to-notification hook, piggybacking herdr's existing OS-notification plumbing** (item #9) — proven demand upstream (claude-code #70591) and a working third-party herdr plugin (#1970) already shows the integration point exists.

## Cuts / things the evidence suggests nobody specifically asked for from us

- **Per-kind concurrency caps to avoid credential races**: keep it (real bug, `.claude.json` corruption, 8+ reporters) — but be honest internally that zero herdr-ecosystem users have asked for this. The demand is "Claude Code has a known unfixed bug," not "herdr-loop users want caps." Don't market it as user-requested; market it as defensive design against a documented upstream failure.
- **Parse-time loop-termination validation**: not wrong to keep, but not novel — bermuda, herdr-factory, and herdr-orchestrator all independently converged on bounded-loop-with-cap. This is table stakes across the whole space, not a differentiator worth leading marketing with.
- **Nothing in the design evidence suggests should be cut outright** — every pillar has either direct validation (file-handoff avoids a documented failure mode) or a real gap next to it (escalation reason, teardown safety) rather than being unwanted. The stronger finding is *missing* pillars (progress/log surface, escalation notification routing), not *excess* ones.

## Single most common unsolved pain across everything read

**Silent status/state lies — an agent, slot, or process is reported as fine (idle/working/started) when it is actually stalled, blocked, or dead, and nobody finds out until they go looking.** Independent evidence across 9+ unrelated repos:
- herdr #509 ("workflow runs show idle while sub-agents are still working")
- herdr #2618/#2591 (OpenCode stuck blocked after subagent permission resolves)
- herdr discussions #1635, #1346 (Pi/OpenCode blocked state reported as working/idle)
- vibe-kanban #3227 (session stuck "running" after WebSocket close misclassified as alive)
- sean1588/herdr-orchestrator #34 ("stalls silently at a timeout instead of saying why")
- madarco/agentbox #302 (start reports success when relay failed to bind)
- AltanS/collie #54, #34 (input silently lost, no recovery signal)
- dcolinmorgan/herdr-remote #17 (enter key silently not delivered to codex)

This is the strongest, most cross-cutting finding in the whole sweep — it recurs across completely unrelated codebases and teams, meaning it is a structural property of "wrap a CLI agent and infer its state from output," not a bug in any one implementation. herdr-loop's event-driven reconciliation (pillar 2) is well-positioned to address this *if and only if* every state transition it infers is treated as unverified until corroborated (e.g., don't trust "idle" without also checking the process is actually alive and the predicate that would explain idleness actually holds) — this should be a stated design principle, not just an implementation detail.
