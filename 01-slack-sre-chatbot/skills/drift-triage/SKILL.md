---
name: drift-triage
description: Triage a release problem where git (the GitOps overlays) and the cluster (Argo CD) disagree about which version of a service runs in an environment. Use when asked to debug, explain or rank drift, to find the root cause of a disagreement between git and the cluster, to say whether users are affected, or to suggest a fix direction. Covers the kinds of drift, how to recognise each from the fact rows, how severe each is, and how to write the root cause.
---

# Drift triage

Explain why git and the cluster disagree, how bad it is, and what to do next. Read-only: you never change anything.

## Procedure

1. **Start from the facts in the first message.** Each row is one service in one environment, with the git version, the running version, Argo CD sync and health, and factual flags. Do not re-fetch what the rows already say.
2. **Classify the worst disagreeing row** with `references/kinds.md`. It maps the flags and the note wording to a kind and a severity. Highest severity first, production first among equals.
3. **Check Argo CD.** Read the row's sync status and `LastMessage`. Before building a theory, rule out the boring explanation, that Argo CD has not looked at git recently:
   - If the first message says the apps were just refreshed, do not call `refresh_app`. The rows predate that refresh, so include `get_service_facts(service)` in your next turn to see whether the disagreement survived it.
   - Otherwise call `refresh_app(env, service)`, then `get_service_facts(service)` to see whether the disagreement survives a fresh look.
4. **Read the overlay when the cause depends on it.** `get_overlay(env)` shows what git actually pins.
5. **Check symptoms and reports.** Calls that do not depend on each other can go in one turn: call `get_overlay`, `get_service_health` and `search_tickets` (and `get_service_facts` from step 3) in parallel rather than one per turn. Every extra turn is a model call against the time budget.
   - `get_service_health(env, service, hours)` for error rate and traffic. Use 6 hours unless you have a reason.
   - `search_tickets(service, days)` for an existing report. Use 30 days.
   - Either may be unconfigured. If so, say impact is unknown instead of guessing.
6. **Use `run_code` only when a calculation or comparison over the evidence would change the conclusion.** Examples: diff two overlays, compare a Grafana series against a deploy time, count failures in a JIRA export. Rules:
   - Evidence files live under `/data` only: `facts.json`, `overlay-<env>.yaml`, `grafana-<env>-<service>.json`, `jira-<service>.json`.
   - Python or bash. No network.
   - The tool may be unavailable. Carry on without it.
   - Do not use it to restate what the facts already show.
7. **Write the answer** in the shape from `references/root-cause-format.md`.

## Rules

- Do not speculate beyond the evidence. Never invent versions, ticket IDs or metrics.
- Only suggest a fix when editing an overlay would actually help. Direction depends on the kind; see the `gitops-fix` skill's `references/direction.md`. Otherwise leave `suggested_fix` out and say a human is needed.

## References

- `references/kinds.md`: the six kinds, how each shows up in the flags and notes, and severity.
- `references/root-cause-format.md`: the four-line root cause and the structured answer fields.
