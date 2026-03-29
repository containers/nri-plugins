// Copyright 2026 Intel Corporation. All Rights Reserved.
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

package topologyaware

import (
	"testing"
)

// buildTestTree constructs a 3-level tree for traversal tests:
//
//	root (id=0)
//	├── child1 (id=1)
//	│   └── grandchild1 (id=3)
//	└── child2 (id=2)
func buildTestTree() (root, child1, child2, grandchild1 *node) {
	root = &node{name: "root", id: 0, kind: VirtualNode, depth: 0, parent: nilnode}
	child1 = &node{name: "child1", id: 1, kind: SocketNode, depth: 1, parent: root}
	child2 = &node{name: "child2", id: 2, kind: SocketNode, depth: 1, parent: root}
	grandchild1 = &node{name: "grandchild1", id: 3, kind: NumaNode, depth: 2, parent: child1}

	root.children = []Node{child1, child2}
	child1.children = []Node{grandchild1}
	return
}

func TestNodeDepthFirst(t *testing.T) {
	root, child1, child2, grandchild1 := buildTestTree()

	var visited []int
	root.DepthFirst(func(n Node) (done bool) {
		visited = append(visited, n.NodeID())
		return false
	})

	// DepthFirst visits children before their parent.
	expected := []int{grandchild1.id, child1.id, child2.id, root.id}
	if len(visited) != len(expected) {
		t.Fatalf("expected %d nodes visited, got %d: %v", len(expected), len(visited), visited)
	}
	for i, id := range expected {
		if visited[i] != id {
			t.Errorf("visited[%d] = %d, want %d", i, visited[i], id)
		}
	}
}

func TestNodeBreadthFirst(t *testing.T) {
	root, child1, child2, grandchild1 := buildTestTree()

	var visited []int
	root.BreadthFirst(func(n Node) (done bool) {
		visited = append(visited, n.NodeID())
		return false
	})

	// BreadthFirst visits a node before recursing into each child (pre-order DFS).
	// root → child1 → grandchild1 → child2
	expected := []int{root.id, child1.id, grandchild1.id, child2.id}
	if len(visited) != len(expected) {
		t.Fatalf("expected %d nodes visited, got %d: %v", len(expected), len(visited), visited)
	}
	for i, id := range expected {
		if visited[i] != id {
			t.Errorf("visited[%d] = %d, want %d", i, visited[i], id)
		}
	}
}

func TestNodeLinkParent(t *testing.T) {
	parent := &node{name: "parent", id: 0, kind: VirtualNode, depth: 2, parent: nilnode}
	child := &node{name: "child", id: 1, kind: SocketNode, depth: 0, parent: nilnode}

	child.LinkParent(parent)

	if child.parent != parent {
		t.Error("child.parent not updated after LinkParent")
	}
	if child.depth != parent.depth+1 {
		t.Errorf("child.depth = %d, want %d", child.depth, parent.depth+1)
	}
	if len(parent.children) != 1 || parent.children[0] != child {
		t.Error("child not added to parent.children after LinkParent")
	}
}

func TestNodeAddChildren(t *testing.T) {
	parent := &node{name: "parent", id: 0, kind: VirtualNode, parent: nilnode}
	c1 := &node{name: "c1", id: 1, parent: nilnode}
	c2 := &node{name: "c2", id: 2, parent: nilnode}

	parent.AddChildren([]Node{c1, c2})

	if len(parent.children) != 2 {
		t.Fatalf("expected 2 children, got %d", len(parent.children))
	}
	if parent.children[0] != c1 || parent.children[1] != c2 {
		t.Error("children not appended in order")
	}
}

func TestNodeIsRootNode(t *testing.T) {
	root := &node{name: "root", id: 0, kind: VirtualNode, parent: nilnode}
	child := &node{name: "child", id: 1, kind: SocketNode, parent: root}

	if !root.IsRootNode() {
		t.Error("root.IsRootNode() = false, want true")
	}
	if child.IsRootNode() {
		t.Error("child.IsRootNode() = true, want false")
	}
}

func TestNodeIsLeafNode(t *testing.T) {
	root, _, _, _ := buildTestTree()
	leaf := &node{name: "leaf", id: 99, kind: NumaNode, parent: root}

	if root.IsLeafNode() {
		t.Error("root.IsLeafNode() = true, want false")
	}
	if !leaf.IsLeafNode() {
		t.Error("leaf.IsLeafNode() = false, want true")
	}
}

func TestNodeRootDistance(t *testing.T) {
	root, child1, _, grandchild1 := buildTestTree()

	cases := []struct {
		n    *node
		want int
	}{
		{root, 0},
		{child1, 1},
		{grandchild1, 2},
	}
	for _, tc := range cases {
		if got := tc.n.RootDistance(); got != tc.want {
			t.Errorf("%s.RootDistance() = %d, want %d", tc.n.name, got, tc.want)
		}
	}
}

func TestNodeParent(t *testing.T) {
	root, child1, _, _ := buildTestTree()

	if root.Parent() != nilnode {
		t.Error("root.Parent() should be nilnode")
	}
	if child1.Parent() != root {
		t.Errorf("child1.Parent() = %v, want root", child1.Parent())
	}
}

func TestNodeChildren(t *testing.T) {
	root, child1, child2, _ := buildTestTree()

	children := root.Children()
	if len(children) != 2 {
		t.Fatalf("root.Children() len = %d, want 2", len(children))
	}
	if children[0] != child1 || children[1] != child2 {
		t.Error("root.Children() returned unexpected nodes")
	}
}

func TestNodeDepthFirstEarlyExit(t *testing.T) {
	root, child1, _, grandchild1 := buildTestTree()

	// Stop as soon as child1 is visited. DepthFirst is post-order, so the visit
	// order would be: grandchild1 → child1 (stop here) → child2 → root.
	// child2 and root must NOT appear in the visited list.
	var visited []int
	done := root.DepthFirst(func(n Node) bool {
		visited = append(visited, n.NodeID())
		return n.NodeID() == child1.id
	})

	if !done {
		t.Error("DepthFirst should return true when the callback requests early exit")
	}
	expected := []int{grandchild1.id, child1.id}
	if len(visited) != len(expected) {
		t.Fatalf("expected %d nodes visited, got %d: %v", len(expected), len(visited), visited)
	}
	for i, id := range expected {
		if visited[i] != id {
			t.Errorf("visited[%d] = %d, want %d", i, visited[i], id)
		}
	}
}

func TestNodeBreadthFirstEarlyExit(t *testing.T) {
	root, child1, _, _ := buildTestTree()

	// Stop as soon as child1 is visited. BreadthFirst is pre-order, so the visit
	// order would be: root → child1 (stop here) → grandchild1 → child2.
	// grandchild1 and child2 must NOT appear in the visited list.
	var visited []int
	done := root.BreadthFirst(func(n Node) bool {
		visited = append(visited, n.NodeID())
		return n.NodeID() == child1.id
	})

	if !done {
		t.Error("BreadthFirst should return true when the callback requests early exit")
	}
	expected := []int{root.id, child1.id}
	if len(visited) != len(expected) {
		t.Fatalf("expected %d nodes visited, got %d: %v", len(expected), len(visited), visited)
	}
	for i, id := range expected {
		if visited[i] != id {
			t.Errorf("visited[%d] = %d, want %d", i, visited[i], id)
		}
	}
}

func TestNodeIsSameNode(t *testing.T) {
	root, child1, _, _ := buildTestTree()

	if !root.IsSameNode(root) {
		t.Error("node should be the same as itself")
	}
	if root.IsSameNode(child1) {
		t.Error("different nodes should not be the same")
	}
}
