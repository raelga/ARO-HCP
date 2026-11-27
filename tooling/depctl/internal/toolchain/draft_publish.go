// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.

package toolchain

import (
	"context"
	"fmt"
	"net/http"
	"time"

	gh "github.com/google/go-github/v69/github"

	"github.com/Azure/ARO-HCP/tooling/depctl/internal/repository"
	automation "github.com/Azure/ARO-HCP/tooling/github"
)

// PublishDraft pushes the validated candidate and verifies the resulting owned draft.
func PublishDraft(ctx context.Context, root string, plan Plan, readToken, writeToken string) (automation.Receipt, error) {
	var empty automation.Receipt
	const actor = "aro-hcp-robot[bot]"
	if readToken == "" || writeToken == "" || readToken == writeToken {
		return empty, fmt.Errorf("draft publication requires distinct read and write tokens")
	}
	head, paths, err := PublicationCandidate(ctx, root, plan)
	if err != nil {
		return empty, err
	}
	if err := verifyDefaultHead(ctx, plan.Repo, plan.BaseSHA); err != nil {
		return empty, err
	}
	open, err := openUpgrades(ctx, plan.Repo)
	if err != nil {
		return empty, fmt.Errorf("check draft overlap: %w", err)
	}
	if len(open) != 0 {
		return empty, blockedf("Go upgrade PR #%d is already open", open[0].Number)
	}
	if err := draftNotificationsSafe(ctx); err != nil {
		return empty, err
	}
	transport := &http.Client{Timeout: time.Minute}
	api := gh.NewClient(transport).WithAuthToken(readToken)
	writer := gh.NewClient(transport).WithAuthToken(writeToken)
	client, err := automation.NewClient(plan.Repo, api)
	if err != nil {
		return empty, fmt.Errorf("configure GitHub reader: %w", err)
	}
	client, err = client.WithWriter(writer)
	if err != nil {
		return empty, fmt.Errorf("configure GitHub writer: %w", err)
	}
	client, err = client.WithActor(actor)
	if err != nil {
		return empty, fmt.Errorf("bind draft owner: %w", err)
	}
	branch := "automated-go-toolchain-" + plan.Target + "-" + head
	repo, err := repository.Open(root)
	if err != nil {
		return empty, fmt.Errorf("open publication checkout: %w", err)
	}
	if _, err := repo.NativeGit(ctx, nil, "", "-c", "credential.helper=!gh auth git-credential", "push", "https://github.com/"+plan.Repo+".git", head+":refs/heads/"+branch); err != nil {
		return empty, fmt.Errorf("push verified Go candidate: %w", err)
	}
	marker := "<!-- depctl-go-toolchain:" + plan.Target + " -->"
	receipt, err := client.Publish(ctx, automation.PublicationRequest{
		HeadRepository: plan.Repo, HeadRef: branch, ExpectedHead: head, BaseRef: "main", ExpectedBase: plan.BaseSHA,
		Actor: actor, Marker: marker, Title: "fix(toolchain): bump Go to " + plan.Target,
		Body:          "### What\n\nBumps the Go compiler pins to " + plan.Target + ".\n\n### Testing\n\nThe candidate passes regeneration and repository checks with the target compiler.\n\n" + marker,
		ExpectedFiles: paths,
	})
	if err != nil {
		return receipt, fmt.Errorf("publish and verify draft: %w", err)
	}
	pull, _, err := api.PullRequests.Get(ctx, "Azure", "ARO-HCP", receipt.Number)
	if err != nil {
		return receipt, fmt.Errorf("check draft reviewers: %w: %w", automation.ErrIndeterminate, err)
	}
	if !pull.GetDraft() || len(pull.RequestedTeams) != 0 {
		return receipt, fmt.Errorf("published proposal is not an unassigned draft: %w", automation.ErrIndeterminate)
	}
	for _, reviewer := range pull.RequestedReviewers {
		if reviewer.GetType() != "Bot" {
			return receipt, fmt.Errorf("published draft has a human reviewer request: %w", automation.ErrIndeterminate)
		}
	}
	return receipt, nil
}
