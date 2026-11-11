# go-ai-agent

English | [简体中文](README.zh-CN.md)

![Platform](https://img.shields.io/badge/Platform-Windows-blue) ![Go](https://img.shields.io/badge/Go-1.27+-00ADD8?logo=go&logoColor=white) ![Wails](https://img.shields.io/badge/Wails-v2-red?logo=wails&logoColor=white) ![Vue](https://img.shields.io/badge/Vue-3-4FC08D?logo=vuedotjs&logoColor=white) ![TypeScript](https://img.shields.io/badge/TypeScript-5.7-3178C6?logo=typescript&logoColor=white) ![Vite](https://img.shields.io/badge/Vite-6-646CFF?logo=vite&logoColor=white) ![SQLite](https://img.shields.io/badge/SQLite-pure--Go-003B57?logo=sqlite&logoColor=white) ![pnpm](https://img.shields.io/badge/pnpm-build-F69220?logo=pnpm&logoColor=white)

A pluggable LLM workflow desktop application. The Vue 3 frontend is embedded into a single binary (`harness.exe`) via `go:embed` and the Wails AssetServer — there is no HTTP service layer: the frontend calls Go directly through `window.go.main.App.*` bindings, and streaming chat responses are pushed as Wails events. The `mq` / `cron` / CLI subcommands are still available.

Built-in features (each one is a pure-config plugin):

| Feature | Description |
| --- | --- |
| AI Chat (`chat`) | Multi-turn chat with any configured provider; model list pulled from `GET /v1/models`; streaming by default; image upload for vision models |
| Payslip OCR (`payslip-ocr`) | Upload one or more payslip screenshots; a VLM extracts salary fields and merges them into one structured JSON |
| Book TOC (`book-toc`) | Extract a table of contents from book pages |
| Subtitle Translate (`subtitle-translate`) | Translate subtitle files |

## Architecture

```
main.go                  entry point (Wails requires the main package at module root);
                         delegates to internal/desktop.Run, which dispatches by first argument
internal/api/            application core: app.go is the Wails binding layer (App methods +
                         chat event sink); static.go exposes the embedded frontend assets
internal/config/         three-layer YAML configuration loading
internal/graph/          workflow orchestration on langgraphgo StateGraph;
                         nodes live in internal/graph/nodes with signature
                         func(ctx, State) (State, error)
internal/llm/            provider client factory (cached per provider+model), streaming
                         accumulator, stats instrumentation, wire dump
internal/stats/          SQLite persistence (modernc.org/sqlite, pure Go, no CGO)
internal/tmpl/           Go text/template rendering; templates in templates/
web/                     Vue 3 + TypeScript + Vite frontend; web/src/api.ts is the ONLY
                         backend call layer (Wails bindings + runtime events, no fetch)
config/                  three-layer YAML: config.yml / ai.yml / features/*.yml
```

Chat streaming protocol: Wails channel `chat:<reqID>` with payloads `{"event": meta|delta|done|error|collect, "data": ...}`. The frontend subscribes *before* calling `ChatSend` (events are not cached).

## Building

Requirements: Go 1.24+, Node.js + pnpm, Windows.

```powershell
.\build.ps1          # pnpm build -> copy web/dist -> internal/api/webdist -> go build -tags desktop,production
.\build.ps1 -Test    # also runs go test
```

Useful commands:

```bash
go build ./...       # backend compile check
go test ./...        # backend tests
cd web && pnpm install && pnpm build   # frontend build (includes vue-tsc -b type check)
wails dev            # frontend hot-reload development (uses wails.json at repo root)
```

`internal/api/webdist` is generated — never edit it by hand; `build.ps1` copies `web/dist` there so `go:embed` picks it up.

## Configuration

Three layers of YAML under `config/`:

- **`config.yml`** — application metadata, storage, caching, logging, and the feature registry (`features[]`). Left-menu order equals list order.
- **`ai.yml`** — AI endpoints only. Providers are a nested list (`providers[].name`) reusable by any step of any feature; unfilled fields fall back to the global defaults at the top.
- **`config/features/<id>.yml`** — one file per feature: inputs, workflow declaration, and per-step `provider` + `model` + prompts + parameters.

### Adding a new feature

1. Create `config/features/<id>.yml` (workflow + steps + prompts).
2. Register it in `config.yml` under `features[]`.

That's it — zero code changes, unless you need a new node/workflow type, which must be registered in `internal/graph/builder.go` (unknown workflow types are rejected with an error).

### Key conventions

- **Prompts live only in YAML** — nothing is hardcoded; a non-local step missing `system-prompt` fails startup validation. Changing prompts or models only touches `config/features/*.yml`, no recompile.
- Provider `base-url` in `ai.yml` **must include `/v1`** — langchaingo only appends `/chat/completions`.
- There is no global default model: every step must explicitly specify `provider` + `model`.
- API keys support `${ENV_VAR:default}` expansion; the model name can be overridden via `HARNESS_DEFAULT_MODEL`.
- Multimodal images are manually built as OpenAI `image_url` data URLs (bypassing langchaingo's private BinaryPart serialization); MIME is detected from magic bytes and images longer than 1600px are downscaled before sending.
- Uploads all go through `uploadSource` (filename + bytes): frontend File → base64 → binding method → temp directory, with size/count limits strictly validated against the feature's `input` declaration.
- Go methods exported to the frontend are all on `App` in `internal/api/app.go`; changing a signature requires updating the type declarations in `web/src/api.ts`.
- Built-in templates can be overridden by external `.tpl` files via `output.template-dir`, no recompile needed.

## Storage & Caching

- SQLite (pure Go driver, no CGO) stores LLM call statistics and async job records; retention is configurable (`retention-days`), with background pruning.
- LLM result cache: identical provider+model+messages+parameters hits the cache instead of re-calling the endpoint (best for deterministic OCR/extraction tasks); TTL and row cap configurable.
