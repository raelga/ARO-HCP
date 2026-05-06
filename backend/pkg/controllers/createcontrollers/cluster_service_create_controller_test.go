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
	"testing"

	"github.com/blang/semver/v4"
	"github.com/go-logr/logr/testr"
	arohcpv1alpha1 "github.com/openshift-online/ocm-sdk-go/arohcp/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"k8s.io/utils/ptr"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	"github.com/Azure/ARO-HCP/backend/pkg/controllers/controllerutils"
	"github.com/Azure/ARO-HCP/internal/api"
	"github.com/Azure/ARO-HCP/internal/api/arm"
	"github.com/Azure/ARO-HCP/internal/databasetesting"
	"github.com/Azure/ARO-HCP/internal/ocm"
	"github.com/Azure/ARO-HCP/internal/utils"
)

const (
	testSubscriptionID    = "00000000-0000-0000-0000-000000000001"
	testResourceGroupName = "test-rg"
	testClusterName       = "test-cluster"
	testCSClusterHREF     = "/api/clusters_mgmt/v1/clusters/abc123"
	testTenantID          = "00000000-0000-0000-0000-000000000099"
)

type alwaysSyncCooldownChecker struct{}

func (c *alwaysSyncCooldownChecker) CanSync(ctx context.Context, key any) bool {
	return true
}

var _ controllerutils.CooldownChecker = &alwaysSyncCooldownChecker{}

type testFixture struct {
	clusterResourceID *azcorearm.ResourceID
}

func newTestFixture() *testFixture {
	return &testFixture{
		clusterResourceID: api.Must(azcorearm.ParseResourceID(
			"/subscriptions/" + testSubscriptionID +
				"/resourceGroups/" + testResourceGroupName +
				"/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/" + testClusterName,
		)),
	}
}

func (f *testFixture) clusterKey() controllerutils.HCPClusterKey {
	return controllerutils.HCPClusterKey{
		SubscriptionID:    testSubscriptionID,
		ResourceGroupName: testResourceGroupName,
		HCPClusterName:    testClusterName,
	}
}

func (f *testFixture) newCluster() *api.HCPOpenShiftCluster {
	cluster := api.MinimumValidClusterTestCase()
	cluster.ID = f.clusterResourceID
	cluster.Name = testClusterName
	cluster.Type = f.clusterResourceID.ResourceType.String()
	cluster.ServiceProviderProperties.ClusterServiceID = nil
	return cluster
}

func (f *testFixture) newSubscription() *arm.Subscription {
	subResourceID := api.Must(azcorearm.ParseResourceID("/subscriptions/" + testSubscriptionID))
	return &arm.Subscription{
		CosmosMetadata: arm.CosmosMetadata{
			ResourceID: subResourceID,
		},
		ResourceID: subResourceID,
		State:      arm.SubscriptionStateRegistered,
		Properties: &arm.SubscriptionProperties{
			TenantId: ptr.To(testTenantID),
		},
	}
}

func (f *testFixture) newServiceProviderCluster(desiredVersion *semver.Version) *api.ServiceProviderCluster {
	clusterResourceIDStr := f.clusterResourceID.String()
	spClusterResourceID := clusterResourceIDStr + "/" + api.ServiceProviderClusterResourceTypeName + "/" + api.ServiceProviderClusterResourceName
	return &api.ServiceProviderCluster{
		CosmosMetadata: api.CosmosMetadata{
			ResourceID: api.Must(azcorearm.ParseResourceID(spClusterResourceID)),
		},
		ResourceID: *f.clusterResourceID,
		Spec: api.ServiceProviderClusterSpec{
			ControlPlaneVersion: api.ServiceProviderClusterSpecVersion{
				DesiredVersion: desiredVersion,
			},
		},
	}
}

func buildTestCSCluster(t *testing.T, href string) *arohcpv1alpha1.Cluster {
	t.Helper()
	cluster, err := arohcpv1alpha1.NewCluster().
		HREF(href).
		Azure(arohcpv1alpha1.NewAzure().
			SubscriptionID(strings.ToLower(testSubscriptionID)).
			ResourceGroupName(strings.ToLower(testResourceGroupName)).
			ResourceName(strings.ToLower(testClusterName)),
		).
		Build()
	require.NoError(t, err)
	return cluster
}

