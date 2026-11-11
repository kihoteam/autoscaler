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
	"fmt"
	"sync"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/upcloud/pkg/github.com/upcloudltd/upcloud-go-api/v8/upcloud"
	"k8s.io/autoscaler/cluster-autoscaler/config/dynamic"
)

// Template nodes let the autoscaler scale a node group up from zero nodes: with no
// real node to copy, the scale-up simulation needs to know what a new node of the
// group would look like.

const (
	// nodeGroupSupportScaleToZero allows node group specs with min size 0 (`0:N:<name>`).
	nodeGroupSupportScaleToZero = true

	// nodeMaxPods is the kubelet pod limit on UKS nodes.
	nodeMaxPods int64 = 110
	// nodeReservedMemory is the difference between memory capacity and allocatable on UKS nodes.
	nodeReservedMemory int64 = 100 * 1024 * 1024
	// nodeEphemeralStorageAllocatablePercent is the allocatable share of the root disk on UKS nodes
	// (the rest is kept free by the kubelet eviction threshold).
	nodeEphemeralStorageAllocatablePercent int64 = 90

	nodeOS   = "linux"
	nodeArch = "amd64"
)

// nodeTemplate holds what the UpCloud API tells about the nodes of a node group.
type nodeTemplate struct {
	plan       string
	customPlan *upcloud.KubernetesNodeGroupCustomPlan
	// storageSize is the root disk size in GB from the node group's cloud native or GPU plan,
	// 0 when the size comes from the server plan.
	storageSize int
	labels      []upcloud.Label
	taints      []upcloud.KubernetesTaint
	zone        string
}

func newNodeTemplate(g upcloud.KubernetesNodeGroup, zone string) nodeTemplate {
	t := nodeTemplate{
		plan:       g.Plan,
		customPlan: g.CustomPlan,
		labels:     g.Labels,
		taints:     g.Taints,
		zone:       zone,
	}
	switch {
	case g.CloudNativePlan != nil:
		t.storageSize = g.CloudNativePlan.StorageSize
	case g.GPUPlan != nil:
		t.storageSize = g.GPUPlan.StorageSize
	}
	return t
}

// nodeResources are the hardware resources of one node.
type nodeResources struct {
	cores       int
	memoryMiB   int
	storageSize int // GB
}

func (t nodeTemplate) resources(ctx context.Context, plans *planCache) (nodeResources, error) {
	var r nodeResources
	if t.customPlan != nil {
		r = nodeResources{cores: t.customPlan.Cores, memoryMiB: t.customPlan.Memory, storageSize: t.customPlan.StorageSize}
	} else {
		p, err := plans.get(ctx, t.plan)
		if err != nil {
			return r, err
		}
		r = nodeResources{cores: p.CoreNumber, memoryMiB: p.MemoryAmount, storageSize: p.StorageSize}
		if t.storageSize > 0 {
			r.storageSize = t.storageSize
		}
	}
	if r.cores <= 0 || r.memoryMiB <= 0 {
		return r, fmt.Errorf("plan %q has no CPU or memory (cores=%d memory=%d MiB)", t.plan, r.cores, r.memoryMiB)
	}
	return r, nil
}

// buildTemplateNode returns a ready node with the node group's resources, labels and taints.
// The autoscaler adds the DaemonSet pods to the template itself.
func buildTemplateNode(name string, t nodeTemplate, r nodeResources) *apiv1.Node {
	memory := int64(r.memoryMiB) * 1024 * 1024
	capacity := apiv1.ResourceList{
		apiv1.ResourceCPU:    *resource.NewQuantity(int64(r.cores), resource.DecimalSI),
		apiv1.ResourceMemory: *resource.NewQuantity(memory, resource.BinarySI),
		apiv1.ResourcePods:   *resource.NewQuantity(nodeMaxPods, resource.DecimalSI),
	}
	allocatable := capacity.DeepCopy()
	allocatable[apiv1.ResourceMemory] = *resource.NewQuantity(max(memory-nodeReservedMemory, 0), resource.BinarySI)
	if r.storageSize > 0 {
		storage := int64(r.storageSize) * 1024 * 1024 * 1024
		capacity[apiv1.ResourceEphemeralStorage] = *resource.NewQuantity(storage, resource.BinarySI)
		allocatable[apiv1.ResourceEphemeralStorage] = *resource.NewQuantity(storage*nodeEphemeralStorageAllocatablePercent/100, resource.BinarySI)
	}

	labels := map[string]string{
		apiv1.LabelOSStable:   nodeOS,
		apiv1.LabelArchStable: nodeArch,
		apiv1.LabelHostname:   name,
	}
	if t.zone != "" {
		labels[apiv1.LabelTopologyRegion] = t.zone
		labels[apiv1.LabelTopologyZone] = t.zone
	}
	for _, l := range t.labels {
		labels[l.Key] = l.Value
	}

	taints := make([]apiv1.Taint, 0, len(t.taints))
	for _, taint := range t.taints {
		taints = append(taints, apiv1.Taint{
			Key:    taint.Key,
			Value:  taint.Value,
			Effect: apiv1.TaintEffect(taint.Effect),
		})
	}

	return &apiv1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: apiv1.NodeSpec{
			ProviderID: fmt.Sprintf("upcloud:////%s", name),
			Taints:     taints,
		},
		Status: apiv1.NodeStatus{
			Capacity:    capacity,
			Allocatable: allocatable,
			Conditions:  cloudprovider.BuildReadyConditions(),
		},
	}
}

// planCache caches UpCloud server plans (`GET /plan`) by name. Plans are read only when
// a template node is needed, and read again when a plan name is not in the cache.
type planCache struct {
	svc   upCloudService
	mu    sync.Mutex
	plans map[string]upcloud.Plan
}

func newPlanCache(svc upCloudService) *planCache {
	return &planCache{svc: svc, plans: make(map[string]upcloud.Plan)}
}

func (c *planCache) get(ctx context.Context, name string) (upcloud.Plan, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.plans[name]; ok {
		return p, nil
	}
	plans, err := c.svc.GetPlans(ctx)
	if err != nil {
		return upcloud.Plan{}, fmt.Errorf("failed to get plans: %w", err)
	}
	c.plans = make(map[string]upcloud.Plan, len(plans.Plans))
	for _, p := range plans.Plans {
		c.plans[p.Name] = p
	}
	if p, ok := c.plans[name]; ok {
		return p, nil
	}
	return upcloud.Plan{}, fmt.Errorf("plan %q not found", name)
}

// atLeastOneNodeGroupHasNodes reports whether a node group spec keeps at least one node.
// The autoscaler runs on the cluster's own worker nodes, so not every group may scale to zero.
func atLeastOneNodeGroupHasNodes(specs map[string]dynamic.NodeGroupSpec) bool {
	for _, spec := range specs {
		if spec.MinSize > 0 {
			return true
		}
	}
	return false
}
