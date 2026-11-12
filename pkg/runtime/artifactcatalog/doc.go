// Package artifactcatalog registers, places, retains and serves the artifacts
// that runs produce. It records immutable artifact versions in a
// models.ArtifactStore, fills in the chat and space each artifact came from,
// sweeps content whose retention has expired, and defines the interfaces a
// host implements to inspect, delete and resolve artifact bytes. It is named
// so that it does not clash with pkg/artifact.
package artifactcatalog
