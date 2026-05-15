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
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	workv1 "open-cluster-management.io/api/work/v1"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	arohcpv1alpha1 "github.com/openshift-online/ocm-sdk-go/arohcp/v1alpha1"

	"github.com/Azure/ARO-HCP/backend/pkg/controllers/controllerutils"
	"github.com/Azure/ARO-HCP/backend/pkg/listertesting"
	"github.com/Azure/ARO-HCP/backend/pkg/maestro"
	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/api/arm"
	"github.com/Azure/ARO-HCP/internal/databasetesting"
	"github.com/Azure/ARO-HCP/internal/ocm"
	"github.com/Azure/ARO-HCP/internal/utils"
)

func buildTestProvisionShard(maestroConsumerName string) *arohcpv1alpha1.ProvisionShard {
	provisionShard, err := arohcpv1alpha1.NewProvisionShard().
		ID("22222222222222222222222222222222").
		MaestroConfig(
			arohcpv1alpha1.NewProvisionShardMaestroConfig().
				ConsumerName(maestroConsumerName).
				RestApiConfig(
					arohcpv1alpha1.NewProvisionShardMaestroRestApiConfig().
						Url("https://maestro.example.com:443"),
				).
				GrpcApiConfig(
					arohcpv1alpha1.NewProvisionShardMaestroGrpcApiConfig().
						Url("https://maestro.example.com:444"),
				),
		).
		Build()
	if err != nil {
		panic(err)
	}
	return provisionShard
}

func newTestCluster(t *testing.T, csID *api.InternalID) *api.HCPOpenShiftCluster {
	t.Helper()
	resourceID := api.Must(azcorearm.ParseResourceID(
		"/subscriptions/" + testSubscriptionID +
			"/resourceGroups/" + testResourceGroupName +
			"/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/" + testClusterName))
	return &api.HCPOpenShiftCluster{
		TrackedResource: arm.TrackedResource{
			Resource: arm.Resource{
				ID:   resourceID,
				Name: testClusterName,
				Type: api.ClusterResourceType.String(),
			},
			Location: "eastus",
		},
		ServiceProviderProperties: api.HCPOpenShiftClusterServiceProviderProperties{
			ClusterServiceID: csID,
		},
	}
}

func newTestSPNP(t *testing.T, bundles api.MaestroBundleReferenceList) *api.ServiceProviderNodePool {
	t.Helper()
	spnpResourceID := api.Must(azcorearm.ParseResourceID(
		"/subscriptions/" + testSubscriptionID +
			"/resourceGroups/" + testResourceGroupName +
			"/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/" + testClusterName +
			"/nodePools/" + testNodePoolName +
			"/serviceProviderNodePools/default"))
	return &api.ServiceProviderNodePool{
		CosmosMetadata: arm.CosmosMetadata{ResourceID: spnpResourceID},
		Status: api.ServiceProviderNodePoolStatus{
			MaestroReadonlyBundles: bundles,
		},
	}
}

