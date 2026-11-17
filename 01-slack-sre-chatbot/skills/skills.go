// Package skills embeds the agent skills that carry the drift knowledge: what each kind
// of disagreement between git and the cluster means, how bad it is, which way a fix
// goes, and how a root cause and a pull request are written. The agents load them
// through ADK's skilltoolset; none of this reasoning lives in Go.
package skills

import "embed"

// FS holds one directory per skill, each with a SKILL.md and a references/ folder.
//
//go:embed drift-triage gitops-fix
var FS embed.FS
