---
name: gitops-fix
description: Turn a confirmed git-versus-cluster drift finding into a pull request that edits one GitOps overlay, or decline when no overlay edit would help. Use when asked to fix, remediate, promote, pin or roll back a service's version in an environment overlay. Covers fix direction (promote versus pin back), the protected overlay fields, pull request title and body conventions, and when to decline.
---

# GitOps fix

Edit one overlay so git tells the truth again, as a pull request a human reviews. You never merge.

## Procedure

1. **Read the overlay first.** Call `get_overlay(env)` for the environment you are fixing. Never edit from memory or from the fact rows alone.
2. **Pick the direction** with `references/direction.md`. Getting it backwards is the easiest mistake here. Promotion moves the tag forward. Skew and failed sync pin it back.
3. **Check the fix is still needed.** If the overlay already pins the target version, someone fixed it or Argo CD has not caught up. Call `decline_fix` and say so.
4. **Make the minimal edit.** Change only the field the fix needs, usually one `newTag`. Keep every other line, comment and the formatting as it is. `new_content` is the complete file, not a diff.
5. **Write the title and body** with `references/pull-request.md`.
6. **Finish with exactly one of `propose_fix` or `decline_fix`.** Not both, not neither, not twice.

```
propose_fix(env, new_content, title, rationale, pr_body)
decline_fix(reason)
```

## Protected fields

`propose_fix` rejects any change to these:

- `resources`
- `namespace`
- the `images` entries' `name` and `newName`
- the `part-of` label patch

Only `newTag` may change under `images`. Do not add or remove `images` entries. If the rejection names a field, fix the edit and call again, or decline.

## When to decline

Call `decline_fix(reason)` when no overlay edit would help, or the edit would need a protected field. Give a reason a human can act on: what is wrong and who needs to do what. `references/direction.md` lists which situations justify an edit and which do not.

Never invent a version. Take versions only from the fact rows and the overlay.
