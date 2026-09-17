#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]
use openseal_daemon_host::provider::{
    load_provider, save_provider, ProviderSettings, ProviderUpdate,
};
use openseal_daemon_host::{ApiResponse, DaemonHost};
use serde::Serialize;
use serde_json::Value;
use std::{
    path::PathBuf,
    sync::{Arc, Mutex},
};
use tauri::Manager;

#[derive(Clone, Serialize)]
struct Status {
    state: String,
    message: String,
    workspace: String,
}
struct Desktop {
    host: Mutex<Option<DaemonHost>>,
    status: Mutex<Status>,
    startup: Mutex<Option<std::thread::JoinHandle<()>>>,
}

#[tauri::command]
fn desktop_status(state: tauri::State<'_, Arc<Desktop>>) -> Result<Status, String> {
    let mut status = state
        .status
        .lock()
        .map_err(|_| "Workspace status unavailable")?
        .clone();
    if status.state == "ready" {
        if let Ok(mut guard) = state.host.try_lock() {
            if let Some(host) = guard.as_mut() {
                if !host.is_running()? {
                    status.state = "error".into();
                    status.message =
                        "OpenSeal stopped. Close and reopen the app to reconnect.".into();
                }
            }
        }
    }
    Ok(status)
}

#[tauri::command]
async fn api_request(
    state: tauri::State<'_, Arc<Desktop>>,
    method: String,
    path: String,
    body: Option<Value>,
    idempotency_key: Option<String>,
) -> Result<ApiResponse, String> {
    let desktop = Arc::clone(&state);
    tauri::async_runtime::spawn_blocking(move || {
        let host = desktop
            .host
            .lock()
            .map_err(|_| "Workspace connection unavailable")?;
        host.as_ref().ok_or("OpenSeal is not ready")?.request(
            &method,
            &path,
            body,
            idempotency_key.as_deref(),
        )
    })
    .await
    .map_err(|_| "Workspace request interrupted")?
}

#[tauri::command]
async fn preview_artifact(
    state: tauri::State<'_, Arc<Desktop>>,
    id: String,
    version: u64,
) -> Result<String, String> {
    let desktop = Arc::clone(&state);
    tauri::async_runtime::spawn_blocking(move || {
        let host = desktop
            .host
            .lock()
            .map_err(|_| "Workspace connection unavailable")?;
        host.as_ref()
            .ok_or("OpenSeal is not ready")?
            .preview_artifact(&id, version)
    })
    .await
    .map_err(|_| "Artifact preview interrupted")?
}

#[tauri::command]
async fn save_artifact(
    app: tauri::AppHandle,
    window: tauri::Window,
    state: tauri::State<'_, Arc<Desktop>>,
    id: String,
    version: u64,
) -> Result<bool, String> {
    use openseal_daemon_host::artifacts::{save_verified, suggested_name, MAX_EXPORT_BYTES};
    use tauri_plugin_dialog::DialogExt;
    let desktop = Arc::clone(&state);
    tauri::async_runtime::spawn_blocking(move || {
        let metadata = {
            let host = desktop
                .host
                .lock()
                .map_err(|_| "Workspace connection unavailable")?;
            host.as_ref()
                .ok_or("OpenSeal is not ready")?
                .artifact_metadata(&id, version)?
        };
        if metadata.size_bytes > MAX_EXPORT_BYTES {
            return Err(
                "Artifacts larger than 100 MiB cannot be exported from this app yet".into(),
            );
        }
        let Some(chosen) = app
            .dialog()
            .file()
            .set_parent(&window)
            .set_title("Save artifact")
            .set_file_name(suggested_name(&metadata.name))
            .blocking_save_file()
        else {
            return Ok(false);
        };
        let path = chosen
            .into_path()
            .map_err(|_| "Choose a local destination file")?;
        let bytes = {
            let host = desktop
                .host
                .lock()
                .map_err(|_| "Workspace connection unavailable")?;
            host.as_ref()
                .ok_or("OpenSeal is not ready")?
                .artifact_bytes(&metadata, MAX_EXPORT_BYTES)?
        };
        save_verified(&metadata, &bytes, &path)?;
        Ok(true)
    })
    .await
    .map_err(|_| "Artifact export interrupted")?
}

