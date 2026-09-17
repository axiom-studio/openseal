use openseal_daemon_host::DaemonHost;
use std::path::PathBuf;

/// Run explicitly with OPENSEAL_TEST_DAEMON pointing at a freshly built Go binary.
#[test]
#[ignore = "requires OPENSEAL_TEST_DAEMON"]
fn starts_authenticates_stops_and_reopens_durable_workspace() {
    let executable =
        PathBuf::from(std::env::var_os("OPENSEAL_TEST_DAEMON").expect("set OPENSEAL_TEST_DAEMON"));
    let workspace =
        std::env::temp_dir().join(format!("openseal-host-test-{}", uuid::Uuid::new_v4()));
    std::fs::create_dir_all(&workspace).unwrap();
    {
        let mut host = DaemonHost::start(&executable, &workspace).expect("daemon startup");
        assert!(host.is_running().unwrap());
        let health = host.request("GET", "/api/v1/health", None, None).unwrap();
        assert_eq!(health.status, 200);
        let capabilities = host
            .request("GET", "/api/v1/capabilities", None, None)
            .unwrap();
        assert_eq!(capabilities.status, 200);
        assert_eq!(capabilities.body["apiVersion"], "agent-kernel/v1");
        assert!(workspace.join("daemon.yaml").is_file());
        assert!(workspace.join("data/openseal.db").is_file());
        let objective = host
            .request(
                "POST",
                "/api/v1/objectives",
                Some(serde_json::json!({
                    "scope": {"kind": "local", "id": "default"},
                    "owner": {"type": "team", "id": "research"},
                    "title": "Persistence verification",
                    "goal": "Keep project state across desktop restarts",
                    "status": "active"
                })),
                Some("desktop-objective-test"),
            )
            .unwrap();
        assert_eq!(objective.status, 201, "{}", objective.body);
        let created = host
            .request(
                "POST",
                "/api/v1/projects",
                Some(serde_json::json!({
                    "id": "desktop-persistence-test",
                    "scope": {"kind": "local", "id": "default"},
                    "owner": {"type": "team", "id": "research"},
                    "title": "Desktop persistence",
                    "purpose": "Verify work survives closing the app",
                    "objectiveRefs": [objective.body["id"]],
                    "status": "active"
                })),
                Some("desktop-persistence-test"),
            )
            .unwrap();
        assert_eq!(created.status, 201, "{}", created.body);
        host.shutdown();
        assert!(!host.is_running().unwrap());
        // Idempotent cleanup must never signal another process.
        host.shutdown();
    }
    {
        let mut host = DaemonHost::start(&executable, &workspace).expect("reopen workspace");
        assert!(host.is_running().unwrap());
        let restored = host
            .request(
                "GET",
                "/api/v1/projects/desktop-persistence-test?scopeKind=local&scopeId=default",
                None,
                None,
            )
            .unwrap();
        assert_eq!(restored.status, 200, "{}", restored.body);
        assert_eq!(restored.body["title"], "Desktop persistence");
        host.shutdown();
        assert!(!host.is_running().unwrap());
    }
    std::fs::remove_dir_all(workspace).unwrap();
}
