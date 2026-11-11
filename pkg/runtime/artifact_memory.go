package runtime

func artifactStorageKey(scope Scope, id string) string {
	return scope.Kind + "\x00" + scope.ID + "\x00" + id
}
