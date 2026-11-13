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
	"strings"
	"sync"
	"time"

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

	// Node sizes as seen on UKS nodes (Kubernetes 1.31 and 1.34, 2026-11): the kernel keeps
	// 144 MiB + 35/2048 (1.71 %) of the plan's memory (2/4/8/16 GB plans: 179/214/284/424 MiB,
	// up to 3 MiB more on 1.31); the template uses 148 MiB so it never overstates a node.
	nodeKernelMemoryBaseMiB int64 = 148
	nodeKernelMemoryPerMiB  int64 = 35 // per 2048 MiB of plan memory
	// nodeReservedMemory is the difference between memory capacity and allocatable.
	nodeReservedMemory int64 = 100 * 1024 * 1024
	// The root filesystem is about 98 % of the disk (20/30/40/100 GB: 19.59/29.43/39.30/98.33 GiB);
	// the template uses 97.5 %. The kubelet eviction threshold leaves 90 % of it allocatable.
	nodeEphemeralStorageCapacityPerMille   int64 = 975
	nodeEphemeralStorageAllocatablePercent int64 = 90
	// nodeMaxPods is the kubelet pod limit on UKS nodes.
	nodeMaxPods int64 = 110

	// planMissRetryInterval is how long a plan name missing from `GET /plan` is remembered.
	planMissRetryInterval = 5 * time.Minute

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
	gpu         bool
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
		t.gpu = true
	}
	return t
}

// nodeResources are the hardware resources of one node.
type nodeResources struct {
	cores       int
	memoryMiB   int
	storageSize int // GB
}

// resources returns cloudprovider.ErrNotImplemented for GPU plans: the template has no GPU resources.
func (t nodeTemplate) resources(ctx context.Context, plans *planCache) (nodeResources, error) {
	var r nodeResources
	if t.gpu {
		return r, cloudprovider.ErrNotImplemented
	}
	if t.customPlan != nil {
		r = nodeResources{cores: t.customPlan.Cores, memoryMiB: t.customPlan.Memory, storageSize: t.customPlan.StorageSize}
	} else {
		p, err := plans.get(ctx, t.plan)
		if err != nil {
			return r, err
		}
		if p.GPUAmount > 0 {
			return r, cloudprovider.ErrNotImplemented
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
	planMemoryMiB := int64(r.memoryMiB)
	kernelMemoryMiB := nodeKernelMemoryBaseMiB + (planMemoryMiB*nodeKernelMemoryPerMiB+2047)/2048
	memory := max(planMemoryMiB-kernelMemoryMiB, 0) * 1024 * 1024
	capacity := apiv1.ResourceList{
		apiv1.ResourceCPU:    *resource.NewQuantity(int64(r.cores), resource.DecimalSI),
		apiv1.ResourceMemory: *resource.NewQuantity(memory, resource.BinarySI),
		apiv1.ResourcePods:   *resource.NewQuantity(nodeMaxPods, resource.DecimalSI),
	}
	allocatable := capacity.DeepCopy()
	allocatable[apiv1.ResourceMemory] = *resource.NewQuantity(max(memory-nodeReservedMemory, 0), resource.BinarySI)
	if r.storageSize > 0 {
		storage := int64(r.storageSize) * 1024 * 1024 * 1024 * nodeEphemeralStorageCapacityPerMille / 1000
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

// planCache caches UpCloud server plans (`GET /plan`) by name (case-insensitive). Plans are
// read only when a template node is needed, and read again when a name is not in the cache;
// a name that is still missing is remembered for planMissRetryInterval.
type planCache struct {
	svc    upCloudService
	now    func() time.Time
	mu     sync.Mutex
	plans  map[string]upcloud.Plan
	misses map[string]time.Time
}

func newPlanCache(svc upCloudService) *planCache {
	return &planCache{
		svc:    svc,
		now:    time.Now,
		plans:  make(map[string]upcloud.Plan),
		misses: make(map[string]time.Time),
	}
}

func (c *planCache) get(ctx context.Context, name string) (upcloud.Plan, error) {
	key := strings.ToLower(name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.plans[key]; ok {
		return p, nil
	}
	if missed, ok := c.misses[key]; ok && c.now().Sub(missed) < planMissRetryInterval {
		return upcloud.Plan{}, fmt.Errorf("plan %q not found", name)
	}
	plans, err := c.svc.GetPlans(ctx)
	if err != nil {
		return upcloud.Plan{}, fmt.Errorf("failed to get plans: %w", err)
	}
	c.plans = make(map[string]upcloud.Plan, len(plans.Plans))
	for _, p := range plans.Plans {
		c.plans[strings.ToLower(p.Name)] = p
	}
	if p, ok := c.plans[key]; ok {
		delete(c.misses, key)
		return p, nil
	}
	c.misses[key] = c.now()
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
