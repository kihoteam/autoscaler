/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package upcloud

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/upcloud/mocks"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/upcloud/pkg/github.com/upcloudltd/upcloud-go-api/v8/upcloud"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	"k8s.io/autoscaler/cluster-autoscaler/config/dynamic"
	"k8s.io/autoscaler/cluster-autoscaler/simulator"
	"k8s.io/autoscaler/cluster-autoscaler/utils/taints"
)

var testServerPlans = []upcloud.Plan{
	{Name: "CLOUDNATIVE-2xCPU-4GB", CoreNumber: 2, MemoryAmount: 4096, StorageSize: 0},
	{Name: "2xCPU-4GB", CoreNumber: 2, MemoryAmount: 4096, StorageSize: 80},
	{Name: "no-resources"},
	{Name: "GPU-8xCPU-64GB-1xL40S", CoreNumber: 8, MemoryAmount: 65536, StorageSize: 0, GPUAmount: 1},
}

func newTemplateTestGroup(svc *mocks.UpCloudService, g upcloud.KubernetesNodeGroup) *upCloudNodeGroup {
	return &upCloudNodeGroup{
		clusterID: uuid.New(),
		name:      g.Name,
		size:      g.Count,
		svc:       svc,
		template:  newNodeTemplate(g, "fi-hel2"),
		plans:     newPlanCache(svc),
	}
}

func requireQuantity(t *testing.T, want string, list apiv1.ResourceList, name apiv1.ResourceName) {
	t.Helper()
	got, ok := list[name]
	require.True(t, ok, "resource %s missing", name)
	wantQuantity := resource.MustParse(want)
	require.Zero(t, wantQuantity.Cmp(got), "resource %s: want %s, got %s", name, want, got.String())
}

func TestTemplateNodeInfo_CloudNativePlan(t *testing.T) {
	t.Parallel()

	svc := &mocks.UpCloudService{ServerPlans: testServerPlans}
	g := newTemplateTestGroup(svc, upcloud.KubernetesNodeGroup{
		Name:            "auto-scale-2x-4gb-una",
		Plan:            "CLOUDNATIVE-2xCPU-4GB",
		CloudNativePlan: &upcloud.KubernetesNodeGroupCloudNativePlan{StorageSize: 20},
		Labels:          []upcloud.Label{{Key: "workload", Value: "migration-tool"}, {Key: "auto-scaling", Value: "true"}},
		Taints:          []upcloud.KubernetesTaint{{Effect: "NoSchedule", Key: "workload-isolation", Value: "migration-tool"}},
	})

	nodeInfo, err := g.TemplateNodeInfo()
	require.NoError(t, err)
	node := nodeInfo.Node()

	requireQuantity(t, "2", node.Status.Capacity, apiv1.ResourceCPU)
	requireQuantity(t, "3878Mi", node.Status.Capacity, apiv1.ResourceMemory)
	requireQuantity(t, "110", node.Status.Capacity, apiv1.ResourcePods)
	requireQuantity(t, "19968Mi", node.Status.Capacity, apiv1.ResourceEphemeralStorage)
	requireQuantity(t, "2", node.Status.Allocatable, apiv1.ResourceCPU)
	requireQuantity(t, "3778Mi", node.Status.Allocatable, apiv1.ResourceMemory)
	requireQuantity(t, "18844169011", node.Status.Allocatable, apiv1.ResourceEphemeralStorage)

	require.Equal(t, "migration-tool", node.Labels["workload"])
	require.Equal(t, "true", node.Labels["auto-scaling"])
	require.Equal(t, "linux", node.Labels[apiv1.LabelOSStable])
	require.Equal(t, "amd64", node.Labels[apiv1.LabelArchStable])
	require.Equal(t, "fi-hel2", node.Labels[apiv1.LabelTopologyZone])
	require.Equal(t, "fi-hel2", node.Labels[apiv1.LabelTopologyRegion])
	require.Equal(t, node.Name, node.Labels[apiv1.LabelHostname])
	require.Equal(t, []apiv1.Taint{{Key: "workload-isolation", Value: "migration-tool", Effect: apiv1.TaintEffectNoSchedule}}, node.Spec.Taints)
	require.Len(t, node.Status.Conditions, len(cloudprovider.BuildReadyConditions()))
	require.Empty(t, nodeInfo.Pods())
}

