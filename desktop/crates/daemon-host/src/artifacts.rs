//! Version-pinned, authenticated artifact reads and verified atomic exports.
use crate::{api_url, DaemonHost};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::{
    io::{Read, Write},
    path::Path,
};

pub const MAX_EXPORT_BYTES: u64 = 100 * 1024 * 1024;
pub const MAX_PREVIEW_BYTES: u64 = 256 * 1024;
#[derive(Deserialize, Clone)]
#[serde(rename_all = "camelCase")]
pub struct ArtifactMetadata {
    pub id: String,
    pub version: u64,
    pub name: String,
    #[serde(default)]
    pub media_type: String,
    pub size_bytes: u64,
    pub digest: String,
}
fn artifact_path(id: &str, version: u64, content: bool) -> Result<String, String> {
    if id.is_empty()
        || id.len() > 128
        || id.chars().any(|c| "\r\n?#@/\\".contains(c))
        || id == "."
        || id == ".."
        || version == 0
    {
        return Err("Invalid artifact identity or version".into());
    }
    let encoded: String = id
        .bytes()
        .map(|b| {
            if b.is_ascii_alphanumeric() || b"-_.~".contains(&b) {
                (b as char).to_string()
            } else {
                format!("%{b:02X}")
            }
        })
        .collect();
    Ok(format!(
        "/api/v1/artifacts/{encoded}{}?scopeKind=local&scopeId=default&version={version}",
        if content { "/content" } else { "" }
    ))
}
pub fn suggested_name(name: &str) -> String {
    let name = name.rsplit(['/', '\\']).next().unwrap_or("").trim();
    let clean: String = name.chars().filter(|c| !c.is_control()).take(180).collect();
    if clean.is_empty() || clean == "." || clean == ".." {
        "artifact".into()
    } else {
        clean
    }
}
pub fn text_media_type(value: &str) -> bool {
    let mime = value
        .split(';')
        .next()
        .unwrap_or("")
        .trim()
        .to_ascii_lowercase();
    mime.starts_with("text/")
        || matches!(
            mime.as_str(),
            "application/json" | "application/xml" | "application/yaml" | "application/x-yaml"
        )
        || mime.ends_with("+json")
        || mime.ends_with("+xml")
}
fn verify(metadata: &ArtifactMetadata, bytes: &[u8], limit: u64) -> Result<(), String> {
    if metadata.size_bytes > limit || bytes.len() as u64 > limit {
        return Err("Artifact exceeds the size limit for this operation".into());
    }
    if bytes.len() as u64 != metadata.size_bytes {
        return Err("Artifact size does not match its saved metadata".into());
    }
    if format!("sha256:{:x}", Sha256::digest(bytes)) != metadata.digest.to_ascii_lowercase() {
        return Err("Artifact checksum does not match. No file was saved".into());
    }
    Ok(())
}
pub fn save_verified(
    metadata: &ArtifactMetadata,
    bytes: &[u8],
    destination: &Path,
) -> Result<(), String> {
    verify(metadata, bytes, MAX_EXPORT_BYTES)?;
    let parent = destination.parent().ok_or("Choose a destination folder")?;
    let mut temporary = tempfile::NamedTempFile::new_in(parent)
        .map_err(|_| "Cannot create a file in the chosen folder")?;
    temporary
        .write_all(bytes)
        .map_err(|_| "Could not write the artifact")?;
    temporary
        .as_file()
        .sync_all()
        .map_err(|_| "Could not finish writing the artifact")?;
    temporary
        .persist(destination)
        .map_err(|_| "Could not save the artifact at the chosen destination")?;
    Ok(())
}
impl DaemonHost {
    pub fn artifact_metadata(&self, id: &str, version: u64) -> Result<ArtifactMetadata, String> {
        let response = self.request("GET", &artifact_path(id, version, false)?, None, None)?;
        if response.status != 200 {
            return Err("Artifact metadata is unavailable. Refresh the task and try again".into());
        }
        let metadata: ArtifactMetadata =
            serde_json::from_value(response.body).map_err(|_| "Artifact metadata is unreadable")?;
        if metadata.id != id || metadata.version != version {
            return Err("Artifact version did not match the request".into());
        }
        Ok(metadata)
    }
    pub fn artifact_bytes(
        &self,
        metadata: &ArtifactMetadata,
        limit: u64,
    ) -> Result<Vec<u8>, String> {
        if metadata.size_bytes > limit {
            return Err("Artifact exceeds the size limit for this operation".into());
        }
        let url = api_url(
            &self.endpoint,
            &artifact_path(&metadata.id, metadata.version, true)?,
        )?;
        let response = self
            .client
            .get(url)
            .bearer_auth(&self.token)
            .send()
            .map_err(|_| "Could not download artifact content")?;
        if response.status().as_u16() != 200 {
            return Err("Artifact content is unavailable. Refresh the task and try again".into());
        }
        let mut bytes = Vec::new();
        response
            .take(limit + 1)
            .read_to_end(&mut bytes)
            .map_err(|_| "Artifact download was interrupted")?;
        verify(metadata, &bytes, limit)?;
        Ok(bytes)
    }
    pub fn preview_artifact(&self, id: &str, version: u64) -> Result<String, String> {
        let metadata = self.artifact_metadata(id, version)?;
        if !text_media_type(&metadata.media_type) {
            return Err("This file type does not have a text preview".into());
        }
        String::from_utf8(self.artifact_bytes(&metadata, MAX_PREVIEW_BYTES)?).map_err(|_| {
            "This artifact is not UTF-8 text. Save it to view it in another app".into()
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn metadata(bytes: &[u8]) -> ArtifactMetadata {
        ArtifactMetadata {
            id: "report".into(),
            version: 2,
            name: "report.txt".into(),
            media_type: "text/plain".into(),
            size_bytes: bytes.len() as u64,
            digest: format!("sha256:{:x}", Sha256::digest(bytes)),
        }
    }
    #[test]
    fn export_is_verified_before_replacing_a_file() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("report.txt");
        std::fs::write(&path, b"existing content").unwrap();
        let record = metadata(b"new content");
        assert!(save_verified(&record, b"bad content", &path).is_err());
        assert_eq!(std::fs::read(&path).unwrap(), b"existing content");
        save_verified(&record, b"new content", &path).unwrap();
        assert_eq!(std::fs::read(&path).unwrap(), b"new content");
        assert_eq!(std::fs::read_dir(directory.path()).unwrap().count(), 1);
    }
    #[test]
    fn bounds_identity_and_preview_types_are_explicit() {
        for id in [
            "../token",
            "https://example.com",
            "..",
            "x?scopeId=other",
            "a/b",
        ] {
            assert!(artifact_path(id, 1, true).is_err());
        }
        assert!(artifact_path("report", 0, true).is_err());
        assert!(artifact_path("report:résumé", 2, true)
            .unwrap()
            .contains("report%3Ar%C3%A9sum%C3%A9/content"));
        assert_eq!(suggested_name("../folder\\report.txt"), "report.txt");
        assert_eq!(suggested_name(".."), "artifact");
        assert!(text_media_type("text/html; charset=utf-8"));
        assert!(!text_media_type("image/png"));
        assert!(verify(&metadata(b"abc"), b"abc", 2).is_err());
        assert!(verify(&metadata(b"abc"), b"ab", 10).is_err());
    }
}
