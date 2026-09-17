//! Native-only ownership of the desktop daemon and its credentials.
//! Call blocking operations on a worker thread, never the UI event loop.

pub mod artifacts;
pub mod provider;

use reqwest::{blocking::Client, header, Method, Url};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::{
    io::{BufRead, BufReader, Read},
    path::Path,
    process::{Child, Command, Stdio},
    sync::mpsc,
    thread,
    time::{Duration, Instant},
};

const STARTUP_TIMEOUT: Duration = Duration::from_secs(30);
const MAX_RESPONSE_BYTES: u64 = 16 * 1024 * 1024;

#[derive(Deserialize)]
struct Readiness {
    #[serde(rename = "type")]
    kind: String,
    endpoint: String,
}

/// Only these response fields may cross the native/webview boundary.
#[derive(Debug, Serialize)]
pub struct ApiResponse {
    pub status: u16,
    pub body: Value,
}

/// Owns exactly one process. Dropping it also reaps the process on startup error.
/// Deliberately does not implement Debug: the token must not enter diagnostics.
pub struct DaemonHost {
    child: Child,
    endpoint: Url,
    client: Client,
    token: String,
}

impl DaemonHost {
    pub fn start(executable: &Path, data_dir: &Path) -> Result<Self, String> {
        let executable = executable
            .canonicalize()
            .map_err(|e| format!("Cannot find the OpenSeal daemon: {e}"))?;
        std::fs::create_dir_all(data_dir).map_err(|e| format!("Cannot create workspace: {e}"))?;
        let data_dir = data_dir
            .canonicalize()
            .map_err(|e| format!("Cannot open workspace: {e}"))?;
        let token = format!(
            "{}{}",
            uuid::Uuid::new_v4().simple(),
            uuid::Uuid::new_v4().simple()
        );
        let client = Client::builder()
            .no_proxy()
            .redirect(reqwest::redirect::Policy::none())
            .connect_timeout(Duration::from_secs(3))
            .timeout(Duration::from_secs(30))
            .build()
            .map_err(|e| format!("Cannot initialize local connection: {e}"))?;
        let mut command = Command::new(executable);
        command
            .arg("daemon")
            .arg("--config")
            .arg(data_dir.join("daemon.yaml"))
            .arg("--context")
            .arg(data_dir.join("context.yaml"))
            .arg("--listen")
            .arg("127.0.0.1:0")
            .arg("--standalone-operator")
            .arg("--desktop-operator")
            .env("OPENSEAL_API_TOKEN", &token)
            .current_dir(&data_dir)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::null());
        #[cfg(windows)]
        {
            use std::os::windows::process::CommandExt;
            command.creation_flags(0x08000000); // CREATE_NO_WINDOW
        }
        let child = command
            .spawn()
            .map_err(|e| format!("Cannot start OpenSeal: {e}"))?;
        let mut host = Self {
            child,
            endpoint: Url::parse("http://127.0.0.1").expect("static URL"),
            client,
            token,
        };
        let stdout = host
            .child
            .stdout
            .take()
            .ok_or("Daemon output unavailable")?;
        let (sender, receiver) = mpsc::sync_channel(1);
        thread::spawn(move || {
            // Continue draining after readiness so a child cannot block on a full pipe.
            let mut reader = BufReader::new(stdout);
            loop {
                let mut line = String::new();
                match reader.by_ref().take(8192).read_line(&mut line) {
                    Ok(0) | Err(_) => break,
                    Ok(_) if !line.ends_with('\n') => break,
                    Ok(_) => {
                        if let Some(payload) = line.strip_prefix("OPENSEAL_DAEMON ") {
                            let result = serde_json::from_str::<Readiness>(payload)
                                .map_err(|_| "Invalid daemon readiness message".to_string());
                            let _ = sender.try_send(result);
                        }
                    }
                }
            }
        });
        let ready = receiver.recv_timeout(STARTUP_TIMEOUT).map_err(|_| {
            "OpenSeal did not become ready. Check the workspace configuration.".to_string()
        })??;
        host.endpoint = readiness_endpoint(ready)?;
        let health = host.request("GET", "/api/v1/health", None, None)?;
        if health.status != 200 {
            return Err("OpenSeal could not verify its local connection".into());
        }
        Ok(host)
    }

    /// Accept only relative kernel API paths. No arbitrary URLs, redirect following,
    /// proxy environment, or caller-supplied authentication headers are permitted.
    pub fn request(
        &self,
        method: &str,
        path: &str,
        body: Option<Value>,
        idempotency_key: Option<&str>,
    ) -> Result<ApiResponse, String> {
        let url = api_url(&self.endpoint, path)?;
        let method = match method {
            "GET" => Method::GET,
            "POST" => Method::POST,
            "PUT" => Method::PUT,
            "PATCH" => Method::PATCH,
            "DELETE" => Method::DELETE,
            _ => return Err("Unsupported API method".into()),
        };
        let mut request = self
            .client
            .request(method, url)
            .bearer_auth(&self.token)
            .header(header::ACCEPT, "application/json");
        if let Some(body) = body {
            request = request.json(&body);
        }
        if let Some(key) = idempotency_key {
            let key = header::HeaderValue::from_str(key).map_err(|_| "Invalid request key")?;
            request = request.header("Idempotency-Key", key);
        }
        let response = request.send().map_err(|e| {
            if e.is_timeout() {
                "OpenSeal took too long to respond. Check the work status before retrying."
                    .to_string()
            } else {
                "The connection to OpenSeal was interrupted.".to_string()
            }
        })?;
        let status = response.status().as_u16();
        let mut bytes = Vec::new();
        response
            .take(MAX_RESPONSE_BYTES + 1)
            .read_to_end(&mut bytes)
            .map_err(|_| "Cannot read the OpenSeal response")?;
        if bytes.len() as u64 > MAX_RESPONSE_BYTES {
            return Err("OpenSeal response exceeds the display limit".into());
        }
        let body = if bytes.is_empty() {
            Value::Null
        } else {
            serde_json::from_slice(&bytes)
                .map_err(|_| "OpenSeal returned an invalid JSON response")?
        };
        Ok(ApiResponse { status, body })
    }

    pub fn is_running(&mut self) -> Result<bool, String> {
        self.child
            .try_wait()
            .map(|status| status.is_none())
            .map_err(|_| "Cannot inspect OpenSeal process".into())
    }

    pub fn shutdown(&mut self) {
        if matches!(self.child.try_wait(), Ok(Some(_))) {
            return;
        }
        #[cfg(unix)]
        unsafe {
            // This PID is owned by Child and has not been reaped or handed off.
            libc::kill(self.child.id() as libc::pid_t, libc::SIGTERM);
        }
        #[cfg(not(unix))]
        let _ = self.child.kill();
        let deadline = Instant::now() + Duration::from_secs(5);
        while Instant::now() < deadline {
            if matches!(self.child.try_wait(), Ok(Some(_))) {
                return;
            }
            thread::sleep(Duration::from_millis(25));
        }
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

impl Drop for DaemonHost {
    fn drop(&mut self) {
        self.shutdown();
    }
}

fn readiness_endpoint(ready: Readiness) -> Result<Url, String> {
    let url = Url::parse(&ready.endpoint).map_err(|_| "Invalid daemon endpoint")?;
    if ready.kind != "ready"
        || url.scheme() != "http"
        || url.host_str() != Some("127.0.0.1")
        || url.port().is_none()
        || url.port() == Some(0)
        || url.path() != "/"
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err("Daemon readiness must identify its loopback port".into());
    }
    Ok(url)
}

