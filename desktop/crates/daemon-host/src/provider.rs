//! Provider preferences exposed to the interface without returning saved secrets.
use reqwest::Url;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::{
    fs,
    io::Write,
    path::{Path, PathBuf},
};

const CREDENTIAL_ID: &str = "desktop-authoring-model";

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ProviderSettings {
    pub base_url: String,
    pub model: String,
    pub has_api_key: bool,
}

// Never derive Debug: this request contains a user-entered secret.
#[derive(Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct ProviderUpdate {
    pub base_url: String,
    pub model: String,
    #[serde(default)]
    pub api_key: String,
}

// Never derive Debug: this request carries a Skill credential.
#[derive(Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct SkillCredentialUpdate {
    pub kind: String,
    pub binding_key: String,
    pub display_name: String,
    pub secret: String,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SkillCredentialSaved {
    pub kind: String,
    pub id: String,
    pub display_name: String,
}

fn context(data_dir: &Path) -> Result<Value, String> {
    let path = data_dir.join("context.yaml");
    match fs::symlink_metadata(&path) {
        Ok(meta) if meta.file_type().is_symlink() || !meta.is_file() => {
            return Err("The workspace context must be a regular file, not a link.".into())
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            return Ok(
                json!({"apiVersion":"openseal.dev/v1alpha1","kind":"StandaloneContext","credentials":[]}),
            )
        }
        Err(_) => return Err("Cannot read workspace provider settings.".into()),
        _ => {}
    }
    let bytes = fs::read(path).map_err(|_| "Cannot read workspace provider settings.")?;
    let value: Value = serde_yaml::from_slice(&bytes).map_err(|_| {
        "The existing context.yaml has invalid syntax. Repair it before saving provider settings."
    })?;
    if !value.is_object() {
        return Err("The existing context.yaml must contain a configuration object.".into());
    }
    Ok(value)
}

fn authoring_credential(value: &Value) -> Option<&Value> {
    let reference = &value["authoring"]["credential"];
    value["credentials"].as_array()?.iter().find(|c| {
        c["scope"]["kind"] == "local"
            && c["scope"]["id"] == "default"
            && c["kind"] == reference["kind"]
            && c["id"] == reference["id"]
    })
}

fn credential_available(data_dir: &Path, credential: &Value) -> bool {
    if let Some(name) = credential["env"].as_str() {
        return std::env::var(name).is_ok_and(|s| !s.trim().is_empty());
    }
    credential["file"].as_str().is_some_and(|file| {
        fs::symlink_metadata(data_dir.join(file)).is_ok_and(|m| {
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                if m.permissions().mode() & 0o077 != 0 {
                    return false;
                }
            }
            m.is_file() && !m.file_type().is_symlink() && m.len() > 0
        })
    })
}

pub fn load_provider(data_dir: &Path) -> Result<ProviderSettings, String> {
    let value = context(data_dir)?;
    Ok(ProviderSettings {
        base_url: value["authoring"]["baseURL"]
            .as_str()
            .unwrap_or("https://api.openai.com/v1")
            .into(),
        model: value["authoring"]["model"].as_str().unwrap_or("").into(),
        has_api_key: authoring_credential(&value)
            .is_some_and(|c| credential_available(data_dir, c)),
    })
}

fn write_private(path: &Path, bytes: &[u8]) -> Result<(), String> {
    let mut options = fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let mut file = options
        .open(path)
        .map_err(|_| "Cannot create the private provider settings file.")?;
    file.write_all(bytes)
        .and_then(|_| file.sync_all())
        .map_err(|_| "Cannot save provider settings.".into())
}

