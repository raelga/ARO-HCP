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
