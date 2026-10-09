# cibot

Automation for LLAR Hub, with streaming toc file handling and a GitHub webhook service.

Build the command with `go build -o ci ./cmd/ci`, then run `./ci serve`. The command loads `.env` from its working directory, constructs the bot, and serves GitHub webhooks. Existing process environment variables take precedence over `.env` values. On SIGINT or SIGTERM, the server stops accepting connections and waits for active requests to finish.

Use `.env.example` as a configuration template:

| Setting | Value |
| --- | --- |
| `WEBHOOK_SECRET` | The secret configured on the GitHub App's webhook. |
| `APP_ID` | The GitHub App's numeric ID. |
| `PRIVATE_KEY_FILE` | Path to the App's PEM private key; relative paths are resolved from the working directory. |
| `LISTEN_ADDR` | HTTP listen address; defaults to `:80` when empty or unset. |

`internal/bot.Bot` implements `http.Handler`. Construct it with `bot.New(webhookSecret, appID, privateKeyFile)` and attach it to an HTTP server owned by the caller. The webhook secret is a nonempty string. The constructor loads the App's PEM private key and initializes a private, empty installation-client map without making network requests. The secret and authentication configuration must remain unchanged while serving.

The handler verifies POST deliveries using go-github before parsing or dispatching events and acknowledges webhook pings. For merged PRs targeting `main` in `llarhub/.index` or `MeteorsLiu/llarhub`, it lazily creates and caches a GitHub client keyed by that delivery's installation ID. A mutex protects lookup and creation, but is released before any network requests. Each cached client owns an installation transport that obtains, caches, and refreshes its token through `ghinstallation`. App transport copies isolate the fields changed during token refresh while sharing the parsed key and underlying connections. Clients remain cached for the lifetime of the bot.

Using the installation client, the handler paginates the PR's changed files and deduplicates affected top-level project directories, including both paths of renamed files. Hidden directories and root-level files are ignored. It checks the merge commit's root tree to exclude deleted directories, symlinks, and submodules, then queries each project's repository under the registry's own owner. Test registry events therefore provision `MeteorsLiu/proj`, not `llarhub/proj`.

For each delivery, the handler partially clones the registry with `--filter=blob:none --no-checkout` into a temporary directory. It selects the affected project directories with cone-mode sparse checkout before checking out the merge commit, so only those directories and root-level files are materialized and their contents downloaded. Commit and tree history is retained; this is not a shallow clone. It copies each affected project's configuration into its own working directory and reads the source ID from `versions.json.path`. It updates the aggregate toc PR, then directly loops over the projects to create repositories, generate bindings, and publish branches and tags. A generation failure leaves the toc PR already submitted by the first step intact. Temporary checkouts are removed when the request finishes.

`updateTOC` finds an open PR into `main` whose head belongs to the same registry and uses the `llarhub-bot/toc-pr-` branch prefix. If one exists, the new request appends to that branch's current toc and commits on its current head, preserving earlier pending records. Otherwise it starts a new `llarhub-bot/toc-pr-<first-registration-pr-number>` branch from the latest `main` and opens a PR. Subsequent requests accumulate in that open PR rather than creating separate PRs. A mutex serializes the lookup and update within the bot, so concurrent requests do not create separate aggregation PRs or overwrite each other's additions. Once a PR is closed or merged, the next registration starts a new aggregation cycle.

`toc.File.Add` returns `(bool, error)`: a successful append returns true, while finding the literal `full + " " + proj` byte sequence returns false. It uses `bytes.Contains` with a local 64 KiB array and overlapping reads, without parsing existing records, checking field/line boundaries, or rejecting another project mapped to the same source. Each `File` owns a mutex covering the entire lookup and append, so concurrent calls on that instance do not append the same mapping twice. A `File` must not be copied after first use; separate instances targeting the same path still require caller synchronization. A request containing only matching mappings leaves the toc PR unchanged. Once project inputs have been copied out, the bot narrows sparse checkout to root-level files, then fetches and checks out the latest aggregation branch while holding the bot's toc lock. This avoids downloading newer project contents just to update the toc. It appends records and commits and pushes only `llarhub.toc`; unselected directories remain in the commit. File contents stay on disk; the bot does not build an in-memory blob or base64 JSON request. It does not edit the contributor's PR or merge its own toc PR.

`provision` attempts to create an initialized public repository when the project repository lookup returned 404, using the personal-account or organization endpoint; GitHub remains responsible for rejecting inaccessible or occupied names. In the copied `llcppg.cfg`, it sets `LLGoPackage` to `link: $(llar install <source-id>)` using `versions.json.path`, preserving the other configuration fields. This expression is generated into the Go package for LLGo to execute at build time; it does not embed the bot's local installation paths. It runs `llar install <source-id> -o <native-dir>`, copies the installed headers into the source configuration directory, and invokes the generator as `llcppg <output-dir> <source-dir>`. It prepares the generated package's `go.mod` with `go mod edit -module github.com/<owner>/<proj>` and `go mod tidy`.

