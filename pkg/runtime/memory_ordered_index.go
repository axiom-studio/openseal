package runtime

// memoryOrderedIndexNode is the development store's ordered index primitive.
// AVL balance bounds insertion, deletion and key seeks independently of caller
// key distribution. Database stores use corresponding persistent B-tree indexes.
type memoryOrderedIndexNode[K any] struct {
	key         K
	left, right *memoryOrderedIndexNode[K]
	height      int
}

func memoryOrderedIndexHeight[K any](node *memoryOrderedIndexNode[K]) int {
	if node == nil {
		return 0
	}
	return node.height
}

func memoryOrderedIndexUpdateHeight[K any](node *memoryOrderedIndexNode[K]) {
	node.height = 1 + max(memoryOrderedIndexHeight(node.left), memoryOrderedIndexHeight(node.right))
}

func memoryOrderedIndexRotateLeft[K any](node *memoryOrderedIndexNode[K]) *memoryOrderedIndexNode[K] {
	root := node.right
	node.right, root.left = root.left, node
	memoryOrderedIndexUpdateHeight(node)
	memoryOrderedIndexUpdateHeight(root)
	return root
}

func memoryOrderedIndexRotateRight[K any](node *memoryOrderedIndexNode[K]) *memoryOrderedIndexNode[K] {
	root := node.left
	node.left, root.right = root.right, node
	memoryOrderedIndexUpdateHeight(node)
	memoryOrderedIndexUpdateHeight(root)
	return root
}

func memoryOrderedIndexBalance[K any](node *memoryOrderedIndexNode[K]) *memoryOrderedIndexNode[K] {
	if node == nil {
		return nil
	}
	memoryOrderedIndexUpdateHeight(node)
	if memoryOrderedIndexHeight(node.left)-memoryOrderedIndexHeight(node.right) > 1 {
		if memoryOrderedIndexHeight(node.left.left) < memoryOrderedIndexHeight(node.left.right) {
			node.left = memoryOrderedIndexRotateLeft(node.left)
		}
		return memoryOrderedIndexRotateRight(node)
	}
	if memoryOrderedIndexHeight(node.right)-memoryOrderedIndexHeight(node.left) > 1 {
		if memoryOrderedIndexHeight(node.right.right) < memoryOrderedIndexHeight(node.right.left) {
			node.right = memoryOrderedIndexRotateRight(node.right)
		}
		return memoryOrderedIndexRotateLeft(node)
	}
	return node
}

func memoryOrderedIndexInsert[K any](node *memoryOrderedIndexNode[K], key K, less func(K, K) bool) *memoryOrderedIndexNode[K] {
	if node == nil {
		return &memoryOrderedIndexNode[K]{key: key, height: 1}
	}
	if less(key, node.key) {
		node.left = memoryOrderedIndexInsert(node.left, key, less)
	} else if less(node.key, key) {
		node.right = memoryOrderedIndexInsert(node.right, key, less)
	} else {
		return node
	}
	return memoryOrderedIndexBalance(node)
}

func memoryOrderedIndexDelete[K any](node *memoryOrderedIndexNode[K], key K, less func(K, K) bool) *memoryOrderedIndexNode[K] {
	if node == nil {
		return nil
	}
	if less(key, node.key) {
		node.left = memoryOrderedIndexDelete(node.left, key, less)
	} else if less(node.key, key) {
		node.right = memoryOrderedIndexDelete(node.right, key, less)
	} else {
		if node.left == nil {
			return node.right
		}
		if node.right == nil {
			return node.left
		}
		successor := node.right
		for successor.left != nil {
			successor = successor.left
		}
		node.key = successor.key
		node.right = memoryOrderedIndexDelete(node.right, successor.key, less)
	}
	return memoryOrderedIndexBalance(node)
}

func memoryOrderedIndexFirst[K any](node *memoryOrderedIndexNode[K]) (K, bool) {
	if node == nil {
		var zero K
		return zero, false
	}
	for node.left != nil {
		node = node.left
	}
	return node.key, true
}

// before must describe a prefix of the index ordering. after is an exclusive
// cursor, and limit bounds returned candidates rather than matching mutations.
func memoryOrderedIndexAfter[K any](root *memoryOrderedIndexNode[K], after *K, before func(K) bool, limit int, less func(K, K) bool) []K {
	if root == nil || limit <= 0 {
		return []K{}
	}
	result := make([]K, 0, limit)
	var visit func(*memoryOrderedIndexNode[K])
	visit = func(node *memoryOrderedIndexNode[K]) {
		if node == nil || len(result) == limit {
			return
		}
		if before != nil && !before(node.key) {
			visit(node.left)
			return
		}
		if after != nil && !less(*after, node.key) {
			visit(node.right)
			return
		}
		visit(node.left)
		if len(result) < limit {
			result = append(result, node.key)
			visit(node.right)
		}
	}
	visit(root)
	return result
}
