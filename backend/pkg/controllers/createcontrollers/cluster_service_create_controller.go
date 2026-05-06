// Copyright 2026 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package createcontrollers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/blang/semver/v4"
	arohcpv1alpha1 "github.com/openshift-online/ocm-sdk-go/arohcp/v1alpha1"

	"github.com/Azure/ARO-HCP/backend/pkg/controllers/controllerutils"
	"github.com/Azure/ARO-HCP/backend/pkg/informers"
	"github.com/Azure/ARO-HCP/backend/pkg/listers"
	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/database"
	"github.com/Azure/ARO-HCP/internal/ocm"
	"github.com/Azure/ARO-HCP/internal/utils"
)

type clusterServiceCreateClusterSyncer struct {
	cooldownChecker      controllerutils.CooldownChecker
	cosmosClient         database.DBClient
	clusterServiceClient ocm.ClusterServiceClientSpec
}

var _ controllerutils.ClusterSyncer = (*clusterServiceCreateClusterSyncer)(nil)

// NewClusterServiceCreateClusterController creates a controller that registers clusters
// with Cluster Service once their desired control plane version is computed.
func NewClusterServiceCreateClusterController(
	cosmosClient database.DBClient,
	clusterServiceClient ocm.ClusterServiceClientSpec,
	activeOperationLister listers.ActiveOperationLister,
	informers informers.BackendInformers,
) controllerutils.Controller {
	syncer := &clusterServiceCreateClusterSyncer{
		cooldownChecker:      controllerutils.DefaultActiveOperationPrioritizingCooldown(activeOperationLister),
		cosmosClient:         cosmosClient,
		clusterServiceClient: clusterServiceClient,
	}

	controller := controllerutils.NewClusterWatchingController(
		"ClusterServiceCreateCluster",
		cosmosClient,
		informers,
		30*time.Second,
		syncer,
	)

	return controller
}

func (c *clusterServiceCreateClusterSyncer) CooldownChecker() controllerutils.CooldownChecker {
	return c.cooldownChecker
}

func (c *clusterServiceCreateClusterSyncer) SyncOnce(ctx context.Context, key controllerutils.HCPClusterKey) error {
	logger := utils.LoggerFromContext(ctx)

	cluster, err := c.cosmosClient.HCPClusters(key.SubscriptionID, key.ResourceGroupName).Get(ctx, key.HCPClusterName)
	if database.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get Cluster: %w", err))
	}

	csInternalIDFromCluster := cluster.ServiceProviderProperties.ClusterServiceID
	if csInternalIDFromCluster != nil && len(csInternalIDFromCluster.String()) > 0 {
		// ClusterServiceID already exists, no work to do
		return nil
	}

	existingServiceProviderCluster, err := database.GetOrCreateServiceProviderCluster(ctx, c.cosmosClient, key.GetResourceID())
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get or create ServiceProviderCluster: %w", err))
	}

	if existingServiceProviderCluster.Spec.ControlPlaneVersion.DesiredVersion == nil {
		logger.Info("DesiredVersion not yet set, waiting for version controller")
		return nil
	}

	// Search for an existing CS cluster that matches this Azure resource.
	// This handles the case where CS creation succeeded but we failed to
	// persist the CS ID in Cosmos.
	existingClusterServiceCluster, err := c.findExistingClusterServiceCluster(ctx, cluster)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to search for existing CS cluster: %w", err))
	}

	if existingClusterServiceCluster == nil {
		existingClusterServiceCluster, err = c.createClusterServiceCluster(ctx, cluster, existingServiceProviderCluster.Spec.ControlPlaneVersion.DesiredVersion)
		if err != nil {
			return utils.TrackError(fmt.Errorf("failed to create cluster in CS: %w", err))
		}
	}

	csInternalID, err := api.NewInternalID(existingClusterServiceCluster.HREF())
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to parse CS cluster HREF: %w", err))
	}

	logger.Info("Storing ClusterServiceID on cluster document", "clusterServiceID", csInternalID.String())
	cluster.ServiceProviderProperties.ClusterServiceID = &csInternalID
	_, err = c.cosmosClient.HCPClusters(key.SubscriptionID, key.ResourceGroupName).Replace(ctx, cluster, nil)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to update cluster with ClusterServiceID: %w", err))
	}

	return nil
}

func (c *clusterServiceCreateClusterSyncer) findExistingClusterServiceCluster(ctx context.Context, cluster *api.HCPOpenShiftCluster) (*arohcpv1alpha1.Cluster, error) {
	searchExpression := fmt.Sprintf(
		"azure.subscription_id = '%s' and azure.resource_group_name = '%s' and azure.resource_name = '%s'",
		strings.ToLower(cluster.ID.SubscriptionID),
		strings.ToLower(cluster.ID.ResourceGroupName),
		strings.ToLower(cluster.Name),
	)

	matches, err := c.csClustersMatchingClusterByAzureInfo(ctx, c.clusterServiceClient.ListClusters(searchExpression))
	if err != nil {
		return nil, err
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf(
			"cluster service returned %d clusters for one Azure resource (expected exactly 1): "+
				"subscription_id=%q resource_group=%q resource_name=%q",
			len(matches), cluster.ID.SubscriptionID, cluster.ID.ResourceGroupName, cluster.Name,
		)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return nil, nil
}

func (c *clusterServiceCreateClusterSyncer) csClustersMatchingClusterByAzureInfo(ctx context.Context, iter ocm.ClusterListIterator) ([]*arohcpv1alpha1.Cluster, error) {
	var res []*arohcpv1alpha1.Cluster
	for csCluster := range iter.Items(ctx) {
		az := csCluster.Azure()
		if az == nil {
			continue
		}
		res = append(res, csCluster)
	}
	if err := iter.GetError(); err != nil {
		return nil, err
	}
	return res, nil
}

func (c *clusterServiceCreateClusterSyncer) createClusterServiceCluster(ctx context.Context, cluster *api.HCPOpenShiftCluster, desiredVersion *semver.Version) (*arohcpv1alpha1.Cluster, error) {
	logger := utils.LoggerFromContext(ctx)

	subscription, err := c.cosmosClient.Subscriptions().Get(ctx, cluster.ID.SubscriptionID)
	if err != nil {
		return nil, utils.TrackError(fmt.Errorf("failed to get subscription: %w", err))
	}

	var tenantID string
	if subscription.Properties != nil && subscription.Properties.TenantId != nil {
		tenantID = *subscription.Properties.TenantId
	}

	// Use the Cincinnati-resolved desired version instead of the
	// customer's minor version so CS gets the exact patch release.
	clusterCopy := *cluster
	clusterCopy.CustomerProperties.Version.ID = desiredVersion.String()

	csClusterBuilder, csAutoscalerBuilder, err := ocm.BuildCSCluster(
		clusterCopy.ID, tenantID, &clusterCopy, nil, nil,
	)
	if err != nil {
		return nil, utils.TrackError(fmt.Errorf("failed to build CS cluster: %w", err))
	}

	logger.Info("Creating cluster in Cluster Service", "version", desiredVersion.String())
	result, err := c.clusterServiceClient.PostCluster(ctx, csClusterBuilder, csAutoscalerBuilder)
	if err != nil {
		return nil, utils.TrackError(fmt.Errorf("PostCluster failed: %w", err))
	}

	return result, nil
}
