package clawhub

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	opensealclaw "github.com/axiom-studio/openseal/pkg/skill/openclaw"
)

var (
	ErrSkillPinned        = errors.New("installed skill is pinned")
	ErrSkillModified      = errors.New("installed skill has local modifications")
	ErrVerificationFailed = errors.New("skill verification failed")
	ErrAmbiguousSkill     = errors.New("installed skill reference is ambiguous")
)

type LockEntry struct {
	Version     *string `json:"version"`
	InstalledAt int64   `json:"installedAt"`
	Registry    string  `json:"registry,omitempty"`
	OwnerHandle string  `json:"ownerHandle,omitempty"`
	Slug        string  `json:"slug,omitempty"`
	Directory   string  `json:"directory,omitempty"`
	Pinned      bool    `json:"pinned,omitempty"`
	PinReason   string  `json:"pinReason,omitempty"`
}

type Lockfile struct {
	Version int                  `json:"version"`
	Skills  map[string]LockEntry `json:"skills"`
}

type SkillOrigin struct {
	Version          int    `json:"version"`
	Registry         string `json:"registry"`
	Slug             string `json:"slug"`
	OwnerHandle      string `json:"ownerHandle,omitempty"`
	InstalledVersion string `json:"installedVersion"`
	InstalledAt      int64  `json:"installedAt"`
	Fingerprint      string `json:"fingerprint,omitempty"`
	ArchiveSHA256    string `json:"archiveSha256,omitempty"`
}

type InstallRequest struct {
	Reference        SkillReference
	Version          string
	Tag              string
	Force            bool
	SkipVerification bool
}

type InstalledSkill struct {
	SourceIdentity string
	Reference      SkillReference
	Version        string
	Directory      string
	Origin         SkillOrigin
	Verification   *Verification
	Compilation    *opensealclaw.Compilation
	Changed        bool
}

type UpdateReport struct {
	Updated       []string          `json:"updated,omitempty"`
	Unchanged     []string          `json:"unchanged,omitempty"`
	SkippedPinned []string          `json:"skippedPinned,omitempty"`
	Errors        map[string]string `json:"errors,omitempty"`
}

type InstallManager struct {
	registryID string
	registry   Registry
	workspace  string
	skillsDir  string
	now        func() time.Time
	mu         sync.Mutex
}

type CompilationValidator func(*opensealclaw.Compilation) error

func NewInstallManager(registryID string, registry Registry, workspace string) (*InstallManager, error) {
	return NewInstallManagerWithSkillsDirectory(registryID, registry, workspace, filepath.Join(workspace, "skills"))
}

// NewInstallManagerWithSkillsDirectory creates an installer whose lock and
// provenance metadata live in workspace while activated skills live in the
// caller-selected directory. This lets embedders adopt the verified installer
// without relocating an existing skills mount.
func NewInstallManagerWithSkillsDirectory(registryID string, registry Registry, workspace, skillsDirectory string) (*InstallManager, error) {
	if registry == nil || strings.TrimSpace(registryID) == "" || strings.TrimSpace(workspace) == "" || strings.TrimSpace(skillsDirectory) == "" {
		return nil, errors.New("registry id, registry, workspace, and skills directory are required")
	}
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	absSkills, err := filepath.Abs(skillsDirectory)
	if err != nil {
		return nil, err
	}
	return &InstallManager{registryID: strings.TrimRight(registryID, "/"), registry: registry, workspace: absWorkspace, skillsDir: absSkills, now: time.Now}, nil
}

func (m *InstallManager) Install(ctx context.Context, req InstallRequest) (*InstalledSkill, error) {
	return m.install(ctx, req, nil)
}

func (m *InstallManager) InstallValidated(ctx context.Context, req InstallRequest, validate CompilationValidator) (*InstalledSkill, error) {
	return m.install(ctx, req, validate)
}

