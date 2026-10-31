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

// fenceFilter hides execute/run/command fenced blocks from the *display*
// stream while the turn streams: the panel shows a clean proposal line
// with Run/Deny buttons instead of raw markdown. History keeps the
// unfiltered text so the model still sees its own proposal.
//
// It is a byte-level state machine over deltas: normal text passes
// through; a line that might open a fence is buffered until the newline
// decides it; inside a block everything is dropped until the closing
// fence. Output accumulates in an out buffer that flushes at each delta
// boundary, so multi-byte UTF-8 split across deltas survives (JSON
// would corrupt a mid-rune write).
type fenceFilter struct {
	emit  func(string)
	out   []byte
	probe []byte
	mode  int  // 0 normal, 1 probing fence open, 2 inside block
	atEOL bool // at the start of a line
}

func newFenceFilter(emit func(string)) *fenceFilter {
	return &fenceFilter{emit: emit, atEOL: true}
}

var fenceInfos = map[string]bool{"execute": true, "run": true, "command": true}

func isExecuteFenceLine(line string) bool {
	return strings.HasPrefix(line, "```") &&
		fenceInfos[strings.ToLower(strings.TrimSpace(line[3:]))]
}

func (f *fenceFilter) Feed(s string) {
	defer f.flush()
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch f.mode {
		case 0:
			if f.atEOL && c == '`' {
				f.probe = append(f.probe[:0], c)
				f.mode = 1
				continue
			}
			f.atEOL = c == '\n'
			f.out = append(f.out, c)
		case 1:
			f.probe = append(f.probe, c)
			if len(f.probe) < 3 && c != '`' {
				// Not a fence after all; the bytes were ordinary text.
				f.out = append(f.out, f.probe...)
				f.probe = f.probe[:0]
				f.mode = 0
				f.atEOL = false
			} else if c == '\n' {
				line := string(f.probe[:len(f.probe)-1])
				f.probe = f.probe[:0]
				if isExecuteFenceLine(line) {
					f.mode = 2
				} else {
					f.out = append(f.out, f.probe...)
					f.out = append(f.out, line...)
					f.out = append(f.out, '\n')
					f.mode = 0
				}
				f.atEOL = true
			}
		case 2:
			f.probe = append(f.probe, c)
			if c == '\n' {
				if strings.TrimSpace(string(f.probe)) == "```" {
					f.mode = 0
				}
				f.probe = f.probe[:0]
				f.atEOL = true
			}
		}
	}
}

// Flush releases any buffered bytes at end of stream (e.g. a probe line
// that never got its newline) and resets the machine.
func (f *fenceFilter) flush() {
	if len(f.out) > 0 {
		f.emit(string(f.out))
		f.out = f.out[:0]
	}
}

func (f *fenceFilter) Flush() {
	if f.mode == 1 {
		f.out = append(f.out, f.probe...)
		f.probe = f.probe[:0]
	}
	f.mode = 0
	f.flush()
}

func newExecID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "exec-unknown"
	}
	return "exec-" + hex.EncodeToString(b[:])
}
