package axiapi

import (
	"encoding/base64"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The gate push-option vocabulary: how a launcher carries run parameters to the
// daemon through the trigger push. Both AXI launchers - the `no-mistakes axi`
// command and the MCP gateway - format options here, so a new option or a
// changed encoding is taught once. The receive hook's parsers
// (internal/cli/daemon_cmd.go) read the same prefixes.
//
// Opaque values are base64-encoded because push options are line-oriented and
// an intent routinely spans lines.
const (
	IntentPushOptionPrefix               = "no-mistakes.intent="
	LaunchNoncePushOptionPrefix          = "no-mistakes.launch-nonce="
	ValidationGenerationPushOptionPrefix = "no-mistakes.validation-generation="
	PRBaseBranchPushOptionPrefix         = "no-mistakes.pr-base-branch="
	SkipPushOptionPrefix                 = "no-mistakes.skip="
)

// FormatIntentPushOption encodes intent as a single push option, or returns ""
// when there is no intent to carry.
func FormatIntentPushOption(intent string) string {
	if strings.TrimSpace(intent) == "" {
		return ""
	}
	return IntentPushOptionPrefix + base64.StdEncoding.EncodeToString([]byte(intent))
}

func FormatLaunchNoncePushOption(nonce string) string {
	return formatOpaquePushOption(LaunchNoncePushOptionPrefix, nonce)
}

func FormatValidationGenerationPushOption(generation string) string {
	return formatOpaquePushOption(ValidationGenerationPushOptionPrefix, generation)
}

func formatOpaquePushOption(prefix, value string) string {
	if value == "" {
		return ""
	}
	return prefix + base64.StdEncoding.EncodeToString([]byte(value))
}

// FormatPRBaseBranchPushOption encodes a per-run PR base branch as a push
// option, or returns "" when unset.
func FormatPRBaseBranchPushOption(branch string) string {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ""
	}
	return PRBaseBranchPushOptionPrefix + branch
}

// FormatSkipPushOptions encodes the requested step skips as one push option.
func FormatSkipPushOptions(steps []types.StepName) []string {
	if len(steps) == 0 {
		return nil
	}
	seen := make(map[types.StepName]bool, len(steps))
	parts := make([]string, 0, len(steps))
	for _, step := range steps {
		if seen[step] {
			continue
		}
		seen[step] = true
		parts = append(parts, string(step))
	}
	return []string{SkipPushOptionPrefix + strings.Join(parts, ",")}
}