pub fn save_provider(data_dir: &Path, update: ProviderUpdate) -> Result<ProviderSettings, String> {
    let base_url = update.base_url.trim().trim_end_matches('/');
    let parsed = Url::parse(base_url)
        .map_err(|_| "Enter a complete provider URL, such as https://api.openai.com/v1.")?;
    let local = matches!(parsed.host_str(), Some("localhost" | "127.0.0.1" | "[::1]"));
    if parsed.host_str().is_none()
        || (parsed.scheme() != "https" && !(local && parsed.scheme() == "http"))
        || !parsed.username().is_empty()
        || parsed.password().is_some()
        || parsed.query().is_some()
        || parsed.fragment().is_some()
    {
        return Err("Use an HTTPS provider URL without credentials or query parameters. HTTP is supported only for local providers.".into());
    }
    let model = update.model.trim();
    if model.is_empty() || model.len() > 256 || model.chars().any(char::is_control) {
        return Err("Enter a model name (up to 256 characters).".into());
    }
    let api_key = update.api_key.trim();
    if api_key.len() > 65536 || api_key.chars().any(char::is_control) {
        return Err("The API key contains invalid characters or is too long.".into());
    }
    fs::create_dir_all(data_dir).map_err(|_| "Cannot open the workspace directory.")?;
    let mut value = context(data_dir)?;
    if !value["credentials"].is_null() && !value["credentials"].is_array() {
        return Err("The workspace credential configuration is invalid.".into());
    }
    let existing = authoring_credential(&value).cloned();
    if api_key.is_empty() {
        if let Some(previous) = value["authoring"]["baseURL"].as_str() {
            if Url::parse(previous).map(|url| url.origin()).ok() != Some(parsed.origin()) {
                return Err("Enter an API key for the new provider URL. The saved key belongs to the previous provider.".into());
            }
        }
    }
    if api_key.is_empty()
        && !existing
            .as_ref()
            .is_some_and(|c| credential_available(data_dir, c))
    {
        return Err("Enter an API key to connect this provider.".into());
    }
    let mut created_secret: Option<PathBuf> = None;
    let mut obsolete_secret: Option<PathBuf> = None;
    let reference = if !api_key.is_empty() {
        let secrets = data_dir.join(".secrets");
        if secrets.exists()
            && fs::symlink_metadata(&secrets)
                .map_err(|_| "Cannot inspect the credential directory.")?
                .file_type()
                .is_symlink()
        {
            return Err("The credential directory must not be a symbolic link.".into());
        }
        fs::create_dir_all(&secrets).map_err(|_| "Cannot create the credential directory.")?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&secrets, fs::Permissions::from_mode(0o700))
                .map_err(|_| "Cannot protect the credential directory.")?;
        }
        let relative = format!(".secrets/desktop-authoring-{}.key", uuid::Uuid::new_v4());
        let secret_path = data_dir.join(&relative);
        if let Err(e) = write_private(&secret_path, api_key.as_bytes()) {
            let _ = fs::remove_file(&secret_path);
            return Err(e);
        }
        created_secret = Some(secret_path);
        if value["credentials"].is_null() {
            value["credentials"] = json!([]);
        }
        let credentials = value["credentials"]
            .as_array_mut()
            .expect("validated credential list");
        let index = credentials.iter().position(|c| {
            c["scope"]["kind"] == "local"
                && c["scope"]["id"] == "default"
                && c["kind"] == "api-key"
                && c["id"] == CREDENTIAL_ID
        });
        if let Some(index) = index {
            if let Some(file) = credentials[index]["file"].as_str() {
                if file.starts_with(".secrets/desktop-authoring-")
                    && !file[9..].contains('/')
                    && !file.contains("..")
                    && credentials.iter().filter(|c| c["file"] == file).count() == 1
                {
                    obsolete_secret = Some(data_dir.join(file));
                }
            }
        }
        let credential = json!({"scope":{"kind":"local","id":"default"},"kind":"api-key","id":CREDENTIAL_ID,"displayName":"Desktop authoring model","bindingKeys":["MODEL_PROVIDER"],"file":relative});
        if let Some(index) = index {
            credentials[index] = credential;
        } else {
            credentials.push(credential);
        }
        json!({"kind":"api-key","id":CREDENTIAL_ID})
    } else {
        value["authoring"]["credential"].clone()
    };
    value["authoring"] = json!({"baseURL":base_url,"model":model,"credential":reference});
    let bytes = serde_yaml::to_string(&value).map_err(|_| "Cannot encode provider settings.")?;
    let temporary = data_dir.join(format!(".context-{}.tmp", uuid::Uuid::new_v4()));
    let result = write_private(&temporary, bytes.as_bytes()).and_then(|_| {
        fs::rename(&temporary, data_dir.join("context.yaml"))
            .map_err(|_| "Cannot replace workspace settings.".into())
    });
    if let Err(error) = result {
        let _ = fs::remove_file(temporary);
        if let Some(path) = created_secret {
            let _ = fs::remove_file(path);
        }
        return Err(error);
    }
    if let Some(path) = obsolete_secret {
        let _ = fs::remove_file(path);
    }
    load_provider(data_dir)
}

