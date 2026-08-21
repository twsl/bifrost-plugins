# Bifrost Subscription Plugins

Dynamically-loaded [Bifrost](https://getbifrost.ai) Go plugins that intercept and
enrich LLM requests on behalf of a subscription account:

- **`codex-sub-plugin`** — intercepts the OpenAI **Codex** provider and serves
  responses through the subscription's `/codex/responses` endpoint.
- **`claude-sub-plugin`** — intercepts the **Anthropic** provider and serves
  responses through the subscription's `/v1/messages` endpoint.

Both plugins are built as Go **shared objects** (`.so` files) and loaded at
runtime by the Bifrost gateway. No restart is required to load, reload, or unload
them.

---

## Table of Contents

- [Prerequisites](#prerequisites)
- [Project layout](#project-layout)
- [Building the plugins](#building-the-plugins)
- [Important platform requirements](#important-platform-requirements)
- [Installing into Bifrost](#installing-into-bifrost)
- [Plugin interface](#plugin-interface)
- [Release binaries](#release-binaries)
- [Documentation](#documentation)

---

## Prerequisites

| Requirement | Version |
| --- | --- |
| Go toolchain | **1.26.x** (matches `go.mod`; see [Important platform requirements](#important-platform-requirements)) |
| Bifrost target | **v1.6.x** (both plugins depend on `github.com/maximhq/bifrost/core v1.6.3`) |
| Operating system | Linux **or** macOS (Darwin) only |

Bifrost plugins are built with the standard library `plugin` package, so you must
have a working Go toolchain for the platform you intend to run Bifrost on.

---

## Project layout

```
bifrost/
├── claude-sub-plugin/     # Anthropic (Claude) subscription plugin
│   ├── claude.go          # plugin implementation
│   ├── login/             # credentials/keychain helpers
│   └── cmd/claude-sub-login/  # standalone login CLI
├── codex-sub-plugin/      # OpenAI Codex subscription plugin
│   ├── codex.go           # plugin implementation
│   ├── login/             # OAuth device/pkce helpers
│   └── cmd/codex-sub-login/   # standalone login CLI
├── Makefile               # build helper (per plugin directory)
└── .github/workflows/      # CI/CD (see Release binaries)
```

Each plugin is an independent Go module. Build them from **inside** the plugin
directory.

---

## Building the plugins

> Also see https://docs.getbifrost.ai/plugins/getting-started

From the plugin directory you want to build, run:

```bash
cd claude-sub-plugin   # or codex-sub-plugin
make build
```

This compiles the plugin into an `.so` shared object:

```
claude-sub-plugin/build/claude-sub.so
codex-sub-plugin/build/codex-sub.so
```

It is equivalent to:

```bash
go build -buildmode=plugin -o build/<name>.so .
```

### Available Make targets

Run `make help` inside a plugin directory for a full list. The important ones are:

| Target | Description |
| --- | --- |
| `make build` | Build the `.so` plugin for the current platform/arch |
| `make build-login` | Build the standalone login CLI (`bin/claude-sub-login` / `bin/codex-sub-login`) |
| `make build-all` | Build both the plugin **and** the login CLI |
| `make install` | Build and copy the plugin into `~/.bifrost/plugins/` |
| `make install-login` | Build and copy the login CLI into `~/.bifrost/bin/` |
| `make install-all` | Install both the plugin and the login CLI |
| `make clean` | Remove build artifacts (`build/`) |

---

## Important platform requirements

Bifrost Go plugins have hard constraints you should know about before building:

- **No cross-compilation.** Plugins must be compiled **on the target platform**
  (Linux or macOS) using the `-buildmode=plugin` flag. They cannot be
  cross-compiled for another OS.
- **Architecture must match.** The plugin and the Bifrost gateway must use the
  **same architecture** (`amd64`, `arm64`).
- **Same Go version as Bifrost.** The plugin must be built with the **same Go
  version** as the Bifrost binary that will load it. Using a different Go
  version will prevent the `.so` from loading.

> **Practical consequence:** to build for Linux AMD64, build on a Linux AMD64
> machine with a matching Go toolchain. Build macOS plugins on macOS.

---

## Installing into Bifrost

After building, copy the `.so` into Bifrost's plugins directory (or use the Make
install targets):

```bash
cd claude-sub-plugin
make install
```

This places the plugin at `~/.bifrost/plugins/claude-sub.so`.

Enable the plugin in your Bifrost `config.json` and restart or dynamically load
it through the gateway's plugin management. See the
[plugins documentation](https://docs.getbifrost.ai/plugins/getting-started) for
configuration details.

---

## Plugin interface

The exported `.so` functions (Bifrost **v1.4.x+** interface, matching v1.6.x):

- `Init(config any) error` — initialize the plugin with configuration
- `GetName() string` — return the plugin name
- `PreRequestHook()` — once-per-request routing phase (provider/model/fallbacks)
- `PreLLMHook()` — intercept requests before they reach providers
- `PostLLMHook()` — process responses after provider calls
- `Cleanup() error` — clean up resources on shutdown

For the full hook reference and lifecycle ordering, see the
[plugin architecture docs](https://docs.getbifrost.ai/plugins/getting-started).

---

## Release binaries

Prebuilt `.so` files and login CLIs are attached to every
[GitHub Release](https://github.com/twsl/bifrost/releases). Assets are named
with the target OS and architecture so you can grab the one matching your Bifrost
deployment:

```
claude-sub_<os>_<arch>.so     # e.g. claude-sub_linux_amd64.so
codex-sub_<os>_<arch>.so      # e.g. codex-sub_darwin_arm64.so
claude-sub-login_<os>_<arch>  # login CLI
codex-sub-login_<os>_<arch>   # login CLI
```

The release pipeline is defined in
`.github/workflows/release.yml`; it builds each platform/architecture on a
native runner and attaches the artifacts to the release.

---

## Documentation

- [Bifrost plugins — getting started](https://docs.getbifrost.ai/plugins/getting-started)
- [Writing Go plugins](https://docs.getbifrost.ai/plugins/writing-go-plugin)
