# Fix direction

The direction of the edit depends on the finding. Getting it backwards is the easiest mistake in the whole fix.

Kinds are defined in the `drift-triage` skill's `references/kinds.md`.

## Promotion gap: move forward

Git itself encodes the lag. Stage runs `v1.2.0`, the prod overlay still pins `v1.1.0`, and each environment matches its own overlay. Nothing is broken.

- Edit the **lagging** environment's overlay.
- Move its tag **forward** to the version the leading environment already runs (`PrevGitVersion`).
- From = the lagging overlay's current tag. To = the leading environment's tag.
- Rationale: the leading environment already runs that version and is healthy, so this promotes the same version.

Check the leading environment is healthy before you promote. If it is not, decline.

## Version skew or failed sync: pin back

Git is already ahead and the cluster could not get there. Moving git forward again changes nothing, because it is already forward.

- Edit the overlay of the **affected** environment.
- Move its tag **backwards** to the version actually running (`RunningVersion`).
- From = the overlay's current tag (`GitVersion`). To = the running version.
- Effect: Argo CD stops retrying a release that does not work, and git tells the truth again.
- Rationale: git declares the new version, but the environment has been running the old one. Argo CD cannot land the new version, so git is pinned back to what is serving. Roll forward again once the new version is known good.

Rolling forward is a human decision. It needs the broken image fixed first. Never propose it.

## Unhealthy, missing, undeclared: usually decline

These are not version problems. A tag change does nothing for them.

| Situation | Edit or decline |
| --- | --- |
| Unhealthy, and the overlay itself holds the cause, for example `replicas` set to `0` or too low | Edit that one unprotected field. Quote the overlay line as evidence. |
| Unhealthy, and the cause is the image, the app or the cluster (crash loop, bad config, node pressure) | Decline. It is a workload problem, not a version problem. |
| Missing, and the overlay is plainly wrong in an unprotected field | Edit it. Rare. |
| Missing, and the application or its sync is the problem | Decline. |
| Undeclared | Decline. Someone must either declare it in git or remove it from the cluster. That is a human decision, and adding an `images` entry is not allowed. |

Test: could you point at the exact overlay line, in a field you are allowed to change, that causes the problem? If not, decline.

## Which version string to write

Copy the version exactly as the overlay writes it. The `v` prefix drifts in and out between a chart value and an image tag. Match the style of the line you are editing.
