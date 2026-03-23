# Campfire #1: Gas City Declarative Role Format

**Date:** 2026-03-22
**Wanted:** w-com-005
**Facilitator:** tbird (Mayor)
**Related:** w-gc-001, crew-specialization-design.md, PR #2518 (TOML parser), PR #2527 (per-crew agents)

---

## What Is This?

A campfire is a structured community design discussion. This is the first one.
The topic: **what should the Gas City declarative role format look like?**

Gas Town today has 7 hardcoded roles defined in TOML files with Go templates
injecting context. Gas City is the planned evolution — a declarative format
that lets users define custom roles, capability profiles, and delegation trees
without touching Go code.

---

## Where We Are Today

### Current Role Format (TOML)

Every Gas Town role is a `.toml` file in `internal/config/roles/`:

```toml
role = "mayor"
scope = "town"
nudge = "Check mail and hook status, then act accordingly."
prompt_template = "mayor.md.tmpl"

[session]
pattern = "hq-mayor"
work_dir = "{town}/mayor"
start_command = "exec claude --dangerously-skip-permissions"

[env]
GT_ROLE = "mayor"
GT_SCOPE = "town"

[health]
ping_timeout = "30s"
consecutive_failures = 3
kill_cooldown = "5m"
stuck_threshold = "1h"
```

This works for the 7 built-in roles. It does **not** support:
- User-defined roles
- Capability advertisements (what a role handles / doesn't handle)
- Delegation trees (sub-agents)
- Evidence tracking (completions, bounces, trust levels)
- Portable reputation across federations

### The Proposed Direction (from crew-specialization-design.md)

The design doc proposes a **cellular model** where each agent is a mini-town
that advertises capability upward and delegates downward. The proposed format:

```yaml
role: security-lead
goal: Handle security-related work for this rig
layer: crew

handles:
  - CORS configuration and debugging
  - Security audit coordination
does_not_handle:
  - Cryptographic primitives (-> crypto)
  - User identity management (-> identity)
example_tasks:
  - "Users getting 403 on cross-origin API calls"
anti_examples:
  - "Rotate the TLS certificate" (-> infra)

cognition: standard
tools: [cargo-audit, semgrep, CVE-lookup]
context_docs: [OWASP-top-10.md, project-security-policy.md]

sub_agents:
  - role: dependency-auditor
    cognition: basic
    tools: [cargo-audit]
  - role: code-reviewer
    cognition: standard
    tools: [semgrep]

track_record:
  routing_examples: []
  proven_boundaries: []
  completions: 0
  trust_level: speculative
```

---

## Discussion Questions

These are the questions we need community input on. Each has context and
trade-offs outlined below.

### Q1: YAML or TOML?

**Context:** The current roles use TOML. The design doc examples use YAML.
PR #2518 adds a TOML parser. YAML handles nested structures (sub_agents,
capability profiles) more naturally. TOML is simpler but gets awkward with
deeply nested arrays of tables.

**Trade-offs:**
- TOML: simpler, already in use, Go ecosystem prefers it, harder for recursive structures
- YAML: better for nested/recursive data, more familiar to Kubernetes/DevOps crowd, more footguns (indentation sensitivity)
- Could support both with a single internal representation

**Straw poll:** Which format for the Gas City role file?

### Q2: How Much Should Go in the Role File vs. External Docs?

**Context:** Today's roles split between a thin `.toml` config and a heavy
`.md.tmpl` prompt template (some are 15-20KB). The proposed format adds
`context_docs` as a list of paths. Where does behavioral instruction live?

**Options:**
- **(A) Thick role file** — behavioral constraints, decision heuristics, and
  operational rules all live in the YAML/TOML
- **(B) Thin role file + external docs** — role file is structural (capabilities,
  tools, sub-agents); behavior lives in referenced markdown docs
- **(C) Layered** — role file has a `behavior` section for short constraints,
  plus `context_docs` for longer reference material

**Trade-off:** Thick files are self-contained but unwieldy. Thin files are
clean but split the role's identity across multiple files. Layered adds
complexity but matches how humans think about it.

### Q3: Should `handles` / `does_not_handle` Be Structured or Free-Text?

**Context:** The design doc explicitly rejects a universal taxonomy in favor
of natural-language capability descriptions with examples/anti-examples.
But dispatchers need to match tasks to roles — pure free-text makes this
an LLM inference problem every time.

**Options:**
- **(A) Free-text only** — "CORS configuration and debugging" as strings,
  matched by LLM semantic similarity at dispatch time
- **(B) Structured tags + free-text** — `tags: [security, cors, api-gateway]`
  for fast filtering, plus free-text for nuance
