---
name: "Deps & Tools: Update Go Toolchain"
on:
  workflow_dispatch:
  schedule: daily
  needs: [upgrade]
if: ${{ !cancelled() && github.repository == 'Azure/ARO-HCP' && github.ref == format('refs/heads/{0}', github.event.repository.default_branch) }}
permissions:
  contents: read
  pull-requests: read
  actions: read
  copilot-requests: write
engine: copilot
model: sonnet
concurrency:
  group: go-toolchain-upgrade
  cancel-in-progress: false
tools:
  github:
    read-only: true
    toolsets: [actions]
network:
  allowed: [defaults, go]
safe-outputs:
  noop:
    report-as-issue: false
jobs:
  upgrade:
    name: Prepare a verified Go patch draft
    needs: []
    runs-on: ubuntu-latest
    timeout-minutes: 90
    if: github.repository == 'Azure/ARO-HCP' && github.ref == format('refs/heads/{0}', github.event.repository.default_branch)
    permissions:
      contents: read
      pull-requests: read
    env:
      GOTOOLCHAIN: local
    steps:
    - name: Checkout trusted default branch
      uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
      with:
        fetch-depth: 0
        persist-credentials: false
    - name: Set up current Go
      uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e
      with:
        go-version-file: go.work
        cache: false
    - name: Build trusted depctl
      run: make -C tooling/depctl build BINARY="$RUNNER_TEMP/depctl"
    - name: Check patch and builder readiness
      id: plan
      env:
        GH_TOKEN: ${{ github.token }}
      run: |
        set -euo pipefail
        "$RUNNER_TEMP/depctl" toolchain plan > "$RUNNER_TEMP/go-toolchain-plan.json"
        jq -r '"status=\(.status)\ntarget=\(.target // "")\nbase=\(.base_sha)"' "$RUNNER_TEMP/go-toolchain-plan.json" >> "$GITHUB_OUTPUT"
        jq -r '.reason' "$RUNNER_TEMP/go-toolchain-plan.json" >> "$GITHUB_STEP_SUMMARY"
    - name: Set up the exact target Go compiler
      if: steps.plan.outputs.status == 'ready'
      uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e
      with:
        go-version: ${{ steps.plan.outputs.target }}
        cache: false
    - name: Set up Node for API regeneration
      if: steps.plan.outputs.status == 'ready'
      uses: actions/setup-node@949feb2413d6458794dcd2491c4babbbce0c15c1
      with:
        node-version: '22'
    - name: Prepare and validate the candidate
      if: steps.plan.outputs.status == 'ready'
      env:
        BASE: ${{ steps.plan.outputs.base }}
        TARGET: ${{ steps.plan.outputs.target }}
      run: |
        set -euo pipefail
        "$RUNNER_TEMP/depctl" toolchain prepare --base "$BASE" --target "$TARGET"
        git -c user.name='aro-hcp-robot[bot]' -c user.email='aro-hcp-robot[bot]@users.noreply.github.com' commit -am "fix(toolchain): bump Go to $TARGET"
        "$RUNNER_TEMP/depctl" toolchain validate --target "$TARGET"
    - name: Recheck publication readiness
      id: recheck
      if: steps.plan.outputs.status == 'ready'
      env:
        GH_TOKEN: ${{ github.token }}
      run: '"$RUNNER_TEMP/depctl" toolchain recheck --plan "$RUNNER_TEMP/go-toolchain-plan.json" >> "$GITHUB_OUTPUT"'
    - name: Mint the scoped draft publication token
      id: writer
      if: steps.recheck.outputs.ready == 'true'
      uses: actions/create-github-app-token@64ab729a6294000f5aa4a4bc0b73d1f32bc45510
      with:
        client-id: ${{ secrets.DEPENDABOT_APP_CLIENT_ID }}
        private-key: ${{ secrets.DEPENDABOT_APP_PRIVATE_KEY }}
        repositories: ${{ github.event.repository.name }}
        permission-contents: write
        permission-pull-requests: write
    - name: Publish only the validated draft
      if: steps.recheck.outputs.ready == 'true'
      env:
        GH_TOKEN: ${{ steps.writer.outputs.token }}
        CI_TOKEN: ${{ github.token }}
      run: '"$RUNNER_TEMP/depctl" toolchain publish --plan "$RUNNER_TEMP/go-toolchain-plan.json"'
    - name: Upload readiness evidence
      if: always()
      uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9
      with:
        name: go-toolchain-readiness
        path: ${{ runner.temp }}/go-toolchain-plan.json
        if-no-files-found: ignore
---

# Explain Go toolchain upgrade outcomes

The trusted upgrade job decides whether a Go patch is ready, prepares and tests
it, and can publish a draft. Do not propose or apply checkout changes, request
reviewers, or publish anything. Use the job outcome and its readiness evidence
to summarize what happened. When blocked, give the specific builder, existing
PR, or failing validation step that needs attention. Use `noop` with that
summary. Never treat a blocked or failed upgrade as published.
