# v0.1.5: prompt repository sync without Git

## Confirmed defects in v0.1.4

- `internal/engine/repo.go` unconditionally executed system `git` for repository URLs. Minimal Docker runtime images may not include it, producing `exec: "git": executable file not found in $PATH`. This is a prompt-repository sync failure, separate from model discovery.
- Existing CI runners have Git installed. The earlier tests did not remove it from PATH, so they did not reproduce the minimal-runtime failure.
- The plugin intentionally advertises RPC schema 1 for older hosts. Newer CPA hosts HTML-escape JSON string values (and keys) for schemas below 6. This displayed `&#34;` in errors and could alter prompt text copied into an editor and saved again.
- The repository table could squeeze columns into vertical text on small screens.

## Changes

- When Git is absent, GitHub, GitLab.com and Codeberg sources download HTTPS ZIP snapshots automatically. Default and explicit branches, subdirectories, tokens and repeat syncs are supported. Public GitHub downloads use public archives without REST API quota; private sources use their authenticated forge API.
- Download and extract into temporary locations. Validate one archive root, paths, file types, ZIP integrity, duplicate files, size/count limits and the selected subdirectory before replacing the old cache. Failed archive updates keep the previous files. Initial Git clones also stage before replacing a cache.
- Keep only eligible prompt files. Reject path traversal; never extract symlinks. Strip credential headers on cross-origin redirects. Report HTTP failures without echoing tokens or signed URLs.
- The embedded page requests an opt-in `wire=base64-json-v1` JSON envelope. Its UTF-8 payload survives old-host HTML escaping byte-for-byte, including literal entity text, without increasing the minimum host schema. Plain JSON clients and HTML resources keep their existing contracts; the UI continues to render user content through text/value properties.
- Put the repository table inside a horizontal scrolling container on narrow screens.

## Verification

```bash
make fmt-check
make test
CGO_ENABLED=1 go test -race ./internal/...
go vet ./...
make build VERSION=0.1.5
```

Go regression tests explicitly set PATH to an empty temporary directory. Coverage includes an end-to-end archive sync and prompt update; stale-file removal; failed-update cache retention; HTTP 401/403/404; branch/subdirectory handling; raw-file fallback; private-token headers and redirect stripping; cancellation; corrupt ZIP CRCs; traversal, duplicate files and archive-root validation; and schema-1-safe transport preserving Unicode, quotes, entity literals and map keys. Seven embedded-JavaScript tests cover the UI transport and existing model-list/error flows.

A separate live smoke test (excluded from network-independent CI) downloaded the public repository from the reported screenshot with Git removed from PATH and scanned 55 eligible files. The compiled Linux shared library was also exercised through its native C ABI with an empty PATH, model-list HTTP fixture, exact-text management round trips and live repository sync. No deployed CPA configuration or installed plugin was changed.

## Publish v0.1.5

1. Confirm the current main commit has passed the `release` workflow's test job and all five platform builds. Run every verification command above on that commit before tagging.
2. From the latest main checkout:

   ```bash
   git switch main
   git pull --ff-only
   git tag -a v0.1.5 -m "prompt-admin v0.1.5: sync prompt repositories without git"
   git push origin v0.1.5
   ```

3. Wait for the tag-triggered workflow to finish. Verify five archives (`linux_amd64`, `linux_arm64`, `windows_amd64`, `darwin_amd64`, `darwin_arm64`) plus `checksums.txt`; download each archive, check SHA-256, and verify its only root entry is the corresponding plugin library.
4. If needed, submit a metadata-only Plugin Store PR updating its legacy version display fallback to `0.1.5`. The store otherwise follows the latest GitHub Release automatically.
5. The operator installs the new version and reopens the page from the Management Center, then retries **立即同步**. Deployment and server configuration changes are a separate step.
