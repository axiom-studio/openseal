package main

import (
	"fmt"
	"os"

	skillgrpc "github.com/axiom-studio/skills.sdk/grpc"
)

const slackSkillVersion = "1.1.0"

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
	server := skillgrpc.NewSkillServer("skill-slack", slackSkillVersion)
	server.RegisterExecutor(slackIngressNodeType, &slackIngressExecutor{adapter: adapter})
	server.RegisterExecutor(slackDeliveryNodeType, &slackDeliveryExecutor{adapter: adapter})
	fmt.Printf("starting Slack Skill %s on port %s\n", slackSkillVersion, port)
	if err := server.Serve(port); err != nil {
		fmt.Fprintf(os.Stderr, "Slack Skill failed: %v\n", err)
		os.Exit(1)
	}
}