The submitted project directory contains `versions.json`, the LLAR formula, and `llcppg.cfg`, with any additional generator configuration files. It does not need `go.mod` or `go.sum`; those belong to the generated project repository. Prefix trimming and other binding choices remain human-authored. The runtime must have Git with `--config-env` support, `llar`, an `llcppg` with `-init` support built with LLGo, and the required Go/LLVM environment on `PATH`.

Project publication uses a local clone. For a newly created repository, `llcppg -init <proj>` commits the template's `c` and `main` branches before the bot overlays the registered configuration and headers under `c/`. A private mutex serializes only `-init` calls within the bot because llcppg shares a template cache; generation and publishing remain outside that lock. The bot changes the C module path to `github.com/<owner>/<proj>/c`, including for personal-account test repositories.

An existing `c` branch retains its history and module dependencies. When an existing repository has no `c` branch, the bot starts it from the default branch and initializes its C module with Go 1.23 and `github.com/goplus/lib v0.6.1`. Generation uses that branch's `c/` directory, and generated bindings are committed to the repository's default branch. Files are overlaid without removing unrelated repository files. Branches are pushed with explicit refspecs and without force. New repositories receive `c/v0.1.0` for the source module and `v0.1.0` for the generated module; updates to existing repositories preserve their tags. Initialization or generation failure does not publish project branches or tags.

GitHub API calls handle repository discovery/creation and PR operations. Git handles checkout, file transfer, commits, branches, and tags. Each Git process obtains its installation's cached/refreshed token and receives it through an environment-backed HTTPS authorization header scoped to `github.com`. Credentials are not placed in command arguments, clone URLs, or persistent Git configuration. Commits use the `llarhub-bot[bot]` identity; Git identity and credential settings apply only to that process. The `llcppg -init` child process receives Git author and committer identity through its own environment, without the installation token.

For non-deletion pushes to `c`, regardless of repository owner, the handler checks out the event's `after` commit and uses its `c/` directory as the generation input. That directory supplies `go.mod`, `llcppg.cfg`, and headers; `versions.json` and another `llar install` are not needed for this path. It uses the same generation and module preparation code as registration, then commits and pushes the output to the repository's default branch. The commit message identifies the source SHA. The source branch and existing tags are left unchanged, and ordinary pushes do not create version tags or toc PRs.

Registration and c-branch generation currently run synchronously in the webhook request. Authentication, API, and command failures return 500; no queue or automatic delivery retry is implemented. Invalid signatures are rejected with 403, invalid event payloads with 400, and unrelated events are acknowledged with 204. Consumers of the generated `LLGoPackage` expression need LLGo with support for `llar install` command expansion; that support is proposed in https://github.com/xgo-dev/llgo/pull/2755.

The integration tests simulate the GitHub API and substitute the `llar` and `llcppg` executables, while running real Git commands against temporary local bare repositories. They verify published files, retained commit history and tags, toc PR aggregation under concurrent deliveries, installation authentication, and rejection of non-fast-forward pushes. They do not replace a real-toolchain generation test or a live GitHub deployment check.

Local deployment settings belong in `.env`, which is ignored by Git. Keep the private key outside the repository.

## Container

The image includes `ci`, Git, Go, LLAR, llcppg, and the LLVM 22/LLGo toolchain used to build the generator. Tool source revisions are pinned in the Dockerfile. Published images target Linux amd64. The default command is `ci serve` and the default listen port is 80.

```sh
docker build --platform linux/amd64 -t cibot:local .
docker run --rm --platform linux/amd64 -p 80:80 --env-file .env \
  --mount type=bind,src=/absolute/path/to/app.private-key.pem,dst=/run/secrets/github-app.pem,readonly \
  -e PRIVATE_KEY_FILE=/run/secrets/github-app.pem \
  cibot:local
```

The image contains only the public `.env.example` defaults. Real `.env` files, private keys, and Git metadata are excluded from the build context. Use a container-visible private key path as shown above, rather than the host path stored in your local `.env`.

## GitHub Actions

- `Go` runs on pull requests and pushes to `main` or tags. It runs vet and race-enabled tests, generates `coverage.out`, and uploads coverage to Codecov. Set the repository Actions secret `CODECOV_TOKEN` to its Codecov upload token. Upload failures fail this workflow; public fork PRs use Codecov's supported tokenless upload path when the secret is unavailable.
- `Docker` runs on pull requests and tag pushes. PRs build the Linux amd64 image without logging in or pushing. Tag pushes publish `ghcr.io/meteorsliu/cibot:<git-tag>` using `GITHUB_TOKEN` with `packages: write`. Image names are lowercased and tag characters are normalized by Docker's metadata action. No additional `latest` tag is published.

Both workflows use `ubuntu-latest`. GHCR publishing does not require a separate registry password.
