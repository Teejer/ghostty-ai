// Package server implements the ghostty-ai daemon: a unix socket
// speaking line-delimited JSON, one AI session per terminal tab.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Teejer/ghostty-ai/internal/config"
	"github.com/Teejer/ghostty-ai/internal/llm"
)

// SocketPath returns the default daemon socket path.
func SocketPath() string {
	if x := os.Getenv("GHOSTTY_AI_SOCKET"); x != "" {
		return x
	}
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		rt = filepath.Join(os.TempDir(),
			fmt.Sprintf("ghostty-ai-%d", os.Getuid()))
	}
	return filepath.Join(rt, "ghostty-ai", "daemon.sock")
}

// Serve listens on path until ctx is canceled.
func Serve(ctx context.Context, path string, cfg *config.Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("socket dir: %w", err)
	}
	// Remove a stale socket from a previous crashed run.
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		if _, err := net.Dial("unix", path); err != nil {
			os.Remove(path)
		} else {
			return fmt.Errorf("%s is already served by a live daemon", path)
		}
	}

	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return fmt.Errorf("listen %s: %w", path, err)
	}
	os.Chmod(path, 0o600)
	slog.Info("listening", "socket", path)

	go func() {
		<-ctx.Done()
		ln.Close()
		os.Remove(path)
	}()

	s := &server{cfg: cfg, sessions: map[string]*session{}}
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Error("accept", "err", err)
			continue
		}
		go s.handleConn(conn)
	}
}

// ----------------------------------------------------------------------
// Wire protocol (line-delimited JSON)
// ----------------------------------------------------------------------

// inbound messages
type inbound struct {
	Type     string `json:"type"` // hello | chat | cancel | set_provider | bye
	TabID    string `json:"tab_id"`
	Title    string `json:"title"`
	Text     string `json:"text"`
	Screen   string `json:"screen"`
	Provider string `json:"provider"`
}

// outbound messages (one struct, sparse fields)
type outbound struct {
	Type      string   `json:"type"` // ready | status | delta | done | error
	State     string   `json:"state,omitempty"`
	Text      string   `json:"text,omitempty"`
	Message   string   `json:"message,omitempty"`
	Provider  string   `json:"provider,omitempty"`
	Model     string   `json:"model,omitempty"`
	Providers []string `json:"providers,omitempty"`
	Default   string   `json:"default,omitempty"`
}

// ----------------------------------------------------------------------

type server struct {
	cfg *config.Config

	mu       sync.Mutex
	sessions map[string]*session
}

// session is the AI state for one terminal tab. It outlives its socket
// connection so a panel reconnect keeps its history, and it keeps
// running while the tab is not focused because generation happens in a
// goroutine independent of any UI.
type session struct {
	tabID   string
	history []llm.Message
	owner   *server

	// Provider chosen for this tab; empty means the config default.
	provider string

	mu         sync.Mutex // guards conn, generating, cancel
	conn       net.Conn
	sendMu     sync.Mutex // serializes writes to conn
	generating bool
	cancel     context.CancelFunc
}

// write sends one message to the attached panel, if any. A detached
// session silently drops events; the panel catches up via history on
// reconnect (later step).
func (s *session) write(msg outbound) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.conn == nil {
		return
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	s.conn.Write(append(b, '\n'))
}

func (s *server) getSession(tabID string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[tabID]
	if !ok {
		sess = &session{tabID: tabID, owner: s, history: []llm.Message{
			{Role: "system", Content: systemPrompt},
		}}
		s.sessions[tabID] = sess
	}
	return sess
}

const systemPrompt = `You are the AI panel embedded in the Ghostty terminal, attached to exactly one terminal tab. ` +
	`The user's messages include the current visible screen of that tab as context. ` +
	`Be concise and practical. Prefer terminal-friendly formatting. When you suggest a command, put it in a fenced code block; never assume it has been run.`