pub fn save_skill_credential(
    data_dir: &Path,
    update: SkillCredentialUpdate,
) -> Result<SkillCredentialSaved, String> {
    let kind = update.kind.trim();
    let binding_key = update.binding_key.trim();
    let display_name = update.display_name.trim();
    let secret = update.secret.trim();
    let safe_key = |text: &str| {
        !text.is_empty()
            && text.len() <= 128
            && text
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-' | b'.'))
    };
    if !safe_key(kind)
        || !safe_key(binding_key)
        || display_name.is_empty()
        || display_name.len() > 128
        || display_name.chars().any(char::is_control)
        || secret.is_empty()
        || secret.len() > 65536
        || secret.contains('\0')
    {
        return Err("Enter a valid credential kind, binding key, name, and secret.".into());
    }
    fs::create_dir_all(data_dir).map_err(|_| "Cannot open the workspace directory.")?;
    let mut value = context(data_dir)?;
    if !value["credentials"].is_null() && !value["credentials"].is_array() {
        return Err("The workspace credential configuration is invalid.".into());
    }
    let secrets = data_dir.join(".secrets");
    if secrets.exists()
        && fs::symlink_metadata(&secrets)
            .map_err(|_| "Cannot inspect the credential directory.")?
            .file_type()
            .is_symlink()
    {
        return Err("The credential directory must not be a symbolic link.".into());
    }
    fs::create_dir_all(&secrets).map_err(|_| "Cannot create the credential directory.")?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(&secrets, fs::Permissions::from_mode(0o700))
            .map_err(|_| "Cannot protect the credential directory.")?;
    }
    let id = format!("desktop-skill-{}", uuid::Uuid::new_v4());
    let relative = format!(".secrets/{id}.key");
    let secret_path = data_dir.join(&relative);
    if let Err(error) = write_private(&secret_path, secret.as_bytes()) {
        let _ = fs::remove_file(&secret_path);
        return Err(error);
    }
    if value["credentials"].is_null() {
        value["credentials"] = json!([]);
    }
    value["credentials"]
        .as_array_mut()
        .expect("validated credential list")
        .push(json!({
            "scope": {"kind": "local", "id": "default"}, "kind": kind, "id": id,
            "displayName": display_name, "bindingKeys": [binding_key], "file": relative
        }));
    let bytes =
        serde_yaml::to_string(&value).map_err(|_| "Cannot encode workspace credentials.")?;
    let temporary = data_dir.join(format!(".context-{}.tmp", uuid::Uuid::new_v4()));
    let result = write_private(&temporary, bytes.as_bytes()).and_then(|_| {
        fs::rename(&temporary, data_dir.join("context.yaml"))
            .map_err(|_| "Cannot replace workspace settings.".into())
    });
    if let Err(error) = result {
        let _ = fs::remove_file(&temporary);
        let _ = fs::remove_file(&secret_path);
        return Err(error);
    }
    Ok(SkillCredentialSaved {
        kind: kind.into(),
        id,
        display_name: display_name.into(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn saves_skill_credential_privately_and_keeps_provider() {
        let dir = tempfile::tempdir().unwrap();
        save_provider(dir.path(), update("model-secret")).unwrap();
        let saved = save_skill_credential(
            dir.path(),
            SkillCredentialUpdate {
                kind: "environment-secret".into(),
                binding_key: "RESEARCH_API_KEY".into(),
                display_name: "Research connection".into(),
                secret: "research-secret".into(),
            },
        )
        .unwrap();
        let value = context(dir.path()).unwrap();
        let credential = value["credentials"]
            .as_array()
            .unwrap()
            .iter()
            .find(|item| item["id"] == saved.id)
            .unwrap();
        assert_eq!(credential["bindingKeys"][0], "RESEARCH_API_KEY");
        assert_eq!(credential["kind"], "environment-secret");
        assert!(load_provider(dir.path()).unwrap().has_api_key);
        assert!(!fs::read_to_string(dir.path().join("context.yaml"))
            .unwrap()
            .contains("research-secret"));
        let secret_path = dir.path().join(credential["file"].as_str().unwrap());
        assert_eq!(fs::read_to_string(&secret_path).unwrap(), "research-secret");
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            assert_eq!(
                fs::metadata(secret_path).unwrap().permissions().mode() & 0o777,
                0o600
            );
        }
    }
    fn directory() -> PathBuf {
        let p = std::env::temp_dir().join(format!("openseal-provider-{}", uuid::Uuid::new_v4()));
        fs::create_dir_all(&p).unwrap();
        p
    }
    fn update(key: &str) -> ProviderUpdate {
        ProviderUpdate {
            base_url: "https://api.openai.com/v1/".into(),
            model: "test-model".into(),
            api_key: key.into(),
        }
    }
    #[test]
    fn saves_secret_privately_and_retains_it_without_returning_it() {
        let dir = directory();
        let saved = save_provider(&dir, update("test-secret-value")).unwrap();
        assert!(saved.has_api_key);
        let serialized = serde_json::to_string(&saved).unwrap();
        assert!(!serialized.contains("test-secret-value"));
        assert!(!fs::read_to_string(dir.join("context.yaml"))
            .unwrap()
            .contains("test-secret-value"));
        let value = context(&dir).unwrap();
        let file = dir.join(
            authoring_credential(&value).unwrap()["file"]
                .as_str()
                .unwrap(),
        );
        assert_eq!(fs::read_to_string(&file).unwrap(), "test-secret-value");
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            assert_eq!(
                fs::metadata(&file).unwrap().permissions().mode() & 0o777,
                0o600
            );
        }
        let mut next = update("");
        next.model = "another-model".into();
        assert_eq!(save_provider(&dir, next).unwrap().model, "another-model");
        assert_eq!(fs::read_to_string(&file).unwrap(), "test-secret-value");
        save_provider(&dir, update("rotated-secret")).unwrap();
        assert!(!file.exists());
        fs::remove_dir_all(dir).unwrap();
    }
    #[test]
    fn validates_before_writing_and_preserves_other_credentials() {
        let dir = directory();
        let mut invalid = update("secret");
        invalid.base_url = "http://untrusted.example/v1".into();
        assert!(save_provider(&dir, invalid).is_err());
        assert!(!dir.join("context.yaml").exists());
        fs::write(dir.join("context.yaml"),"apiVersion: openseal.dev/v1alpha1\nkind: StandaloneContext\ncredentials:\n  - scope: {kind: tenant, id: other}\n    kind: api-key\n    id: external\n    displayName: Existing\n    env: EXTERNAL_SECRET\n").unwrap();
        save_provider(&dir, update("test-secret")).unwrap();
        assert_eq!(context(&dir).unwrap()["credentials"][0]["id"], "external");
        fs::remove_dir_all(dir).unwrap();
    }
    #[test]
    fn changing_provider_origin_requires_a_new_key() {
        let dir = directory();
        save_provider(&dir, update("original-key")).unwrap();
        let before = fs::read(dir.join("context.yaml")).unwrap();
        let mut changed = update("");
        changed.base_url = "https://other-provider.example/v1".into();
        assert!(save_provider(&dir, changed)
            .err()
            .unwrap()
            .contains("previous provider"));
        assert_eq!(fs::read(dir.join("context.yaml")).unwrap(), before);
        fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn missing_key_does_not_create_a_partial_configuration() {
        let dir = directory();
        assert!(save_provider(&dir, update("")).is_err());
        assert!(!dir.join("context.yaml").exists());
        fs::remove_dir_all(dir).unwrap();
    }
}