func (m *InstallManager) install(ctx context.Context, req InstallRequest, validate CompilationValidator) (*InstalledSkill, error) {
	ref, err := canonicalReference(req.Reference)
	if err != nil || req.Version != "" && req.Tag != "" {
		return nil, errors.New("skill reference is required and version and tag are mutually exclusive")
	}
	req.Reference = ref
	if req.SkipVerification && req.Tag != "" {
		return nil, errors.New("tag resolution requires verification")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.MkdirAll(m.skillsDir, 0o755); err != nil {
		return nil, err
	}
	lock, err := m.readLockfile()
	if err != nil {
		return nil, err
	}
	identity := m.identity(req.Reference)
	existing, installed := lock.Skills[identity]
	if installed && existing.Pinned {
		return nil, fmt.Errorf("%w: %s", ErrSkillPinned, req.Reference.String())
	}
	detail, err := m.registry.InspectSkill(ctx, req.Reference)
	if err != nil {
		return nil, err
	}
	resolvedVersion := strings.TrimSpace(req.Version)
	var verification *Verification
	if !req.SkipVerification {
		verification, err = m.registry.VerifySkill(ctx, req.Reference, req.Version, req.Tag)
		if err != nil {
			return nil, err
		}
		if !verification.OK || verification.Decision != "pass" {
			return nil, fmt.Errorf("%w: %s", ErrVerificationFailed, strings.Join(verification.Reasons, "; "))
		}
		if verification.Slug != req.Reference.Slug || req.Reference.Owner != "" && verification.PublisherHandle != req.Reference.Owner || verification.Version == "" {
			return nil, fmt.Errorf("%w: verification identity does not match requested skill", ErrVerificationFailed)
		}
		resolvedVersion = verification.Version
	}
	if resolvedVersion == "" {
		resolvedVersion = detail.Version
	}
	if resolvedVersion == "" {
		return nil, errors.New("registry did not resolve a skill version")
	}
	archive, err := m.registry.DownloadArchive(ctx, req.Reference, resolvedVersion, "")
	if err != nil {
		return nil, err
	}
	stage, bundle, fingerprint, err := m.stageArchive(req.Reference, resolvedVersion, archive, verification)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	if validate != nil {
		if err := validate(bundle); err != nil {
			return nil, fmt.Errorf("validate compiled skill: %w", err)
		}
	}
	targetEntry := LockEntry{Directory: installDirectory(identity, req.Reference.Slug)}
	if installed {
		targetEntry = existing
	}
	target := m.entryPath(identity, targetEntry)
	if installed {
		modified, err := m.isLocallyModified(target)
		if err != nil {
			return nil, err
		}
		if modified && !req.Force {
			return nil, fmt.Errorf("%w: %s", ErrSkillModified, req.Reference.String())
		}
		if existing.Version != nil && *existing.Version == resolvedVersion && !modified {
			origin, err := readOrigin(target)
			if err != nil {
				return nil, err
			}
			return &InstalledSkill{SourceIdentity: identity, Reference: req.Reference, Version: resolvedVersion, Directory: target, Origin: *origin, Verification: verification, Compilation: bundle, Changed: false}, nil
		}
	}
	now := m.now().UTC()
	origin := SkillOrigin{Version: 1, Registry: m.registryID, Slug: req.Reference.Slug, OwnerHandle: req.Reference.Owner, InstalledVersion: resolvedVersion, InstalledAt: now.UnixMilli(), Fingerprint: fingerprint, ArchiveSHA256: archive.SHA256}
	if err := writeAtomicJSON(filepath.Join(stage, ".clawhub", "origin.json"), origin); err != nil {
		return nil, err
	}
	if verification != nil {
		if err := writeAtomicJSON(filepath.Join(stage, ".clawhub", "verification.json"), verification); err != nil {
			return nil, err
		}
	}
	backup := ""
	if _, err := os.Stat(target); err == nil {
		backup = target + ".backup-" + fmt.Sprint(now.UnixNano())
		if err := os.Rename(target, backup); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.Rename(stage, target); err != nil {
		if backup != "" {
			_ = os.Rename(backup, target)
		}
		return nil, err
	}
	versionCopy := resolvedVersion
	lock.Skills[identity] = LockEntry{Version: &versionCopy, InstalledAt: now.UnixMilli(), Registry: m.registryID, OwnerHandle: req.Reference.Owner, Slug: req.Reference.Slug, Directory: filepath.Base(target)}
	if err := m.writeLockfile(lock); err != nil {
		_ = os.RemoveAll(target)
		if backup != "" {
			_ = os.Rename(backup, target)
		}
		return nil, err
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	return &InstalledSkill{SourceIdentity: identity, Reference: req.Reference, Version: resolvedVersion, Directory: target, Origin: origin, Verification: verification, Compilation: bundle, Changed: true}, nil
}

func (m *InstallManager) Update(ctx context.Context, slug string) (*InstalledSkill, error) {
	return m.update(ctx, slug, nil)
}

func (m *InstallManager) UpdateValidated(ctx context.Context, slug string, validate CompilationValidator) (*InstalledSkill, error) {
	return m.update(ctx, slug, validate)
}

func (m *InstallManager) update(ctx context.Context, reference string, validate CompilationValidator) (*InstalledSkill, error) {
	lock, err := m.List()
	if err != nil {
		return nil, err
	}
	_, entry, err := m.resolve(lock, reference)
	if err != nil {
		return nil, err
	}
	return m.install(ctx, InstallRequest{Reference: SkillReference{Owner: entry.OwnerHandle, Slug: entry.Slug}}, validate)
}

func (m *InstallManager) UpdateAll(ctx context.Context) UpdateReport {
	lock, err := m.List()
	if err != nil {
		return UpdateReport{Errors: map[string]string{"lockfile": err.Error()}}
	}
	slugs := make([]string, 0, len(lock.Skills))
	for identity := range lock.Skills {
		slugs = append(slugs, identity)
	}
	sort.Strings(slugs)
	report := UpdateReport{Errors: make(map[string]string)}
	for _, slug := range slugs {
		entry := lock.Skills[slug]
		if entry.Pinned {
			report.SkippedPinned = append(report.SkippedPinned, slug)
			continue
		}
		installed, err := m.install(ctx, InstallRequest{Reference: SkillReference{Owner: entry.OwnerHandle, Slug: entry.Slug}}, nil)
		if err != nil {
			report.Errors[slug] = err.Error()
			continue
		}
		if installed.Changed {
			report.Updated = append(report.Updated, slug)
		} else {
			report.Unchanged = append(report.Unchanged, slug)
		}
	}
	if len(report.Errors) == 0 {
		report.Errors = nil
	}
	return report
}

func (m *InstallManager) VerifyInstalled(ctx context.Context, reference string) (*Verification, error) {
	lock, err := m.List()
	if err != nil {
		return nil, err
	}
	identity, entry, err := m.resolve(lock, reference)
	if err != nil || entry.Version == nil {
		if err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	modified, err := m.isLocallyModified(m.entryPath(identity, entry))
	if err != nil {
		return nil, err
	}
	if modified {
		return nil, ErrSkillModified
	}
	ref := SkillReference{Owner: entry.OwnerHandle, Slug: entry.Slug}
	verification, err := m.registry.VerifySkill(ctx, ref, *entry.Version, "")
	if err != nil {
		return nil, err
	}
	if !verification.OK || verification.Decision != "pass" || !strings.EqualFold(verification.Slug, entry.Slug) || verification.Version != *entry.Version || entry.OwnerHandle != "" && !strings.EqualFold(verification.PublisherHandle, entry.OwnerHandle) {
		return nil, ErrVerificationFailed
	}
	return verification, nil
}

func (m *InstallManager) LoadInstalled() ([]*InstalledSkill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, err := m.readLockfile()
	if err != nil {
		return nil, err
	}
	slugs := make([]string, 0, len(lock.Skills))
	for identity := range lock.Skills {
		slugs = append(slugs, identity)
	}
	sort.Strings(slugs)
	result := make([]*InstalledSkill, 0, len(slugs))
	for _, slug := range slugs {
		entry := lock.Skills[slug]
		installed, err := m.loadInstalled(slug, entry)
		if err != nil {
			return nil, fmt.Errorf("load installed skill %s: %w", slug, err)
		}
		result = append(result, installed)
	}
	return result, nil
}

func (m *InstallManager) ListInstalledStates() ([]InstalledState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, err := m.readLockfile()
	if err != nil {
		return nil, err
	}
	identities := make([]string, 0, len(lock.Skills))
	for identity := range lock.Skills {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	states := make([]InstalledState, 0, len(identities))
	for _, identity := range identities {
		entry := lock.Skills[identity]
		installed, err := m.loadInstalled(identity, entry)
		if err != nil {
			return nil, fmt.Errorf("inspect installed skill %s: %w", identity, err)
		}
		modified, err := m.isLocallyModified(m.entryPath(identity, entry))
		if err != nil {
			return nil, err
		}
		state := InstalledState{
			APIVersion: LifecycleAPIVersion, SourceIdentity: identity, Reference: installed.Reference,
			Registry: installed.Origin.Registry, Version: installed.Version, InstalledAt: installed.Origin.InstalledAt,
			Fingerprint: installed.Origin.Fingerprint, ArchiveSHA256: installed.Origin.ArchiveSHA256,
			Pinned: entry.Pinned, PinReason: entry.PinReason, LocallyModified: modified,
		}
		state.Verified = installed.Verification != nil && installed.Verification.OK && installed.Verification.Decision == "pass"
		states = append(states, state)
	}
	return states, nil
}

func (m *InstallManager) Pin(reference, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 240 || strings.ContainsAny(reason, "\r\n") {
		return errors.New("pin reason must be 1-240 characters without line breaks")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, err := m.readLockfile()
	if err != nil {
		return err
	}
	identity, entry, err := m.resolve(*lock, reference)
	if err != nil {
		return err
	}
	entry.Pinned = true
	entry.PinReason = reason
	lock.Skills[identity] = entry
	return m.writeLockfile(lock)
}

func (m *InstallManager) Unpin(reference string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, err := m.readLockfile()
	if err != nil {
		return err
	}
	identity, entry, err := m.resolve(*lock, reference)
	if err != nil {
		return err
	}
	entry.Pinned = false
	entry.PinReason = ""
	lock.Skills[identity] = entry
	return m.writeLockfile(lock)
}

func (m *InstallManager) Uninstall(reference string, force bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, err := m.readLockfile()
	if err != nil {
		return err
	}
	identity, entry, err := m.resolve(*lock, reference)
	if err != nil {
		return err
	}
	if entry.Pinned {
		return ErrSkillPinned
	}
	target := m.entryPath(identity, entry)
	modified, err := m.isLocallyModified(target)
	if err != nil {
		return err
	}
	if modified && !force {
		return ErrSkillModified
	}
	trash := target + ".remove-" + fmt.Sprint(m.now().UnixNano())
	if err := os.Rename(target, trash); err != nil && !os.IsNotExist(err) {
		return err
	}
	delete(lock.Skills, identity)
	if err := m.writeLockfile(lock); err != nil {
		_ = os.Rename(trash, target)
		return err
	}
	return os.RemoveAll(trash)
}

func (m *InstallManager) List() (Lockfile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, err := m.readLockfile()
	if err != nil {
		return Lockfile{}, err
	}
	return *lock, nil
}

func (m *InstallManager) stageArchive(ref SkillReference, version string, archive *DownloadedArchive, verification *Verification) (string, *opensealclaw.Compilation, string, error) {
	if archive == nil || len(archive.Bytes) == 0 {
		return "", nil, "", errors.New("downloaded archive is empty")
	}
	stage, err := os.MkdirTemp(m.skillsDir, ".install-"+ref.Slug+"-")
	if err != nil {
		return "", nil, "", err
	}
	files, err := extractArchive(archive.Bytes, stage)
	if err != nil {
		os.RemoveAll(stage)
		return "", nil, "", err
	}
	skillMD, ok := files["SKILL.md"]
	if !ok {
		if lowercase, exists := files["skill.md"]; exists {
			skillMD = lowercase
			if err := os.Rename(filepath.Join(stage, "skill.md"), filepath.Join(stage, "SKILL.md")); err != nil {
				os.RemoveAll(stage)
				return "", nil, "", err
			}
			delete(files, "skill.md")
			files["SKILL.md"] = skillMD
			ok = true
		}
	}
	if !ok {
		os.RemoveAll(stage)
		return "", nil, "", errors.New("ClawHub archive must contain SKILL.md at its root")
	}
	bundleFiles := make([]opensealclaw.File, 0, len(files)-1)
	for path, content := range files {
		if path != "SKILL.md" {
			bundleFiles = append(bundleFiles, opensealclaw.File{Path: path, Content: content})
		}
	}
	trust := map[string]interface{}(nil)
	if verification != nil {
		encoded, _ := json.Marshal(verification)
		_ = json.Unmarshal(encoded, &trust)
	}
	compilation, err := opensealclaw.Compile(opensealclaw.Bundle{SkillMD: skillMD, Files: bundleFiles, Source: opensealclaw.Source{Registry: m.registryID, Publisher: ref.Owner, Reference: ref.String(), Version: version, Trust: trust}})
	if err != nil {
		os.RemoveAll(stage)
		return "", nil, "", fmt.Errorf("compile installed skill: %w", err)
	}
	fingerprint := fingerprintFiles(files)
	return stage, compilation, fingerprint, nil
}

func (m *InstallManager) loadInstalled(identity string, entry LockEntry) (*InstalledSkill, error) {
	target := m.entryPath(identity, entry)
	origin, err := readOrigin(target)
	if err != nil {
		return nil, err
	}
	if origin == nil || !strings.EqualFold(origin.Registry, entry.Registry) || !strings.EqualFold(origin.Slug, entry.Slug) || entry.Version == nil || origin.InstalledVersion != *entry.Version || !strings.EqualFold(origin.OwnerHandle, entry.OwnerHandle) {
		return nil, errors.New("lockfile and installed origin do not match")
	}
	files, err := readInstalledFiles(target)
	if err != nil {
		return nil, err
	}
	if fingerprintFiles(files) != origin.Fingerprint {
		return nil, ErrSkillModified
	}
	verification, err := readStoredVerification(target)
	if err != nil {
		return nil, err
	}
	skillMD, ok := files["SKILL.md"]
	if !ok {
		return nil, errors.New("installed skill is missing SKILL.md")
	}
	bundleFiles := make([]opensealclaw.File, 0, len(files)-1)
	for path, content := range files {
		if path != "SKILL.md" {
			bundleFiles = append(bundleFiles, opensealclaw.File{Path: path, Content: content})
		}
	}
	var trust map[string]interface{}
	if verification != nil {
		encoded, _ := json.Marshal(verification)
		_ = json.Unmarshal(encoded, &trust)
	}
	ref := SkillReference{Owner: entry.OwnerHandle, Slug: entry.Slug}
	compilation, err := opensealclaw.Compile(opensealclaw.Bundle{SkillMD: skillMD, Files: bundleFiles, Source: opensealclaw.Source{Registry: origin.Registry, Publisher: ref.Owner, Reference: ref.String(), Version: origin.InstalledVersion, Trust: trust}})
	if err != nil {
		return nil, err
	}
	return &InstalledSkill{SourceIdentity: identity, Reference: ref, Version: origin.InstalledVersion, Directory: target, Origin: *origin, Verification: verification, Compilation: compilation, Changed: false}, nil
}

func extractArchive(raw []byte, target string) (map[string][]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("open skill archive: %w", err)
	}
	if len(reader.File) > 2048 {
		return nil, errors.New("skill archive contains too many files")
	}
	result := make(map[string][]byte)
	canonicalPaths := make(map[string]string)
	var total uint64
	for _, file := range reader.File {
		path := filepath.ToSlash(filepath.Clean(file.Name))
		if path == "." || strings.HasPrefix(path, "../") || strings.HasPrefix(path, "/") || strings.Contains(file.Name, "\\") {
			return nil, fmt.Errorf("unsafe skill archive path %q", file.Name)
		}
		if file.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("skill archive symlink %q is not allowed", file.Name)
		}
		if file.FileInfo().IsDir() {
			continue
		}
		canonical := strings.ToLower(path)
		if previous := canonicalPaths[canonical]; previous != "" {
			return nil, fmt.Errorf("skill archive contains colliding paths %q and %q", previous, path)
		}
		canonicalPaths[canonical] = path
		if file.UncompressedSize64 > 32*1024*1024 {
			return nil, fmt.Errorf("skill archive file %q exceeds 32 MiB", path)
		}
		total += file.UncompressedSize64
		if total > 256*1024*1024 {
			return nil, errors.New("skill archive exceeds 256 MiB uncompressed")
		}
		stream, err := file.Open()
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(io.LimitReader(stream, 32*1024*1024+1))
		stream.Close()
		if err != nil || len(content) > 32*1024*1024 {
			return nil, fmt.Errorf("read skill archive file %q: %w", path, err)
		}
		destination := filepath.Join(target, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return nil, err
		}
		mode := os.FileMode(0o644)
		if file.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		if err := os.WriteFile(destination, content, mode); err != nil {
			return nil, err
		}
		result[path] = content
	}
	return result, nil
}

func (m *InstallManager) isLocallyModified(target string) (bool, error) {
	origin, err := readOrigin(target)
	if err != nil {
		return false, err
	}
	if origin == nil || origin.Fingerprint == "" {
		return true, nil
	}
	files, err := readInstalledFiles(target)
	if err != nil {
		return false, err
	}
	return fingerprintFiles(files) != origin.Fingerprint, nil
}

func (m *InstallManager) readLockfile() (*Lockfile, error) {
	path := filepath.Join(m.workspace, ".clawhub", "lock.json")
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Lockfile{Version: 2, Skills: make(map[string]LockEntry)}, nil
	}
	if err != nil {
		return nil, err
	}
	var lock Lockfile
	if err := json.Unmarshal(content, &lock); err != nil {
		return nil, fmt.Errorf("parse ClawHub lockfile: %w", err)
	}
	if lock.Skills == nil || lock.Version != 1 && lock.Version != 2 {
		return nil, errors.New("unsupported ClawHub lockfile")
	}
	if lock.Version == 1 {
		if err := m.migrateLockfile(&lock); err != nil {
			return nil, err
		}
	}
	return &lock, nil
}

func (m *InstallManager) writeLockfile(lock *Lockfile) error {
	return writeAtomicJSON(filepath.Join(m.workspace, ".clawhub", "lock.json"), lock)
}

func readOrigin(target string) (*SkillOrigin, error) {
	content, err := os.ReadFile(filepath.Join(target, ".clawhub", "origin.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var origin SkillOrigin
	if err := json.Unmarshal(content, &origin); err != nil {
		return nil, err
	}
	if origin.Version != 1 && origin.Version != 2 {
		return nil, errors.New("unsupported skill origin metadata")
	}
	return &origin, nil
}

func canonicalReference(ref SkillReference) (SkillReference, error) {
	parsed, err := ParseSkillReference(ref.String())
	if err != nil {
		return SkillReference{}, err
	}
	parsed.Owner = strings.ToLower(parsed.Owner)
	parsed.Slug = strings.ToLower(parsed.Slug)
	return parsed, nil
}

func (m *InstallManager) identity(ref SkillReference) string {
	canonical, _ := canonicalReference(ref)
	return strings.ToLower(m.registryID) + "::" + canonical.String()
}

func installDirectory(identity, slug string) string {
	digest := sha256.Sum256([]byte(identity))
	return slug + "-" + hex.EncodeToString(digest[:8])
}

func (m *InstallManager) entryPath(identity string, entry LockEntry) string {
	directory := entry.Directory
	if directory == "" {
		directory = installDirectory(identity, entry.Slug)
	}
	return filepath.Join(m.skillsDir, filepath.Base(directory))
}

func (m *InstallManager) resolve(lock Lockfile, reference string) (string, LockEntry, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return "", LockEntry{}, ErrNotFound
	}
	if ref, err := ParseSkillReference(reference); err == nil && ref.Owner != "" {
		canonical, _ := canonicalReference(ref)
		for identity, entry := range lock.Skills {
			if strings.EqualFold(entry.Registry, m.registryID) && strings.EqualFold(entry.OwnerHandle, canonical.Owner) && strings.EqualFold(entry.Slug, canonical.Slug) {
				return identity, entry, nil
			}
		}
		return "", LockEntry{}, ErrNotFound
	}
	ref, err := ParseSkillReference(reference)
	if err != nil {
		return "", LockEntry{}, err
	}
	slug := strings.ToLower(ref.Slug)
	var match string
	var entry LockEntry
	for identity, candidate := range lock.Skills {
		if strings.EqualFold(candidate.Slug, slug) {
			if match != "" {
				return "", LockEntry{}, fmt.Errorf("%w: %s matches %s and %s", ErrAmbiguousSkill, reference, match, identity)
			}
			match, entry = identity, candidate
		}
	}
	if match == "" {
		return "", LockEntry{}, ErrNotFound
	}
	return match, entry, nil
}

func (m *InstallManager) migrateLockfile(lock *Lockfile) error {
	migrated := make(map[string]LockEntry, len(lock.Skills))
	type move struct {
		from string
		to   string
	}
	moves := make([]move, 0, len(lock.Skills))
	for legacySlug, entry := range lock.Skills {
		legacyPath := filepath.Join(m.skillsDir, filepath.Base(legacySlug))
		origin, err := readOrigin(legacyPath)
		if err != nil {
			return fmt.Errorf("migrate installed skill %s: %w", legacySlug, err)
		}
		if origin == nil {
			return fmt.Errorf("migrate installed skill %s: origin metadata is missing", legacySlug)
		}
		entry.Registry, entry.OwnerHandle, entry.Slug = origin.Registry, origin.OwnerHandle, origin.Slug
		if entry.Registry == "" {
			entry.Registry = m.registryID
		}
		if !strings.EqualFold(strings.TrimRight(entry.Registry, "/"), m.registryID) {
			return fmt.Errorf("migrate installed skill %s: registry %q does not match manager registry %q", legacySlug, entry.Registry, m.registryID)
		}
		entry.Registry = m.registryID
		if entry.Slug == "" {
			entry.Slug = legacySlug
		}
		ref, err := canonicalReference(SkillReference{Owner: entry.OwnerHandle, Slug: entry.Slug})
		if err != nil {
			return fmt.Errorf("migrate installed skill %s: %w", legacySlug, err)
		}
		entry.OwnerHandle, entry.Slug = ref.Owner, ref.Slug
		identity := m.identity(ref)
		if _, exists := migrated[identity]; exists {
			return fmt.Errorf("migrate ClawHub lockfile: duplicate identity %s", identity)
		}
		entry.Directory = installDirectory(identity, entry.Slug)
		newPath := filepath.Join(m.skillsDir, entry.Directory)
		if legacyPath != newPath {
			if _, err := os.Stat(newPath); err == nil {
				return fmt.Errorf("migrate ClawHub lockfile: target %s already exists", entry.Directory)
			} else if !os.IsNotExist(err) {
				return err
			}
			moves = append(moves, move{legacyPath, newPath})
		}
		migrated[identity] = entry
	}
	moved := 0
	rollback := func() {
		for i := moved - 1; i >= 0; i-- {
			_ = os.Rename(moves[i].to, moves[i].from)
		}
	}
	for _, movement := range moves {
		if err := os.Rename(movement.from, movement.to); err != nil {
			rollback()
			return fmt.Errorf("migrate installed skill directory: %w", err)
		}
		moved++
	}
	lock.Version, lock.Skills = 2, migrated
	if err := m.writeLockfile(lock); err != nil {
		rollback()
		return err
	}
	return nil
}

func readStoredVerification(target string) (*Verification, error) {
	content, err := os.ReadFile(filepath.Join(target, ".clawhub", "verification.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var verification Verification
	if err := json.Unmarshal(content, &verification); err != nil {
		return nil, err
	}
	if verification.Schema != "clawhub.skill.verify.v1" {
		return nil, errors.New("unsupported stored verification envelope")
	}
	return &verification, nil
}

func readInstalledFiles(root string) (map[string][]byte, error) {
	result := make(map[string][]byte)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if relative == ".clawhub" || relative == ".git" || relative == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("installed skill contains symlink %q", relative)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[relative] = content
		return nil
	})
	return result, err
}

func fingerprintFiles(files map[string][]byte) string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, path := range paths {
		digest := sha256.Sum256(files[path])
		_, _ = io.WriteString(hash, path+":"+hex.EncodeToString(digest[:])+"\n")
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writeAtomicJSON(path string, value interface{}) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tempPath, 0o600); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