func TestClusterServiceCreateClusterSyncer_SyncOnce(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(f *testFixture) (*api.HCPOpenShiftCluster, []any)
		setupMocks  func(t *testing.T, mockCS *ocm.MockClusterServiceClientSpec)
		expectError bool
		errContains string
		verify      func(t *testing.T, ctx context.Context, db *databasetesting.MockDBClient)
	}{
		{
			name: "cluster not found in cosmos is a no-op",
			setup: func(f *testFixture) (*api.HCPOpenShiftCluster, []any) {
				return nil, nil
			},
		},
		{
			name: "cluster with existing ClusterServiceID is a no-op",
			setup: func(f *testFixture) (*api.HCPOpenShiftCluster, []any) {
				cluster := f.newCluster()
				csID := api.Must(api.NewInternalID(testCSClusterHREF))
				cluster.ServiceProviderProperties.ClusterServiceID = &csID
				return cluster, nil
			},
			verify: func(t *testing.T, ctx context.Context, db *databasetesting.MockDBClient) {
				cluster, err := db.HCPClusters(testSubscriptionID, testResourceGroupName).Get(ctx, testClusterName)
				require.NoError(t, err)
				assert.Equal(t, testCSClusterHREF, cluster.ServiceProviderProperties.ClusterServiceID.String())
			},
		},
		{
			name: "desired version not yet set waits for version controller",
			setup: func(f *testFixture) (*api.HCPOpenShiftCluster, []any) {
				cluster := f.newCluster()
				spc := f.newServiceProviderCluster(nil)
				return cluster, []any{spc}
			},
			verify: func(t *testing.T, ctx context.Context, db *databasetesting.MockDBClient) {
				cluster, err := db.HCPClusters(testSubscriptionID, testResourceGroupName).Get(ctx, testClusterName)
				require.NoError(t, err)
				assert.Nil(t, cluster.ServiceProviderProperties.ClusterServiceID)
			},
		},
		{
			name: "SPC auto-created when missing and desired version not set",
			setup: func(f *testFixture) (*api.HCPOpenShiftCluster, []any) {
				cluster := f.newCluster()
				return cluster, nil
			},
			verify: func(t *testing.T, ctx context.Context, db *databasetesting.MockDBClient) {
				cluster, err := db.HCPClusters(testSubscriptionID, testResourceGroupName).Get(ctx, testClusterName)
				require.NoError(t, err)
				assert.Nil(t, cluster.ServiceProviderProperties.ClusterServiceID)
			},
		},
		{
			name: "existing CS cluster found by Azure properties stores its ID",
			setup: func(f *testFixture) (*api.HCPOpenShiftCluster, []any) {
				cluster := f.newCluster()
				desiredVersion := semver.MustParse("4.19.2")
				spc := f.newServiceProviderCluster(&desiredVersion)
				sub := f.newSubscription()
				return cluster, []any{spc, sub}
			},
			setupMocks: func(t *testing.T, mockCS *ocm.MockClusterServiceClientSpec) {
				existing := buildTestCSCluster(t, testCSClusterHREF)
				mockCS.EXPECT().
					ListClusters(gomock.Any()).
					Return(ocm.NewSimpleClusterListIterator([]*arohcpv1alpha1.Cluster{existing}, nil))
			},
			verify: func(t *testing.T, ctx context.Context, db *databasetesting.MockDBClient) {
				cluster, err := db.HCPClusters(testSubscriptionID, testResourceGroupName).Get(ctx, testClusterName)
				require.NoError(t, err)
				require.NotNil(t, cluster.ServiceProviderProperties.ClusterServiceID)
				assert.Equal(t, testCSClusterHREF, cluster.ServiceProviderProperties.ClusterServiceID.String())
			},
		},
		{
			name: "no existing CS cluster creates one and stores its ID",
			setup: func(f *testFixture) (*api.HCPOpenShiftCluster, []any) {
				cluster := f.newCluster()
				desiredVersion := semver.MustParse("4.19.2")
				spc := f.newServiceProviderCluster(&desiredVersion)
				sub := f.newSubscription()
				return cluster, []any{spc, sub}
			},
			setupMocks: func(t *testing.T, mockCS *ocm.MockClusterServiceClientSpec) {
				created := buildTestCSCluster(t, testCSClusterHREF)
				mockCS.EXPECT().
					ListClusters(gomock.Any()).
					Return(ocm.NewSimpleClusterListIterator(nil, nil))
				mockCS.EXPECT().
					PostCluster(gomock.Any(), gomock.Any(), gomock.Any()).
					Return(created, nil)
			},
			verify: func(t *testing.T, ctx context.Context, db *databasetesting.MockDBClient) {
				cluster, err := db.HCPClusters(testSubscriptionID, testResourceGroupName).Get(ctx, testClusterName)
				require.NoError(t, err)
				require.NotNil(t, cluster.ServiceProviderProperties.ClusterServiceID)
				assert.Equal(t, testCSClusterHREF, cluster.ServiceProviderProperties.ClusterServiceID.String())
			},
		},
		{
			name: "ListClusters error propagates",
			setup: func(f *testFixture) (*api.HCPOpenShiftCluster, []any) {
				cluster := f.newCluster()
				desiredVersion := semver.MustParse("4.19.2")
				spc := f.newServiceProviderCluster(&desiredVersion)
				return cluster, []any{spc}
			},
			setupMocks: func(t *testing.T, mockCS *ocm.MockClusterServiceClientSpec) {
				mockCS.EXPECT().
					ListClusters(gomock.Any()).
					Return(ocm.NewSimpleClusterListIterator(nil, fmt.Errorf("list failed")))
			},
			expectError: true,
			errContains: "failed to search for existing CS cluster",
		},
		{
			name: "PostCluster error propagates",
			setup: func(f *testFixture) (*api.HCPOpenShiftCluster, []any) {
				cluster := f.newCluster()
				desiredVersion := semver.MustParse("4.19.2")
				spc := f.newServiceProviderCluster(&desiredVersion)
				sub := f.newSubscription()
				return cluster, []any{spc, sub}
			},
			setupMocks: func(t *testing.T, mockCS *ocm.MockClusterServiceClientSpec) {
				mockCS.EXPECT().
					ListClusters(gomock.Any()).
					Return(ocm.NewSimpleClusterListIterator(nil, nil))
				mockCS.EXPECT().
					PostCluster(gomock.Any(), gomock.Any(), gomock.Any()).
					Return(nil, fmt.Errorf("CS unavailable"))
			},
			expectError: true,
			errContains: "failed to create cluster in CS",
		},
		{
			name: "multiple CS clusters found returns error",
			setup: func(f *testFixture) (*api.HCPOpenShiftCluster, []any) {
				cluster := f.newCluster()
				desiredVersion := semver.MustParse("4.19.2")
				spc := f.newServiceProviderCluster(&desiredVersion)
				return cluster, []any{spc}
			},
			setupMocks: func(t *testing.T, mockCS *ocm.MockClusterServiceClientSpec) {
				a := buildTestCSCluster(t, "/api/clusters_mgmt/v1/clusters/aaa")
				b := buildTestCSCluster(t, "/api/clusters_mgmt/v1/clusters/bbb")
				mockCS.EXPECT().
					ListClusters(gomock.Any()).
					Return(ocm.NewSimpleClusterListIterator([]*arohcpv1alpha1.Cluster{a, b}, nil))
			},
			expectError: true,
			errContains: "cluster service returned 2 clusters for one Azure resource",
		},
		{
			name: "resolved desired version is sent to PostCluster",
			setup: func(f *testFixture) (*api.HCPOpenShiftCluster, []any) {
				cluster := f.newCluster()
				desiredVersion := semver.MustParse("4.19.7")
				spc := f.newServiceProviderCluster(&desiredVersion)
				sub := f.newSubscription()
				return cluster, []any{spc, sub}
			},
			setupMocks: func(t *testing.T, mockCS *ocm.MockClusterServiceClientSpec) {
				created := buildTestCSCluster(t, testCSClusterHREF)
				mockCS.EXPECT().
					ListClusters(gomock.Any()).
					Return(ocm.NewSimpleClusterListIterator(nil, nil))
				mockCS.EXPECT().
					PostCluster(gomock.Any(), gomock.Any(), gomock.Any()).
					DoAndReturn(func(ctx context.Context, clusterBuilder *arohcpv1alpha1.ClusterBuilder, autoscalerBuilder *arohcpv1alpha1.ClusterAutoscalerBuilder) (*arohcpv1alpha1.Cluster, error) {
						built, err := clusterBuilder.Build()
						require.NoError(t, err)
						version, ok := built.Version().GetID()
						require.True(t, ok, "version ID should be set on CS cluster")
						assert.Contains(t, version, "4.19.7")
						return created, nil
					})
			},
		},
		{
			name: "subscription tenant ID is passed through to CS",
			setup: func(f *testFixture) (*api.HCPOpenShiftCluster, []any) {
				cluster := f.newCluster()
				desiredVersion := semver.MustParse("4.19.2")
				spc := f.newServiceProviderCluster(&desiredVersion)
				sub := f.newSubscription()
				return cluster, []any{spc, sub}
			},
			setupMocks: func(t *testing.T, mockCS *ocm.MockClusterServiceClientSpec) {
				created := buildTestCSCluster(t, testCSClusterHREF)
				mockCS.EXPECT().
					ListClusters(gomock.Any()).
					Return(ocm.NewSimpleClusterListIterator(nil, nil))
				mockCS.EXPECT().
					PostCluster(gomock.Any(), gomock.Any(), gomock.Any()).
					DoAndReturn(func(ctx context.Context, clusterBuilder *arohcpv1alpha1.ClusterBuilder, autoscalerBuilder *arohcpv1alpha1.ClusterAutoscalerBuilder) (*arohcpv1alpha1.Cluster, error) {
						built, err := clusterBuilder.Build()
						require.NoError(t, err)
						az := built.Azure()
						require.NotNil(t, az)
						tid, ok := az.GetTenantID()
						require.True(t, ok)
						assert.Equal(t, testTenantID, tid)
						return created, nil
					})
			},
		},
		{
			name: "search expression uses lowercased Azure properties",
			setup: func(f *testFixture) (*api.HCPOpenShiftCluster, []any) {
				cluster := f.newCluster()
				desiredVersion := semver.MustParse("4.19.2")
				spc := f.newServiceProviderCluster(&desiredVersion)
				sub := f.newSubscription()
				return cluster, []any{spc, sub}
			},
			setupMocks: func(t *testing.T, mockCS *ocm.MockClusterServiceClientSpec) {
				expectedSearch := fmt.Sprintf(
					"azure.subscription_id = '%s' and azure.resource_group_name = '%s' and azure.resource_name = '%s'",
					strings.ToLower(testSubscriptionID),
					strings.ToLower(testResourceGroupName),
					strings.ToLower(testClusterName),
				)
				created := buildTestCSCluster(t, testCSClusterHREF)
				mockCS.EXPECT().
					ListClusters(expectedSearch).
					Return(ocm.NewSimpleClusterListIterator(nil, nil))
				mockCS.EXPECT().
					PostCluster(gomock.Any(), gomock.Any(), gomock.Any()).
					Return(created, nil)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctx = utils.ContextWithLogger(ctx, testr.New(t))
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			fixture := newTestFixture()
			cluster, extraResources := tt.setup(fixture)

			var resources []any
			if cluster != nil {
				resources = append(resources, cluster)
			}
			resources = append(resources, extraResources...)

			mockDB, err := databasetesting.NewMockDBClientWithResources(ctx, resources)
			require.NoError(t, err)

			mockCS := ocm.NewMockClusterServiceClientSpec(ctrl)
			if tt.setupMocks != nil {
				tt.setupMocks(t, mockCS)
			}

			syncer := &clusterServiceCreateClusterSyncer{
				cooldownChecker:      &alwaysSyncCooldownChecker{},
				cosmosClient:         mockDB,
				clusterServiceClient: mockCS,
			}

			err = syncer.SyncOnce(ctx, fixture.clusterKey())

			if tt.expectError {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}

			require.NoError(t, err)

			if tt.verify != nil {
				tt.verify(t, ctx, mockDB)
			}
		})
	}
}
