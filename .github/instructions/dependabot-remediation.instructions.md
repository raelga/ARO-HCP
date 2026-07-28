---
applyTo: "**/go.mod,**/go.sum,go.work,go.work.sum,api/package.json,api/package-lock.json,.github/dependabot.yml"
---

# Dependabot remediation — agent instructions

These rules govern any task that fixes a Dependabot / security-advisory alert in
this repository (whether started by a human via the `fix-dependabot` skill or by
the Copilot cloud agent from an assigned alert / issue).

ARO-HCP is a **`go.work` multi-module workspace (34 modules)**. Native Dependabot
opens one isolated PR per manifest directory and does **not** run the workspace
sync ritual, so its PRs fail CI. Always follow the flow below instead.

## 1. Group, don't fan out

Group alerts by **package / advisory cascade**, not by manifest directory:

- One PR per package that clears the alert everywhere it appears.
- Find **all** affected modules yourself — don't trust Dependabot's directory
  list. Grep every `go.mod`:
  ```bash
  grep -rl '<module-path>' --include=go.mod .
  ```
- Multiple manifests for the same CVE → same PR. A bump that cascades through
  `go work sync` into other modules belongs in the same PR.

## 2. Apply the fix + the workspace ritual (Go)

```bash
# 1. Bump in every affected module directory:
for dir in <affected-dirs>; do (cd "$dir" && go get <package>@<fixed-version>); done

# 2. Run the FULL workspace ritual — this is the step native Dependabot skips:
make all-tidy      # = per-module `go mod tidy` -> `go work sync` -> fmt -> licenses

# 3. Validate before pushing (there is no root `make build`; `test-compile`
#    compiles every module in the workspace):
make test-compile
make test
make lint
make verify        # deepcopy, json-format, yamlfmt, materialize, gomega, schema — what CI runs
```

## 2a. Required checks — the agent MUST reproduce every gating CI check locally

The PR is gated upstream by these Prow contexts. Run the mapped command in the
Copilot environment and do not finish until each is green. This is the
**definition of done** — a bump that only *looks* right but fails one of these is
not done.

| Prow required check      | Reproduce locally with                                   |
|--------------------------|----------------------------------------------------------|
| `ci/prow/verify`         | `make verify` **and** `make all-tidy` leaves a clean tree |
| `ci/prow/lint`           | `make lint` (`LINT_GOTAGS=E2Etests`)                     |
| `ci/prow/test-unit`      | `make test` (envtest auto-downloads)                     |
| `ci/prow/mega-linter`    | `make verify-yamlfmt` + `git diff --exit-code` (yaml/format) |
| `ci/prow/images`         | best-effort `make -C <changed svc> build` if a service dir changed |
| `ci/prow/e2e-*`          | cannot run locally (needs a live Azure cluster) — a pure dep bump ships no runtime change; do NOT attempt, retest on flake |

After all local checks pass, **mark the PR ready for review** (`gh pr ready
<num>`) — an agent PR must not be left as a draft. Then drive it per §7.

- **Generated-SDK modules** under `test/sdk/*` are **not** in the `go.work` `use`
  list. Building them inside the workspace fails; build/tidy them with
  `GOWORK=off`:
  ```bash
  (cd test/sdk/<mod> && GOWORK=off go mod tidy && GOWORK=off go build ./...)
  ```
- A bump can leave a stray `go.sum` missing only the `…/go.mod` hash line for a
  transitively-affected module (e.g. `tooling/prometheus-rules`). `make all-tidy`
  usually fixes it, but if CI's `verify-deepcopy` still shows go.sum drift, run
  `go mod tidy` **inside that specific module** and re-run `make all-tidy`.

## 3. npm (transitive deps under /api)

- Prefer **version-scoped overrides** in `api/package.json`
  (`"<pkg>@<vulnerable-range>": "<fixed>"`), never a blanket override — a blanket
  override silently pins unrelated major branches.
- `cd api && npm install --package-lock-only`, then confirm `npm audit` reports
  **0 vulnerabilities** (it flags ranges Dependabot misses, e.g. js-yaml 4.x).

## 4. No-fix advisories

If an advisory has **no patched version** (`first_patched_version` is null /
range is `<= latest`), do **not** fake a fix. Dismiss the alert with a
justification (`tolerable_risk` / `no_bandwidth` / `inaccurate`) and track it in
the linked JIRA. Call it out explicitly in the PR body.

## 5. PR + commit conventions

- Conventional Commits: `deps(<scope>): bump <package> to fix <N> CVEs (<JIRA-KEY>)`.
- Diff must be dependency-only (`go.mod`/`go.sum`/`go.work.sum` or the lockfile) —
  no source edits.
- PR body must include:
  - the JIRA link at the top,
  - a **CVEs fixed** section (each CVE linked to its record + an alert-search link),
  - `Closes #NNNN` for every superseded native-Dependabot PR,
  - the **count of Dependabot alerts** the PR resolves.
- Author `Rael Garcia <rael@redhat.com>`; **no** `Co-authored-by: Copilot` trailer.

## 6. Guardrails (never cross these)

- **Never self-merge, force-merge, `/override`, or route around required checks.**
  `tide` + a maintainer `/lgtm` is the human gate and must be preserved.
- `tide` is **not** a CI check — ignore it when judging pass/fail.
- Treat `e2e-parallel` provisioning timeouts (`CreateHCPCluster … 20 minutes
  exceeded`) and `InvalidSubscriptionState` as environmental flakes: verify scope
  (same signature on unrelated PRs / prior pass on same HEAD), then retest with
  evidence — do **not** patch and do **not** treat as PR-caused. A pure
  dependency bump ships no runtime change and cannot regress provisioning.
  To rerun a single job comment `/test e2e-parallel` (the job short-name);
  `/retest <target>` is **rejected** by prow, plain `/retest` reruns all failed.
- Push with `--force-with-lease`, never plain `--force`.

## 7. Drive the PR to green (monitor loop — do not stop at "opened")

Opening the PR is not the finish line. Follow the **`pr-merge-monitor`** skill:
poll → classify → act until every required check is green and the PR is ready
for a maintainer's `/lgtm`:

1. After the fix is pushed and local checks pass, `gh pr ready <num>` so CI runs.
2. `gh pr checks <num> --watch`. For each **red** required check:
   - **Caused by this bump** (stale go.sum, tidy drift, a renamed API in the new
     version, a lockfile mismatch) → apply the **minimum in-scope fix**, prefer
     regenerating over hand-editing, re-run the mapped local check from §2a, then
     `--force-with-lease` push.
   - **Environmental flake** (`e2e-parallel` 20-min provisioning timeout,
     `InvalidSubscriptionState`, proxy/IPv6 `go mod` errors) → prove scope (same
     signature on unrelated PRs / prior green on same HEAD), then retest with an
     evidence comment (`/test <job>`); never patch.
3. Every mutating action (force-push, `/test`) needs a "why" PR comment with hard
   evidence, and you must then confirm it took effect (new HEAD SHA / new build-id
   pending). A posted comment is not proof of effect.
4. Repeat until green. **Never** self-merge / force-merge / `/override`.

Then create/attach the JIRA per the `jira-pr-workflow` skill and set the JIRA
status (`Review` while open, `Release Pending` on merge, `Closed` only in prod).
