// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.

package toolchain

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"

	"github.com/Azure/ARO-HCP/tooling/depctl/internal/repository"
)

var publicationSHA = regexp.MustCompile(`^[a-f0-9]{40}$`)

// ReadPublicationPlan binds a candidate to the single trusted workflow's readiness artifact.
func ReadPublicationPlan(path string) (Plan, error) {
	var plan Plan
	data, err := os.ReadFile(path)
	if err != nil {
		return plan, fmt.Errorf("read readiness evidence: %w", err)
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		return plan, fmt.Errorf("decode readiness evidence: %w", err)
	}
	if plan.Schema != 1 || plan.Status != "ready" || plan.Repo != Repository || !publicationSHA.MatchString(plan.BaseSHA) || !patchVersion.MatchString("go"+plan.Target) {
		return plan, fmt.Errorf("invalid publication readiness evidence")
	}
	return plan, nil
}

// RecheckPublication verifies the exact base, open upgrade inventory and current builder evidence.
func RecheckPublication(ctx context.Context, root string, plan Plan) error {
	if err := verifyDefaultHead(ctx, plan.Repo, plan.BaseSHA); err != nil {
		return err
	}
	open, err := openUpgrades(ctx, plan.Repo)
	if err != nil {
		return fmt.Errorf("recheck open Go upgrades: %w", err)
	}
	if len(open) > 0 {
		return blockedf("Go upgrade PR #%d is already open", open[0].Number)
	}
	checkout, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("open readiness files: %w", err)
	}
	defer checkout.Close()
	fresh, configs, err := builderReadiness(ctx, checkout, plan.Target)
	if err != nil {
		return fmt.Errorf("recheck compiler builders: %w", err)
	}
	if len(fresh) != len(plan.Builders) || !slices.Equal(configs, plan.ReleaseConfigs) {
		return blockedf("compiler builder inventory changed")
	}
	for i, prior := range plan.Builders {
		if !reflect.DeepEqual(prior, fresh[i]) {
			return blockedf("compiler builder evidence changed for %s", prior.Image)
		}
	}
	return draftNotificationsSafe(ctx)
}

// PublicationCandidate checks the candidate is one clean commit on the exact trusted base.
func PublicationCandidate(ctx context.Context, root string, plan Plan) (string, []string, error) {
	repo, err := repository.Open(root)
	if err != nil {
		return "", nil, fmt.Errorf("open publication checkout: %w", err)
	}
	if err := requireCleanCandidate(ctx, repo); err != nil {
		return "", nil, err
	}
	parent, err := repo.Resolve(ctx, "HEAD^1")
	if err != nil {
		return "", nil, fmt.Errorf("resolve candidate parent: %w", err)
	}
	if parent != plan.BaseSHA {
		return "", nil, blockedf("candidate is not based on the planned default branch")
	}
	head, err := repo.Head(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("read candidate HEAD: %w", err)
	}
	paths, err := repo.ChangedPathsBetween(ctx, plan.BaseSHA, head)
	if err != nil {
		return "", nil, fmt.Errorf("inspect candidate changes: %w", err)
	}
	if len(paths) == 0 {
		return "", nil, fmt.Errorf("empty toolchain upgrade")
	}
	for _, path := range paths {
		if !allowedCandidatePath(path) {
			return "", nil, fmt.Errorf("candidate contains unexpected path %s", path)
		}
	}
	return head, paths, nil
}
