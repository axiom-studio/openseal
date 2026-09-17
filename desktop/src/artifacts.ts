import { isTauri, invoke } from "@tauri-apps/api/core";
import { api, scoped } from "./api";
export type Artifact = {
  id: string;
  version: number;
  name: string;
  mediaType?: string;
  sizeBytes: number;
  digest: string;
  classification: string;
  contentAvailability?: string;
  provenance: { runId?: string; producer: { type: string; id: string } };
  createdAt: string;
};
export const previewLimit = 256 * 1024;
export const exportLimit = 100 * 1024 * 1024;
export function textArtifact(media = "") {
  const mime = media.split(";")[0].trim().toLowerCase();
  return (
    mime.startsWith("text/") ||
    [
      "application/json",
      "application/xml",
      "application/yaml",
      "application/x-yaml",
    ].includes(mime) ||
    mime.endsWith("+json") ||
    mime.endsWith("+xml")
  );
}
export function artifactName(name: string) {
  const clean = name
    .split(/[\\/]/)
    .at(-1)
    ?.replace(/[\u0000-\u001f\u007f]/g, "")
    .trim()
    .slice(0, 180);
  return !clean || clean === "." || clean === ".." ? "artifact" : clean;
}
async function content(record: Artifact, limit: number) {
  const artifact = await api<Artifact>(
    scoped(
      `/artifacts/${encodeURIComponent(record.id)}?version=${record.version}`,
    ),
  );
  if (artifact.id !== record.id || artifact.version !== record.version)
    throw new Error("Artifact version did not match the request.");
  if (
    !Number.isSafeInteger(artifact.sizeBytes) ||
    artifact.sizeBytes < 0 ||
    artifact.sizeBytes > limit
  )
    throw new Error("This artifact exceeds the size limit for this operation.");
  const response = await fetch(
    `/api/v1${scoped(`/artifacts/${encodeURIComponent(record.id)}/content?version=${record.version}`)}`,
    { signal: AbortSignal.timeout(35000) },
  );
  if (!response.ok || !response.body)
    throw new Error("Artifact content is unavailable. Refresh and try again.");
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let length = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      length += value.byteLength;
      if (length > limit)
        throw new Error(
          "This artifact exceeds the size limit for this operation.",
        );
      chunks.push(value);
    }
  } finally {
    await reader.cancel();
  }
  const bytes = new Uint8Array(length);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  if (bytes.byteLength !== artifact.sizeBytes)
    throw new Error("Artifact size does not match its saved metadata.");
  const digest = Array.from(
    new Uint8Array(await crypto.subtle.digest("SHA-256", bytes)),
    (byte) => byte.toString(16).padStart(2, "0"),
  ).join("");
  if (`sha256:${digest}` !== artifact.digest.toLowerCase())
    throw new Error("Artifact checksum does not match. No file was saved.");
  return { artifact, bytes };
}
export async function previewArtifact(artifact: Artifact): Promise<string> {
  if (isTauri())
    return invoke("preview_artifact", {
      id: artifact.id,
      version: artifact.version,
    });
  const result = await content(artifact, previewLimit);
  if (!textArtifact(result.artifact.mediaType))
    throw new Error("This file type does not have a text preview.");
  try {
    return new TextDecoder("utf-8", { fatal: true }).decode(result.bytes);
  } catch {
    throw new Error(
      "This artifact is not UTF-8 text. Save it to view it in another app.",
    );
  }
}
export async function saveArtifact(artifact: Artifact): Promise<string> {
  if (isTauri()) {
    const saved = await invoke<boolean>("save_artifact", {
      id: artifact.id,
      version: artifact.version,
    });
    return saved ? "Artifact saved." : "Save canceled.";
  }
  const result = await content(artifact, exportLimit);
  const url = URL.createObjectURL(
    new Blob([result.bytes], {
      type: result.artifact.mediaType || "application/octet-stream",
    }),
  );
  const link = document.createElement("a");
  link.href = url;
  link.download = artifactName(result.artifact.name);
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 30000);
  return "Download started.";
}
