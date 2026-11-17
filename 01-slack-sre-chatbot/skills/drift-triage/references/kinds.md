# Kinds of drift

A fact row says what disagrees. It does not say what that means. You decide the kind.

Row fields: `GitVersion`, `RunningVersion`, `SyncStatus`, `Health`, `LastMessage`, `InGit`, `InCluster`, `PrevEnv`, `PrevGitVersion`.
Flags: `VersionMismatch`, `OnlyInGit`, `OnlyInCluster`, `NotSynced`, `NotHealthy`, `DiffersFromPrevEnv`.
`Note()` restates the values behind the flags, joined with `; `. Long sync messages are cut at about 300 characters.

Environments are ordered least to most production-like. Production is the last one, usually `prod`. A row's `PrevEnv` is the nearest earlier environment that declares the same service.

## Recognise the kind

Check in this order. The first match is the kind. A louder kind subsumes a quieter one on the same row.

| Kind | Flags | Note looks like |
| --- | --- | --- |
| **missing** | `OnlyInGit` | `declared in git, not in the cluster` |
| **undeclared** | `OnlyInCluster` | `running v1.2.0, not declared in git` |
| **failed sync** | `VersionMismatch` and `NotSynced`, with `SyncStatus` OutOfSync and a non-empty `LastMessage` | `git v1.3.0, running v1.2.0; sync OutOfSync: ImagePullBackOff` |
| **version skew** | `VersionMismatch`, and no sync error message | `git v1.3.0, running v1.2.0` |
| **unhealthy** | versions match, `Health` is `Degraded` or `Missing` | `health Degraded` |
| **promotion gap** | `DiffersFromPrevEnv`, and no louder kind above matched | `git v1.1.0, stage git v1.2.0` |

### Missing
Declared in git, absent from the cluster. Nothing is running. Look at whether the Argo CD application exists and what its sync message says.

### Undeclared
Running in the cluster, absent from git. Someone ran `kubectl` by hand. Git does not know about it, so no overlay edit can fix it.

### Failed sync
Git is ahead, Argo CD tried to land the git version and failed. The rollout was attempted and lost. This is a deploy failure, not a forgotten promotion. The sync message is the evidence: `ImagePullBackOff` means the tag does not exist in the registry, `ComparisonError` means Argo CD cannot read the repo.

### Version skew
Git and the cluster disagree on the version and Argo CD reports no sync error. The sync probably never started: a paused application, a missing image, or auto-sync turned off. A refresh (yours with `refresh_app`, or the one the first message says was just done) rules out Argo CD simply not having looked at the repo yet.

### Unhealthy
Right version, unhealthy workload. A version change will not fix it. Read the health and message to find the real problem.

### Promotion gap
Every environment matches its own overlay, but the overlays disagree with each other. Example: stage runs `v1.2.0`, the prod overlay still pins `v1.1.0`. Nothing is broken. The later environment was never promoted. This is the common case, and the one a pull request genuinely fixes.

- `PrevEnv` is the environment that leads, `PrevGitVersion` its version. The row you triage is the lagging one.
- The facts only say the versions differ. Read both versions to confirm which side is ahead.
- Only call it a gap when the row has no louder finding. "Prod failed to deploy" subsumes "prod is behind".

## Reading sync and health

`NotSynced` and `NotHealthy` fire on anything that is not `Synced` or `Healthy`. They are not findings by themselves.

| What you see | Meaning |
| --- | --- |
| `Synced` / `Progressing` | Pods still starting. Wait, then re-check with `refresh_app`. Not a finding yet. |
| `Synced` / `Degraded` with `ImagePullBackOff` | The rendered image tag does not exist in the registry. |
| `Unknown` / `ComparisonError` | Argo CD cannot read the repo. The path or branch is wrong, or the commit was never pushed. |
| `OutOfSync` with matching versions | Git and the cluster disagree on something other than the tag. Read the message. |

## Severity

Three levels: high, medium, low.

| Kind | Severity |
| --- | --- |
| missing | high, in every environment |
| failed sync | high |
| version skew | medium, high in production |
| unhealthy | medium, high in production |
| undeclared | medium |
| promotion gap | low, medium when the lagging environment is production |

Judgement rules:

- A failed sync outranks plain skew. It tells you the rollout was attempted and lost, rather than never started. It is the one worth paging about.
- Production is not an absolute override. It bumps severity: skew and unhealthy go to high, a promotion gap goes to medium. A stage failed sync (high) still outranks a prod promotion gap (medium).
- A service declared in git and missing from the cluster is always high.
- A louder finding subsumes a promotion gap on the same service and environment.

When ranking several rows, order by severity, then production before stage before test, then service name. Triage the first one unless asked about a specific service.
