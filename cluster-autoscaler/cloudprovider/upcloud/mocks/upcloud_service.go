/*
Copyright 2023 The Kubernetes Authors.

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

package mocks

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/upcloud/pkg/github.com/upcloudltd/upcloud-go-api/v6/upcloud"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/upcloud/pkg/github.com/upcloudltd/upcloud-go-api/v6/upcloud/request"
)

// UpCloudService is mock that implements UpCloudService
type UpCloudService struct {
	Clusters map[string]upcloud.KubernetesCluster
	Plans    []upcloud.KubernetesPlan
	// nodes keeps the nodes of each node group (key cluster UUID/group name) across calls,
	// like UKS: node names stay the same when other nodes are deleted
	nodes    map[string][]upcloud.KubernetesNode
	nextNode map[string]int
	// DeletedNodes records the node names passed to DeleteKubernetesNodeGroupNode
	DeletedNodes []string
	NodePlans    []upcloud.Plan
	mu           sync.Mutex
}

// GetKubernetesNodeGroups list node groups
func (s *UpCloudService) GetKubernetesNodeGroups(ctx context.Context, r *request.GetKubernetesNodeGroupsRequest) ([]upcloud.KubernetesNodeGroup, error) {
	cluster, err := s.GetKubernetesCluster(ctx, &request.GetKubernetesClusterRequest{UUID: r.ClusterUUID})
	if err != nil {
		return nil, err
	}
	return cluster.NodeGroups, nil
}

// ModifyKubernetesNodeGroup modifies the node group
func (s *UpCloudService) ModifyKubernetesNodeGroup(ctx context.Context, r *request.ModifyKubernetesNodeGroupRequest) (*upcloud.KubernetesNodeGroup, error) {
	cluster, err := s.GetKubernetesCluster(ctx, &request.GetKubernetesClusterRequest{UUID: r.ClusterUUID})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range cluster.NodeGroups {
		if cluster.NodeGroups[i].Name == r.Name {
			cluster.NodeGroups[i].Count = r.NodeGroup.Count
			return &cluster.NodeGroups[i], nil
		}
	}
	return nil, fmt.Errorf("node group not found %+v", r)
}

// DeleteKubernetesNodeGroupNode deletes the node group
func (s *UpCloudService) DeleteKubernetesNodeGroupNode(ctx context.Context, r *request.DeleteKubernetesNodeGroupNodeRequest) error {
	cluster, err := s.GetKubernetesCluster(ctx, &request.GetKubernetesClusterRequest{UUID: r.ClusterUUID})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range cluster.NodeGroups {
		if cluster.NodeGroups[i].Name != r.Name {
			continue
		}
		key := nodeGroupKey(r.ClusterUUID, r.Name)
		nodes := s.groupNodes(key, &cluster.NodeGroups[i])
		n := make([]upcloud.KubernetesNode, 0, len(nodes))
		for j := range nodes {
			if nodes[j].Name != r.NodeName {
				n = append(n, nodes[j])
			}
		}
		if len(n) == len(nodes) {
			// the UKS API deletes nodes by name and returns 404 for an unknown name
			return &upcloud.Problem{Status: http.StatusNotFound}
		}
		s.DeletedNodes = append(s.DeletedNodes, r.NodeName)
		s.nodes[key] = n
		cluster.NodeGroups[i].Count--
		s.Clusters[r.ClusterUUID] = *cluster
		return nil
	}
	return fmt.Errorf("node group not found %+v", r)
}

// GetKubernetesNodeGroup returns node group details
func (s *UpCloudService) GetKubernetesNodeGroup(ctx context.Context, r *request.GetKubernetesNodeGroupRequest) (*upcloud.KubernetesNodeGroupDetails, error) {
	cluster, err := s.GetKubernetesCluster(ctx, &request.GetKubernetesClusterRequest{UUID: r.ClusterUUID})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range cluster.NodeGroups {
		if cluster.NodeGroups[i].Name == r.Name {
			nodes := s.groupNodes(nodeGroupKey(r.ClusterUUID, r.Name), &cluster.NodeGroups[i])
			return &upcloud.KubernetesNodeGroupDetails{
				KubernetesNodeGroup: cluster.NodeGroups[i],
				Nodes:               append([]upcloud.KubernetesNode(nil), nodes...),
			}, nil
		}
	}
	return nil, fmt.Errorf("node group details not found %+v", r)
}

func nodeGroupKey(clusterUUID, nodeGroup string) string {
	return clusterUUID + "/" + nodeGroup
}

// groupNodes returns the kept nodes of a node group, matched to its count: new nodes get
// the next unused index, a lower count removes the newest nodes. Callers hold s.mu.
func (s *UpCloudService) groupNodes(key string, nodeGroup *upcloud.KubernetesNodeGroup) []upcloud.KubernetesNode {
	if s.nodes == nil {
		s.nodes = make(map[string][]upcloud.KubernetesNode)
		s.nextNode = make(map[string]int)
	}
	nodes := s.nodes[key]
	for len(nodes) < nodeGroup.Count {
		i := s.nextNode[key]
		s.nextNode[key] = i + 1
		nodes = append(nodes, upcloud.KubernetesNode{
			UUID:  fmt.Sprintf("%s-%d", nodeGroup.Name, i),
			Name:  fmt.Sprintf("%s-node-%d", nodeGroup.Name, i),
			State: upcloud.KubernetesNodeStateRunning,
		})
	}
	if len(nodes) > nodeGroup.Count {
		nodes = nodes[:nodeGroup.Count]
	}
	s.nodes[key] = nodes
	return nodes
}

// GetKubernetesCluster return UKS cluster object
func (s *UpCloudService) GetKubernetesCluster(_ context.Context, r *request.GetKubernetesClusterRequest) (*upcloud.KubernetesCluster, error) {
	if c, ok := s.Clusters[r.UUID]; ok {
		return &c, nil
	}
	return nil, &upcloud.Problem{Status: http.StatusNotFound}
}

// GetKubernetesPlans list UKS plans
func (s *UpCloudService) GetKubernetesPlans(_ context.Context, _ *request.GetKubernetesPlansRequest) ([]upcloud.KubernetesPlan, error) {
	return s.Plans, nil
}

// AppendNodeGroup is mock helper function to add new node groups during tests
func (s *UpCloudService) AppendNodeGroup(ctx context.Context, clusterID uuid.UUID, group upcloud.KubernetesNodeGroup) error {
	cluster, err := s.GetKubernetesCluster(ctx, &request.GetKubernetesClusterRequest{UUID: clusterID.String()})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cluster.NodeGroups = append(cluster.NodeGroups, group)
	s.Clusters[clusterID.String()] = *cluster
	return nil
}

// GetPlans lists server plans
func (s *UpCloudService) GetPlans(ctx context.Context) (*upcloud.Plans, error) {
	return &upcloud.Plans{Plans: s.NodePlans}, nil
}
