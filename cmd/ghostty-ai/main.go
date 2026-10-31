// ghostty-ai is the AI backend daemon for the Ghostty AI panel. It
// listens on a unix socket and runs one chat session per terminal tab.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Teejer/ghostty-ai/internal/config"
	"github.com/Teejer/ghostty-ai/internal/llm"
	"github.com/Teejer/ghostty-ai/internal/server"
)

func main() {
	socket := flag.String("socket", server.SocketPath(), "daemon socket path")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ghostty-ai:", err)
		os.Exit(1)
	}

	// `ghostty-ai ask "..."` is a one-shot smoke test against the
	// configured provider, useful without any terminal attached.
	if flag.Arg(0) == "ask" {
		if err := oneShot(cfg, flag.Arg(1)); err != nil {
			fmt.Fprintln(os.Stderr, "ghostty-ai:", err)
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	if _, _, ok := cfg.Default(); !ok {
		slog.Warn("no providers configured", "path", config.Path())
	}
	if err := server.Serve(ctx, *socket, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "ghostty-ai:", err)
		os.Exit(1)
	}
}

func oneShot(cfg *config.Config, prompt string) error {
	if prompt == "" {
		return fmt.Errorf("usage: ghostty-ai ask \"prompt\"")
	}
	name, prov, ok := cfg.Default()
	if !ok {
		return fmt.Errorf("no providers configured in %s", config.Path())
	}
	fmt.Fprintf(os.Stderr, "provider=%s model=%s\n", name, prov.Model)
	client := llm.New(prov.BaseURL, prov.APIKey, prov.Model)
	return client.ChatStream(context.Background(), []llm.Message{
		{Role: "user", Content: prompt},
	}, func(d string) { fmt.Print(d) }, nil)
}