func TestTemplateNodeInfo_ServerPlanStorage(t *testing.T) {
	t.Parallel()

	svc := &mocks.UpCloudService{ServerPlans: testServerPlans}
	g := newTemplateTestGroup(svc, upcloud.KubernetesNodeGroup{Name: "g", Plan: "2xCPU-4GB"})

	nodeInfo, err := g.TemplateNodeInfo()
	require.NoError(t, err)
	requireQuantity(t, "78Gi", nodeInfo.Node().Status.Capacity, apiv1.ResourceEphemeralStorage)
	require.Empty(t, nodeInfo.Node().Spec.Taints)
}

func TestTemplateNodeInfo_NoStorageSize(t *testing.T) {
	t.Parallel()

	svc := &mocks.UpCloudService{ServerPlans: testServerPlans}
	g := newTemplateTestGroup(svc, upcloud.KubernetesNodeGroup{Name: "g", Plan: "CLOUDNATIVE-2xCPU-4GB"})

	nodeInfo, err := g.TemplateNodeInfo()
	require.NoError(t, err)
	_, ok := nodeInfo.Node().Status.Capacity[apiv1.ResourceEphemeralStorage]
	require.False(t, ok, "unknown root disk size must not be reported")
}

func TestTemplateNodeInfo_CustomPlan(t *testing.T) {
	t.Parallel()

	svc := &mocks.UpCloudService{}
	g := newTemplateTestGroup(svc, upcloud.KubernetesNodeGroup{
		Name:       "g",
		Plan:       "custom",
		CustomPlan: &upcloud.KubernetesNodeGroupCustomPlan{Cores: 6, Memory: 12288, StorageSize: 50},
	})

	nodeInfo, err := g.TemplateNodeInfo()
	require.NoError(t, err)
	requireQuantity(t, "6", nodeInfo.Node().Status.Capacity, apiv1.ResourceCPU)
	requireQuantity(t, "11930Mi", nodeInfo.Node().Status.Capacity, apiv1.ResourceMemory)
	requireQuantity(t, "49920Mi", nodeInfo.Node().Status.Capacity, apiv1.ResourceEphemeralStorage)
	require.Zero(t, svc.GetPlansCalls, "custom plan must not read server plans")
}

func TestTemplateNodeInfo_PlanErrors(t *testing.T) {
	t.Parallel()

	svc := &mocks.UpCloudService{ServerPlans: testServerPlans}
	for _, plan := range []string{"unknown", "no-resources"} {
		g := newTemplateTestGroup(svc, upcloud.KubernetesNodeGroup{Name: "g", Plan: plan})
		_, err := g.TemplateNodeInfo()
		require.Error(t, err, plan)
		require.NotErrorIs(t, err, cloudprovider.ErrNotImplemented, plan)
	}
}

func TestPlanCache(t *testing.T) {
	t.Parallel()

	svc := &mocks.UpCloudService{ServerPlans: testServerPlans}
	c := newPlanCache(svc)

	p, err := c.get(context.Background(), "2xCPU-4GB")
	require.NoError(t, err)
	require.Equal(t, 80, p.StorageSize)
	_, err = c.get(context.Background(), "CLOUDNATIVE-2xCPU-4GB")
	require.NoError(t, err)
	require.Equal(t, 1, svc.GetPlansCalls, "cached plans must not be read again")

	_, err = c.get(context.Background(), "unknown")
	require.Error(t, err)
	require.Equal(t, 2, svc.GetPlansCalls, "a missing plan reloads the list")
}