fn daemon_executable() -> Result<PathBuf, String> {
    if cfg!(debug_assertions) {
        Ok(std::env::var_os("OPENSEAL_DAEMON_PATH")
            .map(PathBuf::from)
            .unwrap_or_else(|| PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../openseal")))
    } else {
        Ok(std::env::current_exe()
            .map_err(|_| "Cannot locate OpenSeal")?
            .parent()
            .ok_or("Missing executable directory")?
            .join(if cfg!(windows) {
                "openseal-daemon.exe"
            } else {
                "openseal-daemon"
            }))
    }
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct ProviderSaveResult {
    settings: ProviderSettings,
    reconnected: bool,
    message: String,
}

#[tauri::command]
async fn load_provider_settings(app: tauri::AppHandle) -> Result<ProviderSettings, String> {
    let directory = app
        .path()
        .app_data_dir()
        .map_err(|_| "Cannot locate workspace")?;
    tauri::async_runtime::spawn_blocking(move || load_provider(&directory))
        .await
        .map_err(|_| "Cannot load provider settings")?
}

#[tauri::command]
async fn save_provider_settings(
    app: tauri::AppHandle,
    state: tauri::State<'_, Arc<Desktop>>,
    settings: ProviderUpdate,
) -> Result<ProviderSaveResult, String> {
    let directory = app
        .path()
        .app_data_dir()
        .map_err(|_| "Cannot locate workspace")?;
    let executable = daemon_executable()?;
    let desktop = Arc::clone(&state);
    tauri::async_runtime::spawn_blocking(move || {
        if let Some(startup) = desktop.startup.lock().map_err(|_| "Workspace startup unavailable")?.take() {
            startup.join().map_err(|_| "Workspace startup interrupted")?;
        }
        let mut host = desktop.host.lock().map_err(|_| "Workspace connection unavailable")?;
        let settings = save_provider(&directory, settings)?;
        { let mut status = desktop.status.lock().map_err(|_| "Workspace status unavailable")?; status.state = "starting".into(); status.message.clear(); }
        if let Some(mut previous) = host.take() { previous.shutdown(); }
        let result = match DaemonHost::start(&executable, &directory) {
            Ok(new_host) => {
                *host = Some(new_host);
                desktop.status.lock().map_err(|_| "Workspace status unavailable")?.state = "ready".into();
                ProviderSaveResult { settings, reconnected: true, message: "Provider settings saved. Workspace reconnected.".into() }
            }
            Err(error) => {
                let mut status = desktop.status.lock().map_err(|_| "Workspace status unavailable")?;
                status.state = "error".into(); status.message = error;
                ProviderSaveResult { settings, reconnected: false, message: "Provider settings were saved, but the workspace could not restart. Check the configuration and save again.".into() }
            }
        };
        Ok(result)
    }).await.map_err(|_| "Provider update interrupted")?
}

fn main() {
    let app = tauri::Builder::default()
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_single_instance::init(|app, _, _| {
            if let Some(window) = app.get_webview_window("main") {
                let _ = window.unminimize();
                let _ = window.set_focus();
            }
        }))
        .invoke_handler(tauri::generate_handler![
            desktop_status,
            api_request,
            preview_artifact,
            save_artifact,
            load_provider_settings,
            save_provider_settings
        ])
        .setup(|app| {
            let directory = app.path().app_data_dir()?;
            let state = Arc::new(Desktop {
                host: Mutex::new(None),
                startup: Mutex::new(None),
                status: Mutex::new(Status {
                    state: "starting".into(),
                    message: String::new(),
                    workspace: directory.to_string_lossy().into(),
                }),
            });
            app.manage(Arc::clone(&state));
            let executable = daemon_executable()?;
            let startup_state = Arc::clone(&state);
            let startup =
                std::thread::spawn(move || match DaemonHost::start(&executable, &directory) {
                    Ok(host) => {
                        *startup_state.host.lock().unwrap() = Some(host);
                        startup_state.status.lock().unwrap().state = "ready".into();
                    }
                    Err(message) => {
                        let mut status = startup_state.status.lock().unwrap();
                        status.state = "error".into();
                        status.message = message;
                    }
                });
            *state.startup.lock().unwrap() = Some(startup);
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("Cannot initialize OpenSeal desktop");
    app.run(|handle, event| {
        if let tauri::RunEvent::Exit = event {
            if let Some(state) = handle.try_state::<Arc<Desktop>>() {
                // Closing during startup must not orphan a child launched by
                // the worker after the UI has already exited.
                if let Ok(mut startup) = state.startup.lock() {
                    if let Some(startup) = startup.take() {
                        let _ = startup.join();
                    }
                }
                if let Ok(mut host) = state.host.lock() {
                    if let Some(mut host) = host.take() {
                        host.shutdown();
                    }
                }
            }
        }
    });
}