func TestNodePoolScopedMaestroReadonlyBundlesDeleteController_NeedsWork(t *testing.T) {
	fixedNow := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		nodePool *api.HCPOpenShiftClusterNodePool
		want     bool
	}{
		{
			name:     "all nil — false",
			nodePool: newTestNodePool(t, nil),
			want:     false,
		},
		{
			name: "DeletionTimestamp only — false",
			nodePool: newTestNodePool(t, func(np *api.HCPOpenShiftClusterNodePool) {
				np.ServiceProviderProperties.DeletionTimestamp = &metav1.Time{Time: fixedNow}
			}),
			want: false,
		},
		{
			name: "DeletionTimestamp + CSDeletionTimestamp but CSID set — false",
			nodePool: newTestNodePool(t, func(np *api.HCPOpenShiftClusterNodePool) {
				np.ServiceProviderProperties.DeletionTimestamp = &metav1.Time{Time: fixedNow}
				np.ServiceProviderProperties.ClusterServiceDeletionTimestamp = &metav1.Time{Time: fixedNow}
			}),
			want: false,
		},
		{
			name: "all conditions met — true",
			nodePool: newTestNodePool(t, func(np *api.HCPOpenShiftClusterNodePool) {
				np.ServiceProviderProperties.DeletionTimestamp = &metav1.Time{Time: fixedNow}
				np.ServiceProviderProperties.ClusterServiceDeletionTimestamp = &metav1.Time{Time: fixedNow}
				np.ServiceProviderProperties.ClusterServiceID = nil
			}),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nodePoolMarkedForDeletion(tt.nodePool)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNodePoolScopedMaestroReadonlyBundlesDeleteController_SyncOnce(t *testing.T) {
	fixedNow := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	readyToDelete := func(np *api.HCPOpenShiftClusterNodePool) {
		np.ServiceProviderProperties.DeletionTimestamp = &metav1.Time{Time: fixedNow.Add(-time.Hour)}
		np.ServiceProviderProperties.ClusterServiceDeletionTimestamp = &metav1.Time{Time: fixedNow.Add(-30 * time.Minute)}
		np.ServiceProviderProperties.ClusterServiceID = nil
	}
	clusterCSID := api.Ptr(api.Must(api.NewInternalID(testClusterServiceIDStr)))

	tests := []struct {
		name                   string
		existingNodePool       *api.HCPOpenShiftClusterNodePool
		existingCluster        *api.HCPOpenShiftCluster
		existingSPNP           *api.ServiceProviderNodePool
		setupMocks             func(*ocm.MockClusterServiceClientSpec, *maestro.MockMaestroClientBuilder, *maestro.MockClient)
		wantErr                bool
		wantErrSubstr          string
		wantRemainingBundles   int
		wantRemainingBundleRef *api.MaestroBundleInternalName
	}{
		{
			name: "nodepool not found — no-op",
		},
		{
			name:             "nodepool not marked for deletion — no-op",
			existingNodePool: newTestNodePool(t, nil),
		},
		{
			name:             "no SPNP — no-op",
			existingNodePool: newTestNodePool(t, readyToDelete),
		},
		{
			name:             "SPNP with empty bundle list — no-op",
			existingNodePool: newTestNodePool(t, readyToDelete),
			existingSPNP:     newTestSPNP(t, api.MaestroBundleReferenceList{}),
		},
		{
			name:             "parent cluster not found — defers to orphaned controller",
			existingNodePool: newTestNodePool(t, readyToDelete),
			existingSPNP: newTestSPNP(t, api.MaestroBundleReferenceList{
				{Name: "bundleA", MaestroAPIMaestroBundleName: "name-a", MaestroAPIMaestroBundleID: "id-a"},
			}),
			wantRemainingBundles: 1,
		},
		{
			name:             "parent cluster CSID nil — defers to orphaned controller",
			existingNodePool: newTestNodePool(t, readyToDelete),
			existingCluster:  newTestCluster(t, nil),
			existingSPNP: newTestSPNP(t, api.MaestroBundleReferenceList{
				{Name: "bundleA", MaestroAPIMaestroBundleName: "name-a", MaestroAPIMaestroBundleID: "id-a"},
			}),
			wantRemainingBundles: 1,
		},
		{
			name:             "single bundle — successful delete and confirmed gone",
			existingNodePool: newTestNodePool(t, readyToDelete),
			existingCluster:  newTestCluster(t, clusterCSID),
			existingSPNP: newTestSPNP(t, api.MaestroBundleReferenceList{
				{Name: "bundleA", MaestroAPIMaestroBundleName: "name-a", MaestroAPIMaestroBundleID: "id-a"},
			}),
			setupMocks: func(cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient) {
				provisionShard := buildTestProvisionShard("test-consumer")
				cs.EXPECT().GetClusterProvisionShard(gomock.Any(), gomock.Any()).Return(provisionShard, nil)
				mb.EXPECT().NewClient(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(mc, nil)
				mc.EXPECT().Delete(gomock.Any(), "name-a", metav1.DeleteOptions{}).Return(nil)
				mc.EXPECT().Get(gomock.Any(), "name-a", metav1.GetOptions{}).Return(nil,
					k8serrors.NewNotFound(schema.GroupResource{}, "name-a"))
			},
			wantRemainingBundles: 0,
		},
		{
			name:             "single bundle — delete ok but still exists in maestro, reference kept",
			existingNodePool: newTestNodePool(t, readyToDelete),
			existingCluster:  newTestCluster(t, clusterCSID),
			existingSPNP: newTestSPNP(t, api.MaestroBundleReferenceList{
				{Name: "bundleA", MaestroAPIMaestroBundleName: "name-a", MaestroAPIMaestroBundleID: "id-a"},
			}),
			setupMocks: func(cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient) {
				provisionShard := buildTestProvisionShard("test-consumer")
				cs.EXPECT().GetClusterProvisionShard(gomock.Any(), gomock.Any()).Return(provisionShard, nil)
				mb.EXPECT().NewClient(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(mc, nil)
				mc.EXPECT().Delete(gomock.Any(), "name-a", metav1.DeleteOptions{}).Return(nil)
				mc.EXPECT().Get(gomock.Any(), "name-a", metav1.GetOptions{}).Return(&workv1.ManifestWork{}, nil)
			},
			wantRemainingBundles: 1,
		},
		{
			name:             "single bundle — delete ok but Get returns error, reference kept",
			existingNodePool: newTestNodePool(t, readyToDelete),
			existingCluster:  newTestCluster(t, clusterCSID),
			existingSPNP: newTestSPNP(t, api.MaestroBundleReferenceList{
				{Name: "bundleA", MaestroAPIMaestroBundleName: "name-a", MaestroAPIMaestroBundleID: "id-a"},
			}),
			setupMocks: func(cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient) {
				provisionShard := buildTestProvisionShard("test-consumer")
				cs.EXPECT().GetClusterProvisionShard(gomock.Any(), gomock.Any()).Return(provisionShard, nil)
				mb.EXPECT().NewClient(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(mc, nil)
				mc.EXPECT().Delete(gomock.Any(), "name-a", metav1.DeleteOptions{}).Return(nil)
				mc.EXPECT().Get(gomock.Any(), "name-a", metav1.GetOptions{}).Return(nil, fmt.Errorf("maestro connection error"))
			},
			wantErr:              true,
			wantErrSubstr:        "failed to verify deletion of Maestro Bundle",
			wantRemainingBundles: 1,
		},
		{
			name:             "single bundle — maestro delete 404 then Get 404 treated as success",
			existingNodePool: newTestNodePool(t, readyToDelete),
			existingCluster:  newTestCluster(t, clusterCSID),
			existingSPNP: newTestSPNP(t, api.MaestroBundleReferenceList{
				{Name: "bundleA", MaestroAPIMaestroBundleName: "name-a", MaestroAPIMaestroBundleID: "id-a"},
			}),
			setupMocks: func(cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient) {
				provisionShard := buildTestProvisionShard("test-consumer")
				cs.EXPECT().GetClusterProvisionShard(gomock.Any(), gomock.Any()).Return(provisionShard, nil)
				mb.EXPECT().NewClient(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(mc, nil)
				mc.EXPECT().Delete(gomock.Any(), "name-a", metav1.DeleteOptions{}).Return(
					k8serrors.NewNotFound(schema.GroupResource{}, "name-a"))
				mc.EXPECT().Get(gomock.Any(), "name-a", metav1.GetOptions{}).Return(nil,
					k8serrors.NewNotFound(schema.GroupResource{}, "name-a"))
			},
			wantRemainingBundles: 0,
		},
		{
			name:             "single bundle — maestro error",
			existingNodePool: newTestNodePool(t, readyToDelete),
			existingCluster:  newTestCluster(t, clusterCSID),
			existingSPNP: newTestSPNP(t, api.MaestroBundleReferenceList{
				{Name: "bundleA", MaestroAPIMaestroBundleName: "name-a", MaestroAPIMaestroBundleID: "id-a"},
			}),
			setupMocks: func(cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient) {
				provisionShard := buildTestProvisionShard("test-consumer")
				cs.EXPECT().GetClusterProvisionShard(gomock.Any(), gomock.Any()).Return(provisionShard, nil)
				mb.EXPECT().NewClient(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(mc, nil)
				mc.EXPECT().Delete(gomock.Any(), "name-a", metav1.DeleteOptions{}).Return(fmt.Errorf("maestro connection error"))
			},
			wantErr:              true,
			wantErrSubstr:        "failed to delete Maestro Bundle",
			wantRemainingBundles: 1,
		},
		{
			name:             "multiple bundles — all succeed",
			existingNodePool: newTestNodePool(t, readyToDelete),
			existingCluster:  newTestCluster(t, clusterCSID),
			existingSPNP: newTestSPNP(t, api.MaestroBundleReferenceList{
				{Name: "bundleA", MaestroAPIMaestroBundleName: "name-a", MaestroAPIMaestroBundleID: "id-a"},
				{Name: "bundleB", MaestroAPIMaestroBundleName: "name-b", MaestroAPIMaestroBundleID: "id-b"},
			}),
			setupMocks: func(cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient) {
				provisionShard := buildTestProvisionShard("test-consumer")
				cs.EXPECT().GetClusterProvisionShard(gomock.Any(), gomock.Any()).Return(provisionShard, nil)
				mb.EXPECT().NewClient(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(mc, nil)
				mc.EXPECT().Delete(gomock.Any(), "name-a", metav1.DeleteOptions{}).Return(nil)
				mc.EXPECT().Get(gomock.Any(), "name-a", metav1.GetOptions{}).Return(nil,
					k8serrors.NewNotFound(schema.GroupResource{}, "name-a"))
				mc.EXPECT().Delete(gomock.Any(), "name-b", metav1.DeleteOptions{}).Return(nil)
				mc.EXPECT().Get(gomock.Any(), "name-b", metav1.GetOptions{}).Return(nil,
					k8serrors.NewNotFound(schema.GroupResource{}, "name-b"))
			},
			wantRemainingBundles: 0,
		},
		{
			name:             "multiple bundles — second delete fails",
			existingNodePool: newTestNodePool(t, readyToDelete),
			existingCluster:  newTestCluster(t, clusterCSID),
			existingSPNP: newTestSPNP(t, api.MaestroBundleReferenceList{
				{Name: "bundleA", MaestroAPIMaestroBundleName: "name-a", MaestroAPIMaestroBundleID: "id-a"},
				{Name: "bundleB", MaestroAPIMaestroBundleName: "name-b", MaestroAPIMaestroBundleID: "id-b"},
			}),
			setupMocks: func(cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient) {
				provisionShard := buildTestProvisionShard("test-consumer")
				cs.EXPECT().GetClusterProvisionShard(gomock.Any(), gomock.Any()).Return(provisionShard, nil)
				mb.EXPECT().NewClient(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(mc, nil)
				mc.EXPECT().Delete(gomock.Any(), "name-a", metav1.DeleteOptions{}).Return(nil)
				mc.EXPECT().Get(gomock.Any(), "name-a", metav1.GetOptions{}).Return(nil,
					k8serrors.NewNotFound(schema.GroupResource{}, "name-a"))
				mc.EXPECT().Delete(gomock.Any(), "name-b", metav1.DeleteOptions{}).Return(fmt.Errorf("maestro error"))
			},
			wantErr:                true,
			wantErrSubstr:          "failed to delete Maestro Bundle",
			wantRemainingBundles:   1,
			wantRemainingBundleRef: ptrTo(api.MaestroBundleInternalName("bundleB")),
		},
		{
			name:             "multiple bundles — first still exists after delete",
			existingNodePool: newTestNodePool(t, readyToDelete),
			existingCluster:  newTestCluster(t, clusterCSID),
			existingSPNP: newTestSPNP(t, api.MaestroBundleReferenceList{
				{Name: "bundleA", MaestroAPIMaestroBundleName: "name-a", MaestroAPIMaestroBundleID: "id-a"},
				{Name: "bundleB", MaestroAPIMaestroBundleName: "name-b", MaestroAPIMaestroBundleID: "id-b"},
			}),
			setupMocks: func(cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient) {
				provisionShard := buildTestProvisionShard("test-consumer")
				cs.EXPECT().GetClusterProvisionShard(gomock.Any(), gomock.Any()).Return(provisionShard, nil)
				mb.EXPECT().NewClient(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(mc, nil)
				mc.EXPECT().Delete(gomock.Any(), "name-a", metav1.DeleteOptions{}).Return(nil)
				mc.EXPECT().Get(gomock.Any(), "name-a", metav1.GetOptions{}).Return(&workv1.ManifestWork{}, nil)
				mc.EXPECT().Delete(gomock.Any(), "name-b", metav1.DeleteOptions{}).Return(nil)
				mc.EXPECT().Get(gomock.Any(), "name-b", metav1.GetOptions{}).Return(nil,
					k8serrors.NewNotFound(schema.GroupResource{}, "name-b"))
			},
			wantRemainingBundles:   1,
			wantRemainingBundleRef: ptrTo(api.MaestroBundleInternalName("bundleA")),
		},
		{
			name:             "bundle with empty maestro name — removed without maestro call",
			existingNodePool: newTestNodePool(t, readyToDelete),
			existingCluster:  newTestCluster(t, clusterCSID),
			existingSPNP: newTestSPNP(t, api.MaestroBundleReferenceList{
				{Name: "bundleA", MaestroAPIMaestroBundleName: "", MaestroAPIMaestroBundleID: ""},
			}),
			setupMocks: func(cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient) {
				provisionShard := buildTestProvisionShard("test-consumer")
				cs.EXPECT().GetClusterProvisionShard(gomock.Any(), gomock.Any()).Return(provisionShard, nil)
				mb.EXPECT().NewClient(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(mc, nil)
			},
			wantRemainingBundles: 0,
		},
		{
			name:             "provision shard fetch fails",
			existingNodePool: newTestNodePool(t, readyToDelete),
			existingCluster:  newTestCluster(t, clusterCSID),
			existingSPNP: newTestSPNP(t, api.MaestroBundleReferenceList{
				{Name: "bundleA", MaestroAPIMaestroBundleName: "name-a", MaestroAPIMaestroBundleID: "id-a"},
			}),
			setupMocks: func(cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient) {
				cs.EXPECT().GetClusterProvisionShard(gomock.Any(), gomock.Any()).Return(nil, fmt.Errorf("CS error"))
			},
			wantErr:              true,
			wantErrSubstr:        "failed to get Cluster Provision Shard",
			wantRemainingBundles: 1,
		},
		{
			name:             "maestro client creation fails",
			existingNodePool: newTestNodePool(t, readyToDelete),
			existingCluster:  newTestCluster(t, clusterCSID),
			existingSPNP: newTestSPNP(t, api.MaestroBundleReferenceList{
				{Name: "bundleA", MaestroAPIMaestroBundleName: "name-a", MaestroAPIMaestroBundleID: "id-a"},
			}),
			setupMocks: func(cs *ocm.MockClusterServiceClientSpec, mb *maestro.MockMaestroClientBuilder, mc *maestro.MockClient) {
				provisionShard := buildTestProvisionShard("test-consumer")
				cs.EXPECT().GetClusterProvisionShard(gomock.Any(), gomock.Any()).Return(provisionShard, nil)
				mb.EXPECT().NewClient(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, fmt.Errorf("client error"))
			},
			wantErr:              true,
			wantErrSubstr:        "failed to create Maestro client",
			wantRemainingBundles: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := utils.ContextWithLogger(context.Background(), testr.New(t))
			ctrl := gomock.NewController(t)
			mockCS := ocm.NewMockClusterServiceClientSpec(ctrl)
			mockMaestroBuilder := maestro.NewMockMaestroClientBuilder(ctrl)
			mockMaestroClient := maestro.NewMockClient(ctrl)

			if tt.setupMocks != nil {
				tt.setupMocks(mockCS, mockMaestroBuilder, mockMaestroClient)
			}

			resources := []any{}
			if tt.existingNodePool != nil {
				resources = append(resources, tt.existingNodePool)
			}
			if tt.existingCluster != nil {
				resources = append(resources, tt.existingCluster)
			}
			if tt.existingSPNP != nil {
				resources = append(resources, tt.existingSPNP)
			}
			mockResourcesDBClient, err := databasetesting.NewMockResourcesDBClientWithResources(ctx, resources)
			require.NoError(t, err)

			nodePoolsForLister := []*api.HCPOpenShiftClusterNodePool{}
			if tt.existingNodePool != nil {
				nodePoolsForLister = append(nodePoolsForLister, tt.existingNodePool)
			}
			spnpForLister := []*api.ServiceProviderNodePool{}
			if tt.existingSPNP != nil {
				spnpForLister = append(spnpForLister, tt.existingSPNP)
			}

			syncer := &nodePoolScopedMaestroReadonlyBundlesDeleteController{
				cooldownChecker:                    &alwaysSyncCooldownChecker{},
				nodePoolLister:                     &listertesting.SliceNodePoolLister{NodePools: nodePoolsForLister},
				serviceProviderNodePoolLister:      &listertesting.SliceServiceProviderNodePoolLister{ServiceProviderNodePools: spnpForLister},
				resourcesDBClient:                  mockResourcesDBClient,
				clusterServiceClient:               mockCS,
				maestroSourceEnvironmentIdentifier: "test-env",
				maestroClientBuilder:               mockMaestroBuilder,
			}

			key := controllerutils.HCPNodePoolKey{
				SubscriptionID:    testSubscriptionID,
				ResourceGroupName: testResourceGroupName,
				HCPClusterName:    testClusterName,
				HCPNodePoolName:   testNodePoolName,
			}

			err = syncer.SyncOnce(ctx, key)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrSubstr)
			} else {
				require.NoError(t, err)
			}

			if tt.existingSPNP != nil {
				spnpCRUD := mockResourcesDBClient.ServiceProviderNodePools(testSubscriptionID, testResourceGroupName, testClusterName, testNodePoolName)
				updatedSPNP, err := spnpCRUD.Get(ctx, api.ServiceProviderNodePoolResourceName)
				require.NoError(t, err)
				assert.Len(t, updatedSPNP.Status.MaestroReadonlyBundles, tt.wantRemainingBundles)

				if tt.wantRemainingBundleRef != nil {
					ref, err := updatedSPNP.Status.MaestroReadonlyBundles.Get(*tt.wantRemainingBundleRef)
					require.NoError(t, err)
					assert.NotNil(t, ref)
				}
			}
		})
	}
}

func ptrTo[T any](v T) *T {
	return &v
}
