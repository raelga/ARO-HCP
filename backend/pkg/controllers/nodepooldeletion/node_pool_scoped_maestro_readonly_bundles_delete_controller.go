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

package nodepooldeletion

import (
	"context"
	"errors"
	"fmt"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/ARO-HCP/backend/pkg/controllers"
	"github.com/Azure/ARO-HCP/backend/pkg/controllers/controllerutils"
	"github.com/Azure/ARO-HCP/backend/pkg/informers"
	"github.com/Azure/ARO-HCP/backend/pkg/listers"
	"github.com/Azure/ARO-HCP/backend/pkg/maestro"
	"github.com/Azure/ARO-HCP/internal/api"
	controllerutil "github.com/Azure/ARO-HCP/internal/controllerutils"
	"github.com/Azure/ARO-HCP/internal/database"
	"github.com/Azure/ARO-HCP/internal/ocm"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// nodePoolScopedMaestroReadonlyBundlesDeleteController deletes Maestro
// readonly bundles that were created for a NodePool when the NodePool is
// being deleted. It runs after ClusterServiceID has been cleared and
// removes each bundle from the Maestro API, then clears the
// corresponding reference from the ServiceProviderNodePool in Cosmos.
type nodePoolScopedMaestroReadonlyBundlesDeleteController struct {
	cooldownChecker                    controllerutil.CooldownChecker
	nodePoolLister                     listers.NodePoolLister
	serviceProviderNodePoolLister      listers.ServiceProviderNodePoolLister
	resourcesDBClient                  database.ResourcesDBClient
	clusterServiceClient               ocm.ClusterServiceClientSpec
	maestroSourceEnvironmentIdentifier string
	maestroClientBuilder               maestro.MaestroClientBuilder
}

var _ controllerutils.NodePoolSyncer = (*nodePoolScopedMaestroReadonlyBundlesDeleteController)(nil)

func NewNodePoolScopedMaestroReadonlyBundlesDeleteController(
	resourcesDBClient database.ResourcesDBClient,
	clusterServiceClient ocm.ClusterServiceClientSpec,
	activeOperationLister listers.ActiveOperationLister,
	informers informers.BackendInformers,
	maestroSourceEnvironmentIdentifier string,
	maestroClientBuilder maestro.MaestroClientBuilder,
) controllerutils.Controller {
	_, nodePoolLister := informers.NodePools()
	_, serviceProviderNodePoolLister := informers.ServiceProviderNodePools()
	syncer := &nodePoolScopedMaestroReadonlyBundlesDeleteController{
		cooldownChecker:                    controllerutils.DefaultActiveOperationPrioritizingCooldown(activeOperationLister),
		nodePoolLister:                     nodePoolLister,
		serviceProviderNodePoolLister:      serviceProviderNodePoolLister,
		resourcesDBClient:                  resourcesDBClient,
		clusterServiceClient:               clusterServiceClient,
		maestroSourceEnvironmentIdentifier: maestroSourceEnvironmentIdentifier,
		maestroClientBuilder:               maestroClientBuilder,
	}

	return controllerutils.NewNodePoolWatchingController(
		"NodePoolScopedMaestroReadonlyBundlesDelete",
		resourcesDBClient,
		informers,
		time.Minute,
		syncer,
	)
}

func (c *nodePoolScopedMaestroReadonlyBundlesDeleteController) CooldownChecker() controllerutil.CooldownChecker {
	return c.cooldownChecker
}

// nodePoolMarkedForDeletion checks if the NodePool has been marked for deletion
// and the ClusterServiceID has been cleared. These are the preconditions for
// this controller to act.
func nodePoolMarkedForDeletion(nodePool *api.HCPOpenShiftClusterNodePool) bool {
	return nodePool.ServiceProviderProperties.DeletionTimestamp != nil &&
		nodePool.ServiceProviderProperties.ClusterServiceDeletionTimestamp != nil &&
		nodePool.ServiceProviderProperties.ClusterServiceID == nil
}

func (c *nodePoolScopedMaestroReadonlyBundlesDeleteController) SyncOnce(ctx context.Context, key controllerutils.HCPNodePoolKey) error {
	logger := utils.LoggerFromContext(ctx)

	cachedNodePool, err := c.nodePoolLister.Get(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName, key.HCPNodePoolName)
	if database.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get node pool from cache: %w", err))
	}
	if !nodePoolMarkedForDeletion(cachedNodePool) {
		return nil
	}

	cachedSPNP, err := c.serviceProviderNodePoolLister.Get(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName, key.HCPNodePoolName)
	if database.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get ServiceProviderNodePool from cache: %w", err))
	}
	if len(cachedSPNP.Status.MaestroReadonlyBundles) == 0 {
		return nil
	}

	nodePoolCRUD := c.resourcesDBClient.HCPClusters(key.SubscriptionID, key.ResourceGroupName).NodePools(key.HCPClusterName)
	nodePool, err := nodePoolCRUD.Get(ctx, key.HCPNodePoolName)
	if database.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get node pool: %w", err))
	}
	if !nodePoolMarkedForDeletion(nodePool) {
		return nil
	}

	spnpCRUD := c.resourcesDBClient.ServiceProviderNodePools(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName, key.HCPNodePoolName)
	spnp, err := spnpCRUD.Get(ctx, api.ServiceProviderNodePoolResourceName)
	if database.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get ServiceProviderNodePool: %w", err))
	}
	if len(spnp.Status.MaestroReadonlyBundles) == 0 {
		return nil
	}

	parentCluster, err := c.resourcesDBClient.HCPClusters(key.SubscriptionID, key.ResourceGroupName).Get(ctx, key.HCPClusterName)
	if database.IsNotFoundError(err) {
		logger.Info("parent cluster not found, deferring maestro bundle cleanup to orphaned bundles controller")
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get parent cluster: %w", err))
	}
	if parentCluster.ServiceProviderProperties.ClusterServiceID == nil || len(parentCluster.ServiceProviderProperties.ClusterServiceID.String()) == 0 {
		logger.Info("parent cluster ClusterServiceID is nil, deferring maestro bundle cleanup to orphaned bundles controller")
		return nil
	}

	// TODO We need the provision shard info of the provision shard assigned to the cluster
	// to instantiate the maestro clients. Right now the wayt it's done is by querying the the cluster scoped
	// provisionshard endpoint from CS. This is a problem because it depends on both having
	// the ClusterServiceID of the nodepool or cluster, as well as the cluster actually not having been deleted on CS side.
	// It's an issue because when the controller that issues delete occurs beforehand as well as the csid delete controller
	// which means this won't have the opportunity to run.
	// The options I can think of are:
	// * We delete the bundles before issuing CS delete call. The CS delete call controller waits until there are no SPNPs.
	//   However that means other controllers that depend on them might start erroring. Also if in the future it's
	//   something else than maestro readonly bundles that might be problematic to be deleted before CS side is gone. It
	//   might also be surfaced as hotlooping on some other controllers maybe too.
	// * before performing the delete call on CS in the same controller we persist to a new attribute in cosmos
	//   in the nodepool object (or spnp?) the provisionshard of the cluster. attribut named like
	//   csprovisionshardfordeletion or something. However this means the cluster from cs side cannot be deleted either as
	//   we depend on that
	// * We wait until the pieces that put provisionshards information land in the RP. This will avoid needing to go
	//   to CS and therefore needing the ClusterServiceID reference. However, it will require not deleting the cosmos cluster
	//   doc as we would depend on having it there.
	// * We store a new attribute in Cluster that contains the CS provision shard id so it can query directly the CS
	//   provision shard api. This would require migrating existing clusters to have that entry. It also means that we
	//   should block cluster cosmos entry deletion until all child nodepools are deleted because otherwise we wouldn't
	//   be able to access the provision shard info. We could also store it in nodepool but then the info is in two
	//   places and if it needs to be changed at some point (support changing of mgmt cluster?) it would need to be changed
	//   in the N places.
	parentClusterCSID := parentCluster.ServiceProviderProperties.ClusterServiceID
	if parentClusterCSID == nil {
		// TODO decide what to do if this occurs. Should we return error, nil, or unset all the spnp.status.maestroreadonlybundles attribute from cosmos
		return utils.TrackError(err)
	}
	parentClusterCSInternalID := api.Must(api.NewInternalID(parentClusterCSID.String()))
	clusterProvisionShard, err := c.clusterServiceClient.GetClusterProvisionShard(ctx, parentClusterCSInternalID)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get Cluster Provision Shard from Cluster Service: %w", err))
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	maestroClient, err := controllers.CreateMaestroClientFromCSProvisionShard(ctx, c.maestroSourceEnvironmentIdentifier, c.maestroClientBuilder, clusterProvisionShard)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to create Maestro client: %w", err))
	}

	bundleRefs := make([]*api.MaestroBundleReference, len(spnp.Status.MaestroReadonlyBundles))
	for i, ref := range spnp.Status.MaestroReadonlyBundles {
		bundleRefs[i] = ref.DeepCopy()
	}

	var syncErrors []error
	var bundlesToRemove []api.MaestroBundleInternalName
	for _, ref := range bundleRefs {
		if len(ref.MaestroAPIMaestroBundleName) == 0 {
			logger.Info("skipping bundle reference with empty Maestro API name", "bundleInternalName", ref.Name)
			bundlesToRemove = append(bundlesToRemove, ref.Name)
			continue
		}

		logger.Info("sending Maestro readonly bundle delete", "bundleInternalName", ref.Name, "maestroAPIMaestroBundleName", ref.MaestroAPIMaestroBundleName)
		err := maestroClient.Delete(ctx, ref.MaestroAPIMaestroBundleName, metav1.DeleteOptions{})
		if err != nil && !k8serrors.IsNotFound(err) {
			syncErrors = append(syncErrors, utils.TrackError(fmt.Errorf("failed to delete Maestro Bundle %q: %w", ref.MaestroAPIMaestroBundleName, err)))
			continue
		}

		_, err = maestroClient.Get(ctx, ref.MaestroAPIMaestroBundleName, metav1.GetOptions{})
		if err != nil && !k8serrors.IsNotFound(err) {
			syncErrors = append(syncErrors, utils.TrackError(fmt.Errorf("failed to verify deletion of Maestro Bundle %q: %w", ref.MaestroAPIMaestroBundleName, err)))
			continue
		}
		if err == nil {
			logger.Info("Maestro readonly bundle still exists after delete, will retry", "bundleInternalName", ref.Name, "maestroAPIMaestroBundleName", ref.MaestroAPIMaestroBundleName)
			continue
		}

		logger.Info("deleted Maestro readonly bundle", "bundleInternalName", ref.Name, "maestroAPIMaestroBundleName", ref.MaestroAPIMaestroBundleName)
		bundlesToRemove = append(bundlesToRemove, ref.Name)
	}

	if len(bundlesToRemove) > 0 {
		for _, name := range bundlesToRemove {
			if err := spnp.Status.MaestroReadonlyBundles.Remove(name); err != nil {
				syncErrors = append(syncErrors, utils.TrackError(fmt.Errorf("failed to remove bundle reference %q: %w", name, err)))
			}
		}
		_, err = spnpCRUD.Replace(ctx, spnp, nil)
		if err != nil {
			syncErrors = append(syncErrors, utils.TrackError(fmt.Errorf("failed to persist ServiceProviderNodePool after deleting bundles: %w", err)))
		}
	}

	return utils.TrackError(errors.Join(syncErrors...))
}
