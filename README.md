# prompt-admin

A native [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that injects one system prompt per model and manages those prompts from the CPA Management Center.

Matching is deterministic string scoring. The plugin does not call a model to choose a prompt. It replaces a standalone prompt admin service plus an edge injector: both the page and the injection run inside the CPA process.

Plugin id: `prompt-admin`. The management page is Chinese.

## Install

Release archives are published on the [Releases page](https://github.com/Tx13758627780/cpa-plugin-prompt-admin/releases) in the CPA Plugin Store layout: `prompt-admin_<version>_<goos>_<goarch>.zip` (only the library at the zip root) plus `checksums.txt` (`sha256sum` output). v0.1.1 ships `linux_amd64`, `linux_arm64`, and `windows_amd64`.

### Option A: manual drop-in (.so)

```bash
VER=0.1.1
curl -fLO https://github.com/Tx13758627780/cpa-plugin-prompt-admin/releases/download/v$VER/prompt-admin_${VER}_linux_amd64.zip
curl -fLO https://github.com/Tx13758627780/cpa-plugin-prompt-admin/releases/download/v$VER/checksums.txt
sha256sum --ignore-missing -c checksums.txt
mkdir -p plugins/linux/amd64
unzip -o prompt-admin_${VER}_linux_amd64.zip -d plugins/linux/amd64/
```

CPA discovers plugins at:

```text
plugins/linux/amd64/prompt-admin.so
```

The same layout applies to the other targets (`plugins/linux/arm64/prompt-admin.so`, `plugins/windows/amd64/prompt-admin.dll`, `plugins/darwin/<arch>/prompt-admin.dylib`). A copy directly under `plugins/` also works when its name matches the host OS and architecture. The plugin id is the file name without the extension. Restart CPA after the first install.

You can also build it yourself (`make build`, see [Build](#build)).

### Option B: Plugin Store

In the CPA Management Center open the plugin store and install **Prompt Admin** (`prompt-admin`) when it is listed in the store registry you use. CPA fetches the GitHub Release of `https://github.com/Tx13758627780/cpa-plugin-prompt-admin`, picks `prompt-admin_<version>_<goos>_<goarch>.zip` for the host, and verifies it against `checksums.txt`.

To list it in a registry (your own, or the official `router-for-me/CLIProxyAPI-Plugins-Store`), add an entry like:

```json
{
  "id": "prompt-admin",
  "name": "Prompt Admin",
  "description": "Per-model system prompts with Git prompt-repo auto-matching and a Management Center page.",
  "author": "Tx13758627780",
  "version": "0.1.1",
  "repository": "https://github.com/Tx13758627780/cpa-plugin-prompt-admin",
  "homepage": "https://github.com/Tx13758627780/cpa-plugin-prompt-admin",
  "license": "MIT",
  "tags": ["Interceptor"],
  "locales": {"zh-CN": {"description": "按模型注入系统提示词，自动匹配 Git 提示词仓库，并在管理中心提供管理页面。"}}
}
```

Only load a plugin binary you trust. It runs in-process.

### config.yaml

Enable plugins and add a config block. Unknown keys are rejected.

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    prompt-admin:
      enabled: true
      priority: 100
      data_dir: prompt-admin-data
      cpa_url: http://127.0.0.1:8317
      # cpa_config: config.yaml
      # api_key: ""          # optional advanced override for GET /v1/models only; NOT required
      # names_file: names.json
      # models: [gpt-4o]     # used only when /v1/models and cpa_config yield nothing
      watch_interval: 15s    # integer means seconds
      sync_interval: 1h      # 0 disables the git timer
      allow_file_urls: false
```

`plugins.enabled` is the global switch. A per-plugin `enabled: true` does not turn that switch on by itself.

## Management page

After installing, open the CPA Management Center (sign in there once), then choose **模型提示词** in the menu. You can use the page immediately — there is no second password and no management-key paste form on this page.

The page URL is:

```text
/v0/resource/plugins/prompt-admin/index.html
```

For a CPA listening on port 8317 that is `http://127.0.0.1:8317/v0/resource/plugins/prompt-admin/index.html`. Prefer opening it from the Management Center menu rather than pasting that URL alone: outside the Management Center session the page cannot edit (by design). It reuses the management key already stored by the Management Center on the same origin (`localStorage` / `cli-proxy-auth` / `managementKey`) and sends `Authorization: Bearer <management key>` on management calls. Changes go to authenticated routes under `/v0/management/plugins/prompt-admin/`. The HTML resource itself is not management-authenticated; do not expose the resource port to untrusted networks if that matters for your deployment.

| Action | Route |
| --- | --- |
| Snapshot | `GET /v0/management/plugins/prompt-admin/state` |
| File text | `GET /v0/management/plugins/prompt-admin/file?key=` |
| Sync now | `POST .../sync` |
| Add / remove / reorder / update a repo | `POST .../repos/add`, `remove`, `move`, `update` |
| Keep one conflict candidate | `POST .../choose` |
| Force text, file, none, or clear | `POST .../override` |
| Local prompt | `POST .../local` |
| Default prompt | `POST .../default` |

Send `Authorization: Bearer <management key>` on those routes.

### Add a repo and resolve a conflict

1. Open the page from the Management Center menu 「模型提示词」 (after signing in there).
2. Paste an `https` GitHub, GitLab, Codeberg, or raw file URL. `owner/repo` is accepted as GitHub. Optional branch, subdirectory, and a token for a private repo. Token is stored in `state.json` and is not returned to the browser.
3. Choose **添加并同步**. The plugin shallow-clones (or downloads a raw file) under `data_dir/repos/` and scores every scanned file against the live model list.
4. When one model has two or more different candidates, the page shows them side by side. Choose **保留这个** on the text to keep. Until you choose, the previous prompt stays in effect, or the default prompt when there is no previous one.
5. A remembered choice is reused until the chosen file disappears or a new candidate appears. A local prompt whose text matches a repo file is not a second candidate.

**立即同步** pulls every enabled repo. The model list is re-read about every `watch_interval` (default 15s) and when the page asks for state. Repo pulls also run on `sync_interval` (default 1 hour). `0` disables that timer. None of this needs a process restart: the injector reloads `prompts.json` when its modification time changes.

## Injection

On `request.intercept_before` the plugin prepends the resolved prompt as the first system message and leaves any client system message after it.

OpenAI chat completions:

```json
{"messages":[{"role":"system","content":"<configured prompt>"},{"role":"system","content":"<client system>"},{"role":"user","content":"hi"}]}
```

The same idea is applied to Responses `instructions`, Claude `system` (string or text blocks), Gemini `systemInstruction` / `system_instruction`, and Interactions `system_instruction`. Empty or missing prompts are skipped. Image generation, embeddings, and Responses compact requests are skipped. A request whose first system content is already the configured prompt is not modified again, so retries do not stack a second copy.

Nested CPA `host.model` callbacks (`metadata.source = plugin_host_model_callback`) are not injected. `host_callback_id` is set on ordinary intercept calls too, so the plugin does not treat that field as a nested call.

When a prompt is in effect, the downstream response gains `X-Prompt-Admin: injected`. A hidden alias rewrite also sets `X-Model-Alias: old -> new`. Those headers are applied from the response interceptor and from the stream header-init chunk. They are not copied onto the upstream request.

### Check a chat completion

After a prompt is saved for `demo-model`:

```bash
curl -sS -D - http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"demo-model","messages":[{"role":"user","content":"hi"}]}'
```

A successful injected call includes:

```text
X-Prompt-Admin: injected
```

Upstream request logs, or a temporary executor that echoes the body, show the gateway system message before the user message. `go test ./internal/...` covers the prepend, the retry, conflict retention, and the management JSON without a running CPA.

## Matching

Names are lowercased. A `:suffix`, a vendor prefix (`vendor/model`), and a trailing extension are removed before comparison. The compact form keeps only letters and digits. Scores at or above 40 are eligible. Only the best score is kept. Upstream ids are scored only at 70 or above, then lose 5 points.

| Score | Rule |
| --- | --- |
| 100 | File name equals the model, including after stripping words like `systemprompt` |
| 90 | File name contains the model (at least 4 compact characters) |
| 85 | Path contains the model |
| 80 | File name contains every model keyword |
| 70 | Path contains every model keyword |
| 50 | Same model family in the file name |
| 40 | Same model family in a directory |

Priority for the prompt that is actually injected:

1. Per-model override: literal text, one repo file, or "do not inject".
2. A single eligible candidate.
3. A remembered choice that still covers the current candidate set.
4. Otherwise the model is a conflict: keep the previous prompt, or the default prompt when there is no previous prompt.

An empty result injects nothing. Models that disappear from the live list keep their local prompt and overrides (`stored_only`) and are not injected. A `names_file` can record `old -> current` aliases. When the new name is live and the old name is not, stored prompts move to the new name. The injector rewrites a request that still uses the old name and then looks up the prompt for the new name, falling back to the old name's prompt.

Scanned extensions are `.md`, `.txt`, `.json`, `.yaml`, and `.yml`. `README`, `LICENSE`, dot directories, `node_modules`, and symlinks are skipped. JSON objects can contribute `system_prompt`, `systemPrompt`, `system`, `prompt`, `instructions`, `content`, or `text`.

## Files

All under `data_dir` (default `prompt-admin-data`, created `0700`):

| Path | Contents |
| --- | --- |
| `prompts.json` | Resolved model to prompt map. The injector reloads it on mtime change. |
| `state.json` | Repos (including tokens), overrides, local prompts, choices, default prompt. Mode `0600`. |
| `status.json` | Last match result, for the page. |
| `repos/<id>/` | Shallow git clone or raw download. |
| `backups/prompts.<timestamp>.json` | Previous `prompts.json`, 60 copies kept. |
| `sync.log` | Sync log, rotated around 1 MB, timestamps in UTC+8. |
| `.gitconfig-empty` | Empty git config so clones do not read the operator's git config. |

Git is invoked with a cleaned environment. A repo token is passed as an HTTP extra header, not as a process argument, and it is not written to the management API. `git` must be on `PATH` for git repos. A single raw HTTPS file does not need git. `file://` repos work only when `allow_file_urls` is true.

The model list prefers `GET {cpa_url}/v1/models` when a bearer is available: an optional `api_key` override, otherwise the first `api-keys` entry under `access` in `cpa_config`. `api_key` is **not required**. If `/v1/models` is unavailable, alias names in `cpa_config` are used. If those are empty, `models` in this plugin's config is used. When every source is empty, `prompts.json` is left unchanged and the page shows an error.

## Build

Requires Go 1.26, a C compiler, and cgo.

```bash
make test
make build    # dist/prompt-admin.so for linux/amd64, exports cliproxy_plugin_init
make release  # store zips + dist/checksums.txt (needs zig + MacOSX SDK for darwin)
make release-nodarwin CC_LINUX_ARM64="zig cc -target aarch64-linux-gnu.2.17" CC_WINDOWS_AMD64="zig cc -target x86_64-windows-gnu"
```

`make release` builds these archives and writes `dist/checksums.txt`:

- `prompt-admin_<version>_linux_amd64.zip` (host gcc, or `CC=gcc`)
- `prompt-admin_<version>_linux_arm64.zip` (`aarch64-linux-gnu-gcc`)
- `prompt-admin_<version>_windows_amd64.zip` (`x86_64-w64-mingw32-gcc`)
- `prompt-admin_<version>_darwin_amd64.zip`
- `prompt-admin_<version>_darwin_arm64.zip`

Darwin cgo needs `zig` plus a MacOSX SDK, because the Go runtime links `CoreFoundation`, `Security`, and `libresolv`. `make release` downloads [MacOSX 14.5.sdk](https://github.com/joseluisq/macosx-sdks/releases/download/14.5/MacOSX14.5.sdk.tar.xz) into `.cache/` when `MACOSX_SDK` is unset. That SDK is Apple's; accept the Xcode license terms before using it. Point `MACOSX_SDK` at an existing SDK tree to skip the download.

Version defaults to `0.1.1`. Override with `make release VERSION=0.1.2`.

## Behavior that is intentionally narrower than a standalone gateway

- Hidden alias rewrite runs only when `names_file` is set. CPA's own model aliases are a separate host feature.
- The response header is attached only when the host calls the response interceptor or the stream header-init hook.
- This build advertises plugin schema 1 so older CPA v7 hosts still load it. HTML in management JSON is carried as raw bytes.
- There is no Cloudflare Worker, edge secret, or separate prompt process.
- Prompt bodies and API keys are not written to the sync log.

## 中文说明

`prompt-admin` 是 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（CPA）的原生插件：为每个模型注入一条系统提示词，并在 CPA 管理中心里提供「模型提示词」管理页面。

- **自动匹配**：填入一个或多个 Git 提示词仓库（GitHub / GitLab / Codeberg 或单个 raw 文件），插件按文件名/路径与实时模型列表做确定性打分匹配（不调用模型）。同一模型有多个候选时，页面并排展示，由你选择「保留这个」。
- **自动更新**：模型列表约每 15 秒刷新一次，仓库默认每小时拉取一次；修改提示词无需重启 CPA。
- **安装方式一（手动）**：从 [Releases](https://github.com/Tx13758627780/cpa-plugin-prompt-admin/releases) 下载对应平台的 zip，校验 `checksums.txt` 后，把 `prompt-admin.so` 解压到 `plugins/linux/amd64/`，在 `config.yaml` 中加入上文的 `plugins` 配置块，重启 CPA。
- **安装方式二（插件商店）**：在管理中心的插件商店中安装 Prompt Admin（需所用的商店 registry 已收录本插件）。
- **管理页面**：安装后打开 CPA 管理中心并登录，在菜单中进入「模型提示词」即可直接使用，无需再输入/配置管理密钥或插件密码。单独打开资源 URL（不经管理中心）无法编辑（设计如此）。页面地址：`/v0/resource/plugins/prompt-admin/index.html`。
- **验证注入**：保存某模型的提示词后调用 `/v1/chat/completions`，响应头中出现 `X-Prompt-Admin: injected` 即表示已注入。

## License

MIT. See [LICENSE](LICENSE).