func TestManager_NodeGroupTemplate(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	svc := newMockService(clusterID)
	svc.ServerPlans = testServerPlans
	require.NoError(t, svc.AppendNodeGroup(context.Background(), clusterID, upcloud.KubernetesNodeGroup{
		Name:            "zero",
		Plan:            "CLOUDNATIVE-2xCPU-4GB",
		CloudNativePlan: &upcloud.KubernetesNodeGroupCloudNativePlan{StorageSize: 20},
		Labels:          []upcloud.Label{{Key: "workload", Value: "batch"}},
		State:           upcloud.KubernetesNodeGroupStateRunning,
	}))

	m, err := newManager(context.Background(), svc, upCloudConfig{ClusterID: clusterID.String()}, config.AutoscalingOptions{},
		cloudprovider.NodeGroupDiscoveryOptions{NodeGroupSpecs: []string{"1:3:group1", "0:2:zero"}})
	require.NoError(t, err)
	require.NoError(t, m.refresh(t.Context()))

	var zero *upCloudNodeGroup
	for _, g := range m.nodeGroups {
		if g.name == "zero" {
			zero = g
		}
	}
	require.NotNil(t, zero)
	require.Equal(t, 0, zero.MinSize())
	require.Equal(t, 2, zero.MaxSize())

	nodeInfo, err := zero.TemplateNodeInfo()
	require.NoError(t, err)
	require.Equal(t, "batch", nodeInfo.Node().Labels["workload"])
	require.Equal(t, "fi-hel2", nodeInfo.Node().Labels[apiv1.LabelTopologyZone])
	requireQuantity(t, "19968Mi", nodeInfo.Node().Status.Capacity, apiv1.ResourceEphemeralStorage)
}

func TestManager_AllNodeGroupsScaleToZeroRejected(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	svc := newMockService(clusterID)
	m, err := newManager(context.Background(), svc, upCloudConfig{ClusterID: clusterID.String()}, config.AutoscalingOptions{},
		cloudprovider.NodeGroupDiscoveryOptions{NodeGroupSpecs: []string{"0:2:group1", "0:3:group2"}})
	require.Nil(t, m)
	require.ErrorContains(t, err, "at least one node group must have min size > 0")
}

// The autoscaler sanitizes the template and adds DaemonSet pods that tolerate the node's taints.
func TestTemplateNodeInfo_AutoscalerSanitizes(t *testing.T) {
	t.Parallel()

	svc := &mocks.UpCloudService{ServerPlans: testServerPlans}
	g := newTemplateTestGroup(svc, upcloud.KubernetesNodeGroup{
		Name:   "g",
		Plan:   "2xCPU-4GB",
		Taints: []upcloud.KubernetesTaint{{Effect: "NoSchedule", Key: "workload-isolation", Value: "x"}},
	})
	daemonSet := func(name string, tolerate bool) *appsv1.DaemonSet {
		ds := &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system", UID: types.UID("uid-" + name)},
			Spec: appsv1.DaemonSetSpec{Template: apiv1.PodTemplateSpec{Spec: apiv1.PodSpec{
				Containers: []apiv1.Container{{Name: name, Resources: apiv1.ResourceRequirements{Requests: apiv1.ResourceList{
					apiv1.ResourceMemory: resource.MustParse("200Mi"),
				}}}},
			}}},
		}
		if tolerate {
			ds.Spec.Template.Spec.Tolerations = []apiv1.Toleration{{Operator: apiv1.TolerationOpExists}}
		}
		return ds
	}

	nodeInfo, err := simulator.SanitizedTemplateNodeInfoFromNodeGroup(g,
		[]*appsv1.DaemonSet{daemonSet("cilium", true), daemonSet("no-toleration", false)}, taints.TaintConfig{})
	require.NoError(t, err)
	require.Len(t, nodeInfo.Pods(), 1)
	require.Equal(t, "cilium", nodeInfo.Pods()[0].Spec.Containers[0].Name)
	requireQuantity(t, "3878Mi", nodeInfo.Node().Status.Capacity, apiv1.ResourceMemory)
}

// Real UKS nodes (read-only kubectl, 2026-11-11): the template must not report more than the
// node has, and should be close to it. Memory capacity in KiB per plan size in MiB.
func TestTemplateNodeSizeMatchesRealNodes(t *testing.T) {
	t.Parallel()

	memoryCapacityKi := map[int]int64{
		2048:  (2048 - 179) * 1024,
		4096:  3975020,
		8192:  8097724,
		16384: (16384 - 424) * 1024,
	}
	for planMiB, realKi := range memoryCapacityKi {
		node := buildTemplateNode("n", nodeTemplate{}, nodeResources{cores: 2, memoryMiB: planMiB})
		got := node.Status.Capacity.Memory().Value() / 1024
		require.LessOrEqual(t, got, realKi, "plan %d MiB", planMiB)
		require.Less(t, realKi-got, int64(8*1024), "plan %d MiB: more than 8 MiB below the real node", planMiB)
		gotAllocatable := node.Status.Allocatable.Memory().Value() / 1024
		require.Equal(t, got-100*1024, gotAllocatable, "plan %d MiB", planMiB)
		require.Equal(t, int64(2), node.Status.Allocatable.Cpu().Value())
	}

	// Root filesystem in GiB (rounded down) per disk size in GB.
	storageCapacityGiB := map[int]float64{20: 19.59, 30: 29.43, 40: 39.30, 100: 98.33}
	for diskGB, realGiB := range storageCapacityGiB {
		node := buildTemplateNode("n", nodeTemplate{}, nodeResources{cores: 2, memoryMiB: 4096, storageSize: diskGB})
		got := float64(node.Status.Capacity.StorageEphemeral().Value()) / (1 << 30)
		require.LessOrEqual(t, got, realGiB, "disk %d GB", diskGB)
		require.Less(t, realGiB-got, 0.02*float64(diskGB), "disk %d GB: more than 2 %% below the real node", diskGB)
	}
}

