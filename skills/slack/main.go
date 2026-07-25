package main

import (
	"fmt"
	"os"

	skillgrpc "github.com/axiom-studio/skills.sdk/grpc"
)

const (
	slackConversationSkillID      = "openseal.slack-conversations"
	slackConversationSkillVersion = "1.0.1"
)

func main() {
	port := os.Getenv("SKILL_PORT")
	if port == "" {
		port = "50051"
	}
	adapter := newSlackAdapter(
		os.Getenv("SLACK_SIGNING_SECRET"),
		os.Getenv("SLACK_API_BASE_URL"),
		nil,
	)
	server := skillgrpc.NewSkillServer(slackConversationSkillID, slackConversationSkillVersion)
	server.RegisterExecutor(slackIngressNodeType, &slackIngressExecutor{adapter: adapter})
	server.RegisterExecutor(slackDeliveryNodeType, &slackDeliveryExecutor{adapter: adapter})
	fmt.Printf("starting Slack Conversations Skill %s on port %s\n", slackConversationSkillVersion, port)
	if err := server.Serve(port); err != nil {
		fmt.Fprintf(os.Stderr, "Slack Skill failed: %v\n", err)
		os.Exit(1)
	}
}
