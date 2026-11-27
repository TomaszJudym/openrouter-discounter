// admech.go holds the admech-code role presets: six per-role specs alongside
// the admech-code worker preset (rank.Code, defined in preset.go, unchanged).
package preset

import (
	"strings"

	_ "embed"

	"github.com/tomaszjudym/openrouter-discounter/internal/rank"
)

// The admech-code role sectors. The contracted rank.Adm* constants carry
// these exact string values and are added to internal/rank by the
// integration step; until they land in this worktree these locals key the
// Slugs and specs entries. rank.Sector is a string type, so the map keys
// stay identical once the rank constants exist.
const (
	admechOracle     rank.Sector = "admech-code-oracle"
	admechReviewer   rank.Sector = "admech-code-reviewer"
	admechResearcher rank.Sector = "admech-code-researcher"
	admechAuditor    rank.Sector = "admech-code-auditor"
	admechDelegate   rank.Sector = "admech-code-delegate"
	admechScout      rank.Sector = "admech-code-scout"
)

//go:embed oracle.txt
var oraclePromptFile string

//go:embed reviewer.txt
var reviewerPromptFile string

//go:embed researcher.txt
var researcherPromptFile string

//go:embed auditor.txt
var auditorPromptFile string

//go:embed delegate.txt
var delegatePromptFile string

//go:embed scout.txt
var scoutPromptFile string

// Role system prompts, one per admech-code preset.
var (
	oraclePrompt     = strings.TrimSpace(oraclePromptFile)
	reviewerPrompt   = strings.TrimSpace(reviewerPromptFile)
	researcherPrompt = strings.TrimSpace(researcherPromptFile)
	auditorPrompt    = strings.TrimSpace(auditorPromptFile)
	delegatePrompt   = strings.TrimSpace(delegatePromptFile)
	scoutPrompt      = strings.TrimSpace(scoutPromptFile)
)

// admechSpec is the spec style shared by the admech-code role presets: a
// role system prompt and a per-role reasoning effort, like the Hermes role
// presets, plus the admech family's verbosity and no sampling params (the
// style of admech-code, the worker preset these roles extend).
func admechSpec(prompt, effort string) presetSpec {
	return presetSpec{systemPrompt: prompt, effort: effort}
}
