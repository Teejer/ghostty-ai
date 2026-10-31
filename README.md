# ghostty-ai

The AI backend daemon for the [ghostty-ai fork](https://github.com/Teejer/ghostty)
of Ghostty. One daemon serves all Ghostty windows; each terminal tab gets
its own isolated AI session that keeps running even when the tab is not
focused.

## Build

```sh
go build -o bin/ghostty-ai ./cmd/ghostty-ai
```

## Run

```sh
./bin/ghostty-ai                    # serve on $XDG_RUNTIME_DIR/ghostty-ai/daemon.sock
./bin/ghostty-ai ask "hello"        # one-shot smoke test against the default provider
```

## Config

`~/.config/ghostty-ai/config.json`:

```json
{
  "default_provider": "mlx",
  "providers": {
    "mlx": {
      "base_url": "http://192.168.8.70:11234/v1",
      "model": "ddalcu/Qwen3.8-Flash-Next-MLX-Serve-mixed-4-8bit"
    },
    "ollama": {
      "base_url": "http://192.168.8.50:11434/v1",
      "model": "qwen3-coder:30b"
    }
  }
}
```

Any OpenAI-compatible `chat/completions` endpoint works (Ollama, MLX
servers, llama.cpp, vLLM, OpenRouter, OpenAI, ...). Anthropic-native
endpoints are planned (`"anthropic": true`).

## Socket protocol (v0, line-delimited JSON)

Panels connect to the unix socket and speak JSONL.

Client → daemon:

| type   | fields                | meaning                                  |
|--------|-----------------------|------------------------------------------|
| hello  | tab_id, title         | attach connection to a tab's session     |
| chat   | text, screen          | user message + current screen as context |
| cancel |                       | interrupt the current generation         |
| bye    |                       | tab closed; drop the session             |

Daemon → panel:

| type   | fields                        | meaning                       |
|--------|-------------------------------|-------------------------------|
| ready  | provider, model, message?     | hello accepted                |
| status | state: thinking\|generating   | turn state changes            |
| delta  | text                          | streamed assistant text       |
| done   |                               | turn finished                 |
| error  | message                       | provider or protocol error    |

Sessions are keyed by `tab_id` and survive panel reconnects; generation
runs in a goroutine, so every tab's AI works in the background whether
or not its tab is focused.

## Roadmap

- [x] MVP: per-tab sessions, streaming chat, screen context
- [ ] Anthropic provider
- [ ] Tools: read_scrollback, inject (with approval flow in the terminal)
- [ ] History replay on reconnect
- [ ] systemd user service unit
