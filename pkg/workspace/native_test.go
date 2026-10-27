package workspace

import "testing"

func TestOperationsAreAChatsFileTools(t *testing.T) {
	if Operations(nil) != nil || Operations(&Authority{}) != nil {
		t.Fatal("operations offered without a chat")
	}
	names := map[string]bool{}
	for _, operation := range Operations(ChatAuthority("chat-1")) {
		names[operation.Name] = true
	}
	for _, name := range []string{OperationReadFile, OperationWriteFile, OperationRunCommand, OperationListChats, OperationCopyFromChat} {
		if !names[name] {
			t.Fatalf("missing %s in %v", name, names)
		}
	}
}

func TestReadsCanNameAnotherChatButWritesCannot(t *testing.T) {
	for _, schema := range []map[string]interface{}{listInput(), pathInput(), searchInput()} {
		if _, ok := schema["properties"].(map[string]interface{})[ChatArgument]; !ok {
			t.Fatalf("read without chat: %v", schema)
		}
	}
	for _, schema := range []map[string]interface{}{writeInput(), editInput(), pathOnlyInput(), moveInput(), shellInput()} {
		if _, ok := schema["properties"].(map[string]interface{})[ChatArgument]; ok {
			t.Fatalf("change names another chat: %v", schema)
		}
	}
}
