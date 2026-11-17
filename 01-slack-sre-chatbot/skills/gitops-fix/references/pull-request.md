# Pull request conventions

## Title

| Situation | Title |
| --- | --- |
| Promotion gap | `promote(<env>): <svc> <from> -> <to>` |
| Version skew or failed sync | `revert(<env>): pin <svc> to the running <ver>` |

`<env>` is the environment whose overlay you edit. `<from>` is the tag now in the overlay. `<to>` is the new tag. `<ver>` is the running version you pin to.

Examples:

- `promote(prod): product-catalog v1.1.0 -> v1.2.0`
- `revert(prod): pin product-catalog to the running v1.2.0`

## Rationale

One or two sentences, passed as `rationale` and reused under "Why".

- Promotion: `<leading env> already runs <to> and is healthy. This promotes the same version to <env>.`
- Pin back: `git declares <from> but <env> has been running <to>. Argo CD cannot land <from>, so git is pinned back to what is actually serving. Roll forward again once <from> is known good.`

## Body

Use these sections, in this order, as `pr_body`:

```markdown
Opened by the SRE Agent.

## What drifted

<one line: for example prod/product-catalog is on v1.1.0, stage already has v1.2.0>

## What this PR changes

`<overlay path>`: `<from>` -> `<to>`

## Why

<the rationale>

## Before merging

<checklist, below>

---
Generated from live Argo CD state, not from a template.
```

Keep "What drifted" to the facts: the versions, the environments and, for a failed sync, the sync message. Do not invent anything.

## "Before merging" checklist

Promotion gap:

```markdown
- [ ] `<leading env>` has been running `<to>` long enough to trust it
- [ ] No open incident on this service
```

Version skew or failed sync:

```markdown
- [ ] Understood why `<from>` could not be deployed
- [ ] A follow-up exists to roll forward past `<to>`
```

For an edit outside the version tags, such as replicas, write checklist items in the same spirit: confirm the cause, and confirm a follow-up exists.

The agent does not merge. Say so only if asked. Merging stays with a human.