func TestTemplateNodeInfo_GPUNotImplemented(t *testing.T) {
	t.Parallel()

	svc := &mocks.UpCloudService{ServerPlans: testServerPlans}
	for _, g := range []upcloud.KubernetesNodeGroup{
		{Name: "gpu-plan", Plan: "GPU-8xCPU-64GB-1xL40S"},
		{Name: "gpu-node-group", Plan: "2xCPU-4GB", GPUPlan: &upcloud.KubernetesNodeGroupGPUPlan{StorageSize: 100}},
	} {
		_, err := newTemplateTestGroup(svc, g).TemplateNodeInfo()
		require.ErrorIs(t, err, cloudprovider.ErrNotImplemented, g.Name)
	}
}

func TestPlanCache_MissIsRemembered(t *testing.T) {
	t.Parallel()

	svc := &mocks.UpCloudService{ServerPlans: testServerPlans}
	c := newPlanCache(svc)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	for range 3 {
		_, err := c.get(context.Background(), "unknown")
		require.ErrorContains(t, err, "not found")
	}
	require.Equal(t, 1, svc.GetPlansCalls, "a missing plan must not be read again within the retry interval")

	now = now.Add(planMissRetryInterval)
	_, err := c.get(context.Background(), "unknown")
	require.Error(t, err)
	require.Equal(t, 2, svc.GetPlansCalls, "after the retry interval the plans are read again")

	p, err := c.get(context.Background(), "cloudnative-2xcpu-4gb")
	require.NoError(t, err, "plan names are case-insensitive")
	require.Equal(t, "CLOUDNATIVE-2xCPU-4GB", p.Name)
}

func TestPlanCache_APIError(t *testing.T) {
	t.Parallel()

	svc := &mocks.UpCloudService{ServerPlans: testServerPlans, GetPlansErr: errors.New("api down")}
	c := newPlanCache(svc)
	_, err := c.get(context.Background(), "2xCPU-4GB")
	require.ErrorContains(t, err, "api down")
	_, err = c.get(context.Background(), "2xCPU-4GB")
	require.Error(t, err)
	require.Equal(t, 2, svc.GetPlansCalls, "API errors are not cached")

	svc.GetPlansErr = nil
	_, err = c.get(context.Background(), "2xCPU-4GB")
	require.NoError(t, err)
}

func TestPlanCache_Concurrent(t *testing.T) {
	t.Parallel()

	svc := &mocks.UpCloudService{ServerPlans: testServerPlans}
	c := newPlanCache(svc)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := []string{"2xCPU-4GB", "CLOUDNATIVE-2xCPU-4GB", "unknown"}[i%3]
			_, _ = c.get(context.Background(), name)
		}()
	}
	wg.Wait()
	require.LessOrEqual(t, svc.GetPlansCalls, 2)
}

func TestSpecsWithoutNodeGroup(t *testing.T) {
	t.Parallel()

	specs := map[string]dynamic.NodeGroupSpec{
		"group1": {Name: "group1", MinSize: 1, MaxSize: 2},
		"typo-b": {Name: "typo-b", MinSize: 0, MaxSize: 2},
		"typo-a": {Name: "typo-a", MinSize: 0, MaxSize: 2},
	}
	groups := []*upCloudNodeGroup{{name: "group1"}, {name: "group2"}}
	require.Equal(t, []string{"typo-a", "typo-b"}, specsWithoutNodeGroup(specs, groups))
	require.Empty(t, specsWithoutNodeGroup(nil, groups))
}
