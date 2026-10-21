package runtime

import (
	"strings"
	"testing"
)

// A routine may keep an app from its own chat up to date; each occurrence is
// told which app to save to. An app from another chat or owner is refused.
func TestScheduledTaskKeepsOnlyItsChatsAppUpToDate(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			store := workflowEvidenceStore(t, kind)
			run, _, _, args := scheduledTaskFixture(t, store)
			conversationID, _ := run.Context[conversationRunContextConversationID].(string)
			register := func(id string, owner ObjectiveOwner, conversation, artifactType string) {
				t.Helper()
				if err := store.(ArtifactStore).CreateArtifactVersion(t.Context(), &Artifact{
					ID: id, Version: 1, Scope: run.Scope, Name: "App.json", Type: artifactType, MediaType: "application/vnd.openseal.app+json",
					ContentRef: "ref-" + id, Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 10, Classification: ArtifactClassificationInternal,
					Metadata:   map[string]interface{}{"conversationId": conversation},
					Provenance: ArtifactProvenance{Producer: ActivityActor{Type: "agent", ID: run.AssignedAgentID}, Owner: &owner, RunID: run.ID},
				}, 0); err != nil {
					t.Fatal(err)
				}
			}
			ours := "app-" + strings.Repeat("1", 64)
			register(ours, run.Owner, conversationID, "app")
			register("app-"+strings.Repeat("2", 64), run.Owner, "another-chat", "app")
			register("app-"+strings.Repeat("3", 64), ObjectiveOwner{Type: OwnerTypeAgent, ID: "someone-else"}, conversationID, "app")
			register("app-"+strings.Repeat("4", 64), run.Owner, conversationID, "page")

			withApp := func(appID string) map[string]interface{} {
				copy := map[string]interface{}{}
				for key, value := range args {
					copy[key] = value
				}
				copy["appId"] = appID
				return copy
			}
			_, task, _, err := resolveScheduledTaskCreate(t.Context(), store.(runbookActionStore), run, withApp(ours))
			if err != nil || task.AppID != ours {
				t.Fatalf("own app: task=%#v err=%v", task, err)
			}
			goal := activationGoal(&RunbookActivation{Task: task}, nil)
			if !strings.Contains(goal, ours) || !strings.Contains(goal, "save_app_data") {
				t.Fatalf("occurrence goal does not name the app: %q", goal)
			}
			for _, foreign := range []string{"app-" + strings.Repeat("2", 64), "app-" + strings.Repeat("3", 64), "app-" + strings.Repeat("4", 64), "app-" + strings.Repeat("5", 64), "site-1"} {
				if _, _, _, err := resolveScheduledTaskCreate(t.Context(), store.(runbookActionStore), run, withApp(foreign)); err == nil {
					t.Fatalf("routine accepted app %s", foreign)
				}
			}
		})
	}
}