fn api_url(endpoint: &Url, path: &str) -> Result<Url, String> {
    if !path.starts_with("/api/v1/")
        || path.contains(['\\', '#'])
        || path.chars().any(char::is_control)
    {
        return Err("Invalid kernel API path".into());
    }
    let url = endpoint.join(path).map_err(|_| "Invalid kernel API path")?;
    if url.origin() != endpoint.origin() || !url.path().starts_with("/api/v1/") {
        return Err("Invalid kernel API path".into());
    }
    Ok(url)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rejects_endpoints_outside_owned_loopback_listener() {
        for endpoint in [
            "https://127.0.0.1:8080",
            "http://example.com:8080",
            "http://127.0.0.1:0",
            "http://127.0.0.1",
            "http://user@127.0.0.1:8080",
            "http://127.0.0.1:8080/path",
        ] {
            assert!(
                readiness_endpoint(Readiness {
                    kind: "ready".into(),
                    endpoint: endpoint.into()
                })
                .is_err(),
                "{endpoint}"
            );
        }
        assert!(readiness_endpoint(Readiness {
            kind: "ready".into(),
            endpoint: "http://127.0.0.1:23456".into()
        })
        .is_ok());
    }

    #[test]
    fn bridge_cannot_escape_kernel_api_origin_or_prefix() {
        let base = Url::parse("http://127.0.0.1:23456").unwrap();
        for path in [
            "https://example.com",
            "//example.com",
            "/api/v1/../../../private",
            "/api/v1/%2e%2e/%2e%2e/private",
            "/api/v1/health#fragment",
            "/api/v1/health\r\nInjected: header",
            "/api/v1/\\example.com",
        ] {
            assert!(api_url(&base, path).is_err(), "{path}");
        }
        assert_eq!(
            api_url(&base, "/api/v1/agent-runs?scopeKind=local&scopeId=default")
                .unwrap()
                .host_str(),
            Some("127.0.0.1")
        );
    }
}
