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
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// newDeleteTestGroup returns node group "group1" (2 nodes: UUIDs group1-0, group1-1,
// UKS names group1-node-0, group1-node-1) backed by the mock service.
func newDeleteTestGroup(t *testing.T) (*upCloudNodeGroup, func() []string) {
	t.Helper()
	clusterID := uuid.New()
	svc := newMockService(clusterID)
	kng := svc.Clusters[clusterID.String()].NodeGroups[0]
	require.Equal(t, "group1", kng.Name)
	require.Equal(t, 2, kng.Count)
	g := &upCloudNodeGroup{size: kng.Count, maxSize: 20, name: kng.Name, svc: svc, clusterID: clusterID}
	return g, func() []string { return svc.DeletedNodes }
}

func node(name, providerID string) *apiv1.Node {
	return &apiv1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: apiv1.NodeSpec{ProviderID: providerID}}
}

// The autoscaler deletes an instance that never joined Kubernetes through a placeholder
// node whose name is the provider ID (clusterstate.FakeNode).
func TestDeleteNodes_NeverRegisteredInstance(t *testing.T) {
	t.Parallel()

	g, deleted := newDeleteTestGroup(t)
	require.NoError(t, g.DeleteNodes([]*apiv1.Node{node("upcloud:////group1-1", "upcloud:////group1-1")}))
	require.Equal(t, []string{"group1-node-1"}, deleted())
	size, err := g.TargetSize()
	require.NoError(t, err)
	require.Equal(t, 1, size)
}

func TestDeleteNodes_RegisteredNode(t *testing.T) {
	t.Parallel()

	g, deleted := newDeleteTestGroup(t)
	require.NoError(t, g.DeleteNodes([]*apiv1.Node{node("group1-node-0", "upcloud:////group1-0")}))
	require.Equal(t, []string{"group1-node-0"}, deleted())
}

func TestDeleteNodes_Batch(t *testing.T) {
	t.Parallel()

	g, deleted := newDeleteTestGroup(t)
	require.NoError(t, g.DeleteNodes([]*apiv1.Node{
		node("group1-node-0", "upcloud:////group1-0"),
		node("upcloud:////group1-1", "upcloud:////group1-1"),
	}))
	require.Equal(t, []string{"group1-node-0", "group1-node-1"}, deleted())
	size, err := g.TargetSize()
	require.NoError(t, err)
	require.Equal(t, 0, size)
}

func TestDeleteNodes_ServerNotInGroup(t *testing.T) {
	t.Parallel()

	g, deleted := newDeleteTestGroup(t)
	require.NoError(t, g.DeleteNodes([]*apiv1.Node{node("upcloud:////gone", "upcloud:////gone")}))
	require.Empty(t, deleted(), "a server that is gone must not be deleted again")
	size, err := g.TargetSize()
	require.NoError(t, err)
	require.Equal(t, 2, size)
}

func TestDeleteNodes_WithoutProviderID(t *testing.T) {
	t.Parallel()

	g, deleted := newDeleteTestGroup(t)
	require.NoError(t, g.DeleteNodes([]*apiv1.Node{node("group1-node-1", "")}))
	require.Equal(t, []string{"group1-node-1"}, deleted())

	g, deleted = newDeleteTestGroup(t)
	require.Error(t, g.DeleteNodes([]*apiv1.Node{node("unknown-node", "")}), "the API returns 404 for unknown names")
	require.Empty(t, deleted())
}

func TestDeleteNodes_NodeGroupLookupFails(t *testing.T) {
	t.Parallel()

	g, deleted := newDeleteTestGroup(t)
	g.name = "missing"
	require.Error(t, g.DeleteNodes([]*apiv1.Node{node("upcloud:////group1-1", "upcloud:////group1-1")}))
	require.Empty(t, deleted())
}

func TestUKSNodeName(t *testing.T) {
	t.Parallel()

	names := map[string]string{"uuid-1": "group-a-xyz"}
	for _, tc := range []struct {
		node   *apiv1.Node
		name   string
		inList bool
	}{
		{node("upcloud:////uuid-1", "upcloud:////uuid-1"), "group-a-xyz", true},
		{node("group-a-xyz", "upcloud:////uuid-1"), "group-a-xyz", true},
		{node("upcloud:////uuid-2", "upcloud:////uuid-2"), "", false},
		{node("group-a-abc", ""), "group-a-abc", true},
		{node("other", "aws:///eu-west-1/i-1"), "other", true},
	} {
		name, ok := uksNodeName(tc.node, names)
		require.Equal(t, tc.inList, ok, tc.node.Name)
		require.Equal(t, tc.name, name, tc.node.Name)
	}
}
