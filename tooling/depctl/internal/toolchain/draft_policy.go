// Copyright 2026 Microsoft Corporation
// Licensed under the Apache License, Version 2.0.

package toolchain

import (
	"context"
	"fmt"

	"go.yaml.in/yaml/v3"
)

// DraftNotificationsSafe verifies that Prow will not request human reviews for drafts.
func draftNotificationsSafe(ctx context.Context) error {
	const repo = "openshift/release"
	sha, err := defaultHead(ctx, repo, readGitHub)
	if err != nil {
		return err
	}
	for i, path := range []string{"core-services/prow/02_config/_plugins.yaml", "core-services/prow/02_config/Azure/_pluginconfig.yaml", "core-services/prow/02_config/Azure/ARO-HCP/_pluginconfig.yaml"} {
		body, err := readGitHubFile(ctx, readGitHub, repo, path, sha)
		if err != nil {
			return err
		}
		var config map[string]any
		if err := yaml.Unmarshal([]byte(body), &config); err != nil {
			return fmt.Errorf("decode Prow configuration %s: %w", path, err)
		}
		if i == 0 {
			blunderbuss, _ := config["blunderbuss"].(map[string]any)
			if blunderbuss["ignore_drafts"] != true {
				return blockedf("Prow does not prove draft reviewer requests are disabled")
			}
			continue
		}
		if containsBlunderbuss(config) {
			return blockedf("Prow configuration %s overrides blunderbuss", path)
		}
	}
	return nil
}

// ContainsBlunderbuss detects nested overrides of the global reviewer policy.
func containsBlunderbuss(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		if _, ok := value["blunderbuss"]; ok {
			return true
		}
		for _, child := range value {
			if containsBlunderbuss(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if containsBlunderbuss(child) {
				return true
			}
		}
	}
	return false
}
