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
	chat := ChatTarget()
	for _, schema := range []map[string]interface{}{chat.input(OpListDirectory), chat.input(OpReadFile), chat.input(OpSearchFiles)} {
		if _, ok := schema["properties"].(map[string]interface{})[ChatArgument]; !ok {
			t.Fatalf("read without chat: %v", schema)
		}
	}
	for _, schema := range []map[string]interface{}{chat.input(OpWriteFile), chat.input(OpEditFile), chat.input(OpDeleteFile), chat.input(OpMoveFile), chat.input(OpRunCommand)} {
		if _, ok := schema["properties"].(map[string]interface{})[ChatArgument]; ok {
			t.Fatalf("change names another chat: %v", schema)
		}
	}
}
