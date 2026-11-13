/*
Copyright 2026 The Kubernetes Authors.

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

	"github.com/google/uuid"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/upcloud/pkg/github.com/upcloudltd/upcloud-go-api/v6/upcloud/request"
)

// providerIDPrefix is the prefix of UKS node provider IDs: upcloud:////<server uuid>.
const providerIDPrefix = "upcloud:////"

// nodeNamesByUUID returns the UKS node names of a node group by server UUID.
func nodeNamesByUUID(ctx context.Context, svc upCloudService, clusterID uuid.UUID, nodeGroup string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeoutGetRequest)
	defer cancel()
	ng, err := svc.GetKubernetesNodeGroup(ctx, &request.GetKubernetesNodeGroupRequest{
		ClusterUUID: clusterID.String(),
		Name:        nodeGroup,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get node group %s nodes: %w", nodeGroup, err)
	}
	names := make(map[string]string, len(ng.Nodes))
	for _, n := range ng.Nodes {
		names[n.UUID] = n.Name
	}
	return names, nil
}

// uksNodeName returns the UKS node name of a node to delete. The UKS API deletes nodes by
// name, but for an instance that never joined Kubernetes the autoscaler passes a placeholder
// node named after the provider ID, so the name is looked up by the server UUID in the
// provider ID. ok is false when the server is not in the node group any more. Nodes without
// an UpCloud provider ID keep their own name.
func uksNodeName(node *apiv1.Node, namesByUUID map[string]string) (name string, ok bool) {
	serverUUID, found := strings.CutPrefix(node.Spec.ProviderID, providerIDPrefix)
	if !found || serverUUID == "" {
		return node.Name, true
	}
	name, ok = namesByUUID[serverUUID]
	return name, ok
}