func (s *server) handleConn(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var sess *session
	for sc.Scan() {
		var in inbound
		if err := json.Unmarshal(sc.Bytes(), &in); err != nil {
			writeOne(conn, outbound{Type: "error", Message: "bad json"})
			continue
		}
		switch in.Type {
		case "hello":
			if in.TabID == "" {
				writeOne(conn, outbound{Type: "error", Message: "hello needs tab_id"})
				continue
			}
			if sess != nil {
				sess.detach(conn)
			}
			sess = s.getSession(in.TabID)
			sess.attach(conn)
			slog.Info("hello", "tab", in.TabID, "title", in.Title)
			msg := outbound{Type: "ready", Providers: s.cfg.ProviderNames()}
			defName, defProv, ok := s.cfg.Default()
			if ok {
				msg.Default = defName
			} else {
				msg.Message = "no providers configured in " + config.Path()
			}
			name, prov, _ := s.resolveProvider(sess.getProvider())
			if name == "" {
				name, prov = defName, defProv
			}
			msg.Provider, msg.Model = name, prov.Model
			writeOne(conn, msg)
		case "chat":
			if sess == nil {
				writeOne(conn, outbound{Type: "error", Message: "send hello first"})
				continue
			}
			slog.Info("chat", "tab", sess.tabID, "prompt_bytes", len(in.Text), "screen_bytes", len(in.Screen))
			s.startTurn(sess, in.Text, in.Screen)
		case "set_provider":
			if sess == nil {
				writeOne(conn, outbound{Type: "error", Message: "send hello first"})
				continue
			}
			if _, ok := s.cfg.Providers[in.Provider]; !ok {
				writeOne(conn, outbound{Type: "error",
					Message: "unknown provider " + in.Provider})
				continue
			}
			sess.setProvider(in.Provider)
			slog.Info("set_provider", "tab", sess.tabID, "provider", in.Provider)
			writeOne(conn, outbound{Type: "ready",
				Provider:  in.Provider,
				Model:     s.cfg.Providers[in.Provider].Model,
				Providers: s.cfg.ProviderNames(),
			})
		case "cancel":
			if sess != nil {
				sess.cancelTurn()
			}
		case "bye":
			if sess != nil {
				s.mu.Lock()
				delete(s.sessions, sess.tabID)
				s.mu.Unlock()
				sess.detach(conn)
			}
			return
		default:
			writeOne(conn, outbound{Type: "error", Message: "unknown type " + in.Type})
		}
	}
	if sess != nil {
		sess.detach(conn)
	}
}

func writeOne(w io.Writer, msg outbound) {
	b, _ := json.Marshal(msg)
	w.Write(append(b, '\n'))
}

func (s *session) getProvider() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.provider
}

func (s *session) setProvider(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.provider = name
}

// resolveProvider looks up a provider by name, falling back to the
// config default when name is empty or unknown.
func (s *server) resolveProvider(name string) (string, config.Provider, bool) {
	if name != "" {
		if p, ok := s.cfg.Providers[name]; ok {
			return name, p, true
		}
	}
	return s.cfg.Default()
}

func (s *session) attach(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conn = conn
}

func (s *session) detach(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == conn {
		s.conn = nil
		// If the panel went away without a bye (crash, kill), reap the
		// session after a grace period so reconnects still work but
		// abandoned sessions do not leak.
		time.AfterFunc(10*time.Minute, s.gc)
	}
}

// gc drops the session from the server unless a panel reattached or a
// turn is still running.
func (s *session) gc() {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil || s.generating {
		return
	}
	if s.owner.sessions[s.tabID] == s {
		delete(s.owner.sessions, s.tabID)
		slog.Info("reaped abandoned session", "tab", s.tabID)
	}
}

func (s *session) cancelTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
}

// startTurn runs one model turn in the background so the tab's AI
// keeps working regardless of which tab is focused in the terminal.
func (s *server) startTurn(sess *session, text, screen string) {
	sess.mu.Lock()
	if sess.generating {
		sess.mu.Unlock()
		sess.cancelTurn()
	}
	sess.mu.Unlock()

	user := text
	if screen != "" {
		screen = tail(screen, s.cfg.MaxScreenBytes)
		user += "\n\nCurrent visible screen of this tab:\n```\n" + screen + "\n```"
	}

	sess.mu.Lock()
	if sess.generating {
		// Previous turn still winding down; queue is out of scope for
		// the MVP, reject politely.
		sess.mu.Unlock()
		sess.write(outbound{Type: "error", Message: "already generating; cancel first"})
		return
	}
	sess.history = append(sess.history, llm.Message{Role: "user", Content: user})
	msgs := append([]llm.Message(nil), sess.history...)
	ctx, cancel := context.WithCancel(context.Background())
	sess.generating, sess.cancel = true, cancel
	sess.mu.Unlock()

	go func() {
		defer func() {
			sess.mu.Lock()
			sess.generating, sess.cancel = false, nil
			sess.mu.Unlock()
		}()

		provName, prov, ok := s.resolveProvider(sess.getProvider())
		if !ok {
			sess.write(outbound{Type: "error",
				Message: "no providers configured in " + config.Path()})
			return
		}

		sess.write(outbound{Type: "status", State: "thinking"})
		client := llm.New(prov.BaseURL, prov.APIKey, prov.Model)

		var full []byte
		err := client.ChatStream(ctx, msgs, func(delta string) {
			if len(full) == 0 {
				sess.write(outbound{Type: "status", State: "generating"})
			}
			full = append(full, delta...)
			sess.write(outbound{Type: "delta", Text: delta})
		})

		sess.mu.Lock()
		if len(full) > 0 {
			sess.history = append(sess.history,
				llm.Message{Role: "assistant", Content: string(full)})
		}
		sess.mu.Unlock()

		if err != nil {
			slog.Error("provider turn failed", "tab", sess.tabID,
				"provider", provName, "err", err)
			sess.write(outbound{Type: "error", Message: err.Error()})
		}
		sess.write(outbound{Type: "done"})
	}()
}

// tail returns the last n bytes of s, aligned to a byte boundary.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…(older screen content truncated)…\n" + s[len(s)-n:]
}