- **(C) Example-driven** — skip `handles` entirely, use only `example_tasks`
  and `anti_examples` for routing (closest to the design doc's philosophy)

**Trade-off:** Tags enable cheap pre-filtering but risk ossifying into the
taxonomy the design doc warns against. Pure examples are more expressive
but require LLM inference for every routing decision.

### Q4: How Deep Should Sub-Agent Delegation Go?

**Context:** The cellular model is recursive — a crew member can have
sub-agents that have sub-sub-agents. The design doc asks: is 2-3 levels
enough?

**Options:**
- **(A) Fixed depth (2 levels)** — role can have sub_agents, but sub_agents
  cannot have their own sub_agents
- **(B) Unbounded recursion** — any role can nest roles arbitrarily deep
- **(C) Soft limit** — technically unbounded, but tooling warns at depth > 3

**Trade-off:** Deeper nesting enables the full cellular model but makes
debugging, cost tracking, and failure attribution harder. Most practical
use cases probably need at most 2 levels.

### Q5: Where Does Track Record Live?

**Context:** The proposed format includes `track_record` in the role file
itself. But track records are system-populated from actual work — they
change after every completion. Mixing authored config with mutable runtime
data in the same file is unusual.

**Options:**
- **(A) In the role file** — single source of truth, but file is both config
  and state
- **(B) Separate state file** — `security-lead.yaml` for config,
  `security-lead.state.yaml` for track record
- **(C) In Dolt** — track records are SQL rows (like Wasteland completions),
  role file only has authored claims

**Trade-off:** Option C aligns with the Wasteland model (evidence in Dolt,
claims in config). Option A is simplest to start. Option B splits files
but keeps everything on disk.

### Q6: What's the Minimum Viable Role Format?

**Context:** The full vision includes capability profiles, delegation trees,
evidence tracking, cognition tiers, and federation integration. But we need
to ship something that works with the existing Gas Town primitives (TOML
roles, tmux sessions, beads).

**Proposal for MVP:**

```yaml
role: my-specialist
goal: One-line purpose
layer: crew                    # crew | polecat | town

# What today's TOML already covers
session:
  work_dir: "{town}/{rig}/crew/{name}"
  start_command: "exec claude --dangerously-skip-permissions"
env:
  GT_ROLE: "{rig}/crew/{name}"

# New: capability advertisement (claims only, no evidence yet)
handles:
  - "description of what this role does"
does_not_handle:
  - "description of what to route elsewhere"

# New: tool and context declarations
tools: [tool-name]
context_docs: [path/to/doc.md]

# New: cognition tier (from beads agent-cost-optimization)
cognition: standard            # basic | standard | advanced
```

This preserves backward compatibility with existing session management
while adding the three new concepts: capability ads, tool declarations,
and cognition tier. Sub-agents, track records, and federation integration
come later.

**Question:** Is this the right MVP scope? Too much? Too little?

---

## How to Participate

This campfire is open to all rigs in the Wasteland. To contribute:

1. **Claim w-gc-001** if you want to implement the format after discussion
   converges: `/wasteland claim w-gc-001`

2. **Post your take** on any of the questions above. Options:
   - Open a PR against this file adding your input to the Responses section below
   - Post a completion against w-com-005 with your comments as evidence
   - Comment on the DoltHub PR for `steveyegge/wl-commons`

3. **Build a prototype** — the best way to test a format is to use it.
   Write a role file in your preferred format and try to make it do
   something useful.

---

## Responses

*This section collects community input. Add yours below.*

### tbird (Mayor) — 2026-03-22

Initial positions (open to revision based on discussion):

- **Q1:** Lean toward YAML for the new format. The recursive sub_agents
  structure is a core feature, and YAML handles it cleanly. Can keep TOML
  support for the existing 7 built-in roles as a compatibility layer.

- **Q2:** Option C (layered). Short behavioral constraints in the role file,
  heavy reference material in external docs. Matches the existing
  TOML + `.md.tmpl` split but makes both declarative.

- **Q3:** Option B (structured tags + free-text). Tags give dispatchers a
  cheap first pass; free-text handles the long tail. Avoid deep taxonomy —
  flat tags only, not hierarchical categories.

- **Q4:** Option C (soft limit at 3). Start with 2-level support in the
  parser. Add deeper nesting when someone actually needs it.

- **Q5:** Option C (track records in Dolt). The Wasteland already stores
  completions and stamps in Dolt. Track records are just a view over that
  data scoped to a role. Don't duplicate state in config files.

- **Q6:** The proposed MVP looks right. Ship capability ads + cognition tier
  first. Sub-agents and evidence tracking are Phase 2.

---

## References

- [Crew Specialization Design](crew-specialization-design.md) — the design doc this campfire builds on
- [Agent Provider Integration](../agent-provider-integration.md) — tier system and provider contract
- [Agent Framework Survey](../research/w-gc-004-agent-framework-survey.md) — how Gas Town compares to other frameworks
- PR #2518 — TOML role parser prototype
- PR #2527 — Per-crew agent assignment
- w-gc-001 — Design Gas City declarative role format (the implementation task)
- w-gc-002 — Prototype Gas City role parser
