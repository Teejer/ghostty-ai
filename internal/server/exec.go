// Command proposal and execution policy for AI-proposed shell commands.
//
// The protocol is a fenced-block convention: the model puts exactly one
// command in a ```execute block; the daemon proposes it to the panel,
// which is the real security boundary (nothing runs without the pty's
// own process approving it).
package server

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
)

// execResult is the panel's verdict plus captured terminal output for
// one command proposal.
type execResult struct {
	Decision string // approve | deny
	Status   string // done | timeout | interactive | failed | lost-connection
	Output   string
}

// maxExecRounds caps command follow-up turns per user message so a
// model cannot loop on proposals forever.
const maxExecRounds = 5

// extractCommands returns commands proposed in fenced blocks whose
// info string is execute, run, or command.
func extractCommands(text string) []string {
	var cmds []string
	var cur []string
	inBlock := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inBlock {
			if strings.HasPrefix(trimmed, "```") {
				info := strings.ToLower(strings.TrimSpace(
					strings.TrimPrefix(trimmed, "```")))
				if info == "execute" || info == "run" || info == "command" {
					inBlock = true
					cur = nil
				}
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") {
			cmd := strings.TrimSpace(strings.Join(cur, "\n"))
			if cmd != "" {
				cmds = append(cmds, cmd)
			}
			inBlock = false
			continue
		}
		cur = append(cur, line)
	}
	return cmds
}

// dangerousPatterns never auto-run, even under auto_except_dangerous.
// The goal is catching accidents, not adversaries.
var dangerousPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\brm\s+(-\w*[rf]\w*|--recursive|--force)`),
	regexp.MustCompile(`\b(mkfs\w*|fdisk|parted|wipefs)\b`),
	regexp.MustCompile(`\bdd\b.*\bof=/dev/`),
	regexp.MustCompile(`>\s*/dev/(sd|nvme|hd|disk)`),
	regexp.MustCompile(`\b(shutdown|reboot|poweroff|halt)\b`),
	regexp.MustCompile(`\bsudo\b|\bsu\b`),
	regexp.MustCompile(`\bchmod\s+(-\w+\s+)?[0-7]*777\b`),
	regexp.MustCompile(`\bchown\s+-\w+\s+\S+\s+/`),
	regexp.MustCompile(`\b(curl|wget)\b.*\|\s*(sudo\s+)?(bash|sh|zsh)\b`),
	regexp.MustCompile(`\bgit\s+push\b.*(\s-f\b|--force)`),
	regexp.MustCompile(`\bgit\s+reset\s+--hard\b`),
	regexp.MustCompile(`>\s*/etc/|\btee\s+/etc/`),
	regexp.MustCompile(`\b(iptables|nft|ufw)\b`),
	regexp.MustCompile(`:\(\)\s*\{.*\|.*&.*\}`),
	regexp.MustCompile(`\bxargs\b.*\brm\b`),
}

func isDangerous(cmd string) bool {
	for _, re := range dangerousPatterns {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}

func newExecID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "exec-unknown"
	}
	return "exec-" + hex.EncodeToString(b[:])
}
