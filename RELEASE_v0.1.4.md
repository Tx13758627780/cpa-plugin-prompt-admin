# v0.1.4: model discovery repair

## Investigation

The v0.1.3 baseline is commit `c352ac1`.

| Responsibility | File / symbols |
| --- | --- |
| Browser authentication, state fetch, rendering | `internal/panel/index.html`: `api`, `load`, `renderModels` |
| Plugin HTTP actions | `internal/panel/panel.go`: `Handle` |
| Registered management routes | `internal/runtime/runtime.go`: `ManagementRegistration` |
| Fetch and config parsing | `internal/engine/models.go`: `fetchModels`, `fetchModelIDs`, `readCPAConfig` |
| Defaults, recomputation, diagnostics | `internal/config/config.go`, `internal/engine/engine.go`, `compute.go`, `view.go` |

Confirmed code defects:

1. `cpa_config` defaulted to empty. `fetchModels` requested `/v1/models` only if it had a nonempty client key. A fresh installation with only `enabled: true` therefore never tried discovery, despite the host's populated `config.yaml`. A Management Center login did not populate the backend client API key.
2. The regex reader inspected only an `access` block and required a multiline key list. Legacy top-level `api-keys`, inline YAML lists and structured v8 provider groups were not reliably supported. Reading failures were silently ignored.
3. The default URL fixed the listener at `http://127.0.0.1:8317`. Loopback is correct for this in-process plugin in the standard Docker image, but custom ports/TLS were not inferred. Appending `/v1/models` also duplicated `/v1` for bases already ending in that path.
4. Browser `API` was fixed to `/v0/management/...`, losing deployment prefixes such as `/cpa`. Proxy headers and CORS cannot be inspected on the reported server without its deployment access; the repository defect is the lost prefix. The implementation does not require cross-origin browser requests.
5. Backend non-2xx and JSON errors were logged, then replaced by a generic empty-model error. A successful `{}` response silently parsed as an empty list. Browser JSON exceptions and `load()` failures were swallowed; the initial failure could become an authentication notice.
6. The release matrix used `macos-14`, which GitHub retired on November 2, 2026. The first validation run confirmed both macOS jobs were cancelled before any steps ran. The matrix now uses `macos-15` for both Darwin targets (Clang selects the output architecture). See [GitHub retirement announcement](https://github.blog/changelog/2026-10-01-github-actions-macos-14-runner-image-retirement/).
7. The old Refresh button only reloaded state; there was no persistent manual-model registration.

## API contract checked against upstream

Checked upstream `router-for-me/CLIProxyAPI` main at `47f18e4` (2026-11-27):

- [Management API v8](https://github.com/router-for-me/CLIProxyAPI/blob/47f18e4/docs/management-api-v8.md): Management Key in `Authorization: Bearer` or `X-Management-Key`. Client keys live at `/v8/management/config/access/api-keys`, whose response is the YAML value (an array), while old `/v0/management/api-keys` uses its legacy payload. Plugin-defined HTTP extensions retain their declared `/v0/management` routes.
- [Configuration](https://github.com/router-for-me/CLIProxyAPI/blob/47f18e4/config.example.yaml): `management`, `access.api-keys`, `server.host/port/tls`; legacy aliases remain readable by CPA.
- [Routes](https://github.com/router-for-me/CLIProxyAPI/blob/47f18e4/internal/api/server_routes.go): `/v1/models` remains the client-facing live catalog, with client API authentication. Credential model endpoints and static channel definitions are not substitutes for this live catalog.
- [Dockerfile](https://github.com/router-for-me/CLIProxyAPI/blob/47f18e4/Dockerfile) and [Compose](https://github.com/router-for-me/CLIProxyAPI/blob/47f18e4/docker-compose.yml): CPA runs in `/CLIProxyAPI` and the standard config mount is `/CLIProxyAPI/config.yaml`.

The fix reads client keys server-side from the host configuration, avoiding a browser round trip to either management key-list endpoint. Management Key is never repurposed as a `/v1/models` key; no new management endpoint is guessed. The C ABI schema stays at 1 for legacy hosts; the existing v7 SDK dependency remains a build-time dependency, not a requirement that the host run v7.

## Resulting behavior

- Infer the config path from the host `-config` / `--config` argument, otherwise `config.yaml`; explicit `cpa_config`, `api_key`, and `cpa_url` overrides remain available.
- Parse YAML structurally; prefer `access.api-keys` even when explicitly empty. Ignore management and upstream secrets when choosing the client key. Derive listener address/port/TLS unless `cpa_url` was explicitly set.
- Accept root, proxy-prefixed, `/v1`, and `/v1/models` base URLs. Refuse redirects so API credentials are not forwarded to a login destination.
- Accept OpenAI `data`, legacy `models`, or bare model arrays; trim, deduplicate and sort names. Report HTTP status, JSON errors, invalid schemas, empty catalogs, network/TLS failures and unreadable config. Selected client keys are redacted from diagnostics.
- Preserve proxy prefixes for same-origin browser requests. Show persistent request errors and offer Retry, explicit model refresh, and manual names with add/remove controls.
- Persist manual models across restarts; allow prompt edits/injection during discovery failure. Keep existing prompt entries during outages and resume normal pruning when discovery recovers.
- Test both Go code and the actual embedded JavaScript using Node's built-in test runner. Run the release workflow for main pushes and PRs as well as tags; publishing remains tag-only. Replace retired `macos-14` runners with `macos-15`.

## Validation

Before tagging, run:

```bash
make fmt-check
make test                  # Go internal packages + embedded JavaScript behavior
CGO_ENABLED=1 go test -race ./internal/...
go vet ./...
make build VERSION=0.1.4    # native linux/amd64 shared library
```

Regression coverage includes fresh v8 config discovery without plugin API overrides, legacy YAML, new/legacy key precedence, quoted/inline keys, nonstandard listener ports, TLS address inference, reverse proxy prefixes, client versus management authentication, unauthenticated catalogs, HTTP 401/403/404, redirects, HTML/invalid JSON, schema mismatches, empty catalogs, explicit refresh, persistent manual models, outage prompt retention on disk, and recovery.

The tests simulate HTTP endpoints; they do not establish that any particular deployed server has an upstream credential or that Cloudflare/its proxy forwards Authorization. Test the intended Docker deployment after installing the build.

## Publish v0.1.4

1. Wait for the main-branch `release` workflow to pass its test job and all five native platform builds. Review the diff and this document.
2. From an up-to-date main checkout:

   ```bash
   git switch main
   git pull --ff-only
   git tag -a v0.1.4 -m "prompt-admin v0.1.4: repair model discovery"
   git push origin v0.1.4
   ```

3. The tag triggers `.github/workflows/release.yml`: tests, five archives (`linux_amd64`, `linux_arm64`, `windows_amd64`, `darwin_amd64`, `darwin_arm64`), `checksums.txt`, then GitHub Release creation. A workflow_dispatch run builds artifacts without publishing a release.
4. Verify the release contains all five ZIPs plus `checksums.txt`. Each ZIP must contain only the correctly named plugin library at its root. Verify the SHA-256 checksums.
5. Update any external Plugin Store registry entry to `0.1.4` if its version is pinned. That registry is separate from this repository.
6. Back up the existing plugin data directory, install/update the plugin and reload it through CPA (or restart the Docker container). Reopen the page from the Management Center. Test automatic discovery, then force an invalid API key in a test deployment to verify the diagnostic and manual-model fallback. Restore the valid key and verify recovery. Do not overwrite the main CPA config with the README's plugin-only example.

The code/version bump prepares v0.1.4. Committing it does not create a tag or publish a GitHub Release.
