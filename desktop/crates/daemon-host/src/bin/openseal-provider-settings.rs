//! Development bridge. Secrets arrive on stdin, never command arguments.
use openseal_daemon_host::provider::{
    load_provider, save_provider, save_skill_credential, ProviderUpdate, SkillCredentialUpdate,
};
use std::io::Read;
fn main() {
    let result = (|| {
        let directory = std::env::args_os()
            .nth(1)
            .ok_or("Workspace directory required")?;
        let mut input = String::new();
        std::io::stdin()
            .take(131072)
            .read_to_string(&mut input)
            .map_err(|_| "Cannot read settings request")?;
        let value: serde_json::Value =
            serde_json::from_str(&input).map_err(|_| "Invalid settings request")?;
        let path = std::path::Path::new(&directory);
        if value["action"] == "load" {
            load_provider(path).and_then(|settings| {
                serde_json::to_value(settings).map_err(|_| "Cannot encode settings".into())
            })
        } else if value["action"] == "save" {
            let update: ProviderUpdate = serde_json::from_value(value["settings"].clone())
                .map_err(|_| "Invalid provider settings")?;
            save_provider(path, update).and_then(|settings| {
                serde_json::to_value(settings).map_err(|_| "Cannot encode settings".into())
            })
        } else if value["action"] == "save_skill_credential" {
            let update: SkillCredentialUpdate = serde_json::from_value(value["credential"].clone())
                .map_err(|_| "Invalid Skill credential")?;
            save_skill_credential(path, update).and_then(|settings| {
                serde_json::to_value(settings).map_err(|_| "Cannot encode Skill connection".into())
            })
        } else {
            Err("Unsupported settings action".into())
        }
    })();
    match result {
        Ok(settings) => println!("{}", serde_json::json!({"settings":settings})),
        Err(error) => {
            println!("{}", serde_json::json!({"error":error}));
            std::process::exit(1);
        }
    }
}
