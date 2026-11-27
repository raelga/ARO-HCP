// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.

package toolchain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Azure/ARO-HCP/tooling/depctl/internal/toolchain"
)

type rawPublication struct{ root, plan string }
type validatedPublication struct {
	root, plan string
	publish    bool
}
type publication struct {
	root                  string
	plan                  toolchain.Plan
	publish               bool
	output                io.Writer
	readToken, writeToken string
}

// RegisterPublication attaches recheck and draft publication to the toolchain group.
func RegisterPublication(parent *cobra.Command) {
	for _, name := range []string{"recheck", "publish"} {
		raw := rawPublication{}
		cmd := &cobra.Command{Use: name, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			validated, err := raw.Validate(name == "publish")
			if err != nil {
				return err
			}
			completed, err := validated.Complete(cmd.OutOrStdout())
			if err != nil {
				return err
			}
			return completed.Run(cmd.Context())
		}}
		cmd.Flags().StringVar(&raw.root, "root", ".", "trusted candidate checkout")
		cmd.Flags().StringVar(&raw.plan, "plan", "", "trusted readiness artifact")
		parent.AddCommand(cmd)
	}
}

// Validate checks required paths before loading credentials or artifacts.
func (r rawPublication) Validate(publish bool) (validatedPublication, error) {
	if r.root == "" || r.plan == "" {
		return validatedPublication{}, fmt.Errorf("checkout root and readiness artifact are required")
	}
	return validatedPublication{root: r.root, plan: r.plan, publish: publish}, nil
}

// Complete opens the trusted plan and binds publication credentials only for the write command.
func (v validatedPublication) Complete(output io.Writer) (publication, error) {
	root, err := filepath.Abs(v.root)
	if err != nil {
		return publication{}, fmt.Errorf("resolve candidate checkout: %w", err)
	}
	plan, err := toolchain.ReadPublicationPlan(v.plan)
	if err != nil {
		return publication{}, err
	}
	result := publication{root: root, plan: plan, publish: v.publish, output: output}
	if v.publish {
		result.readToken, result.writeToken = os.Getenv("CI_TOKEN"), os.Getenv("GH_TOKEN")
	}
	return result, nil
}

// Run reports normal readiness blockers without opening an invalid draft.
func (o publication) Run(ctx context.Context) error {
	if o.publish {
		receipt, err := toolchain.PublishDraft(ctx, o.root, o.plan, o.readToken, o.writeToken)
		if err != nil {
			return fmt.Errorf("publish Go toolchain draft: %w", err)
		}
		return json.NewEncoder(o.output).Encode(receipt)
	}
	err := toolchain.RecheckPublication(ctx, o.root, o.plan)
	if err != nil && !errors.Is(err, toolchain.ErrBlocked) {
		return err
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	_, err = fmt.Fprintf(o.output, "ready=%t\n", err == nil)
	return err
}
