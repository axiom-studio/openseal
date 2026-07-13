// Package resourcefs implements the local trusted-filesystem form of the
// product-neutral skill.ResourceStager contract.
package resourcefs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/axiom-studio/openseal/pkg/capability"
	kernelskill "github.com/axiom-studio/openseal/pkg/skill"
)

const (
	AdapterID        = "filesystem-sandbox/v1"
	maxResourceFiles = 2048
	maxResourceBytes = int64(128 << 20)
)

type Stager struct {
	root     string
	provider kernelskill.ResourceContentProvider
	mu       sync.Mutex
}

type manifest struct {
	Version      int                   `json:"version"`
	Revision     string                `json:"revision"`
	SourceDigest string                `json:"sourceDigest"`
	Resources    []capability.Resource `json:"resources"`
}

func New(root string, provider kernelskill.ResourceContentProvider) (*Stager, error) {
	root = strings.TrimSpace(root)
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) {
		return nil, errors.New("filesystem resource staging requires a non-root absolute sandbox directory")
	}
	if provider == nil {
		return nil, errors.New("filesystem resource staging requires a content provider")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create resource sandbox: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("secure resource sandbox: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve resource sandbox: %w", err)
	}
	info, err := os.Stat(realRoot)
	if err != nil || !info.IsDir() {
		return nil, errors.New("resource sandbox must resolve to a directory")
	}
	return &Stager{root: realRoot, provider: provider}, nil
}

func (s *Stager) StageResources(ctx context.Context, request kernelskill.ResourceStageRequest) (*kernelskill.ResourceStage, error) {
	if s == nil || s.provider == nil {
		return nil, errors.New("filesystem resource stager is not configured")
	}
	if err := kernelskill.ValidateResourceStageRequest(request); err != nil {
		return nil, err
	}
	resources, revision, err := normalizeResources(request.Resources)
	if err != nil {
		return nil, err
	}
	target := filepath.Join(s.root, stageKey(request))
	if !withinRoot(s.root, target) {
		return nil, errors.New("resource stage escaped sandbox root")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := verifyStage(target, request.SourceDigest, revision, resources); err == nil {
		return &kernelskill.ResourceStage{Root: target, Revision: revision, Adapter: AdapterID}, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("existing resource stage is invalid: %w", err)
	}

	temporary, err := os.MkdirTemp(s.root, ".stage-")
	if err != nil {
		return nil, fmt.Errorf("create temporary resource stage: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(temporary)
		}
	}()
	if err := os.Chmod(temporary, 0o700); err != nil {
		return nil, fmt.Errorf("secure temporary resource stage: %w", err)
	}
	for _, resource := range resources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		content, err := s.provider.ReadResource(ctx, request.Scope, request.SourceDigest, resource.Path)
		if err != nil {
			return nil, fmt.Errorf("read declared resource %q: %w", resource.Path, err)
		}
		if err := verifyContent(resource, content); err != nil {
			return nil, err
		}
		destination := filepath.Join(temporary, filepath.FromSlash(resource.Path))
		if !withinRoot(temporary, destination) {
			return nil, fmt.Errorf("resource %q escaped temporary stage", resource.Path)
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return nil, fmt.Errorf("create resource directory: %w", err)
		}
		mode := os.FileMode(0o400)
		if resource.Kind == "script" {
			mode = 0o500
		}
		if err := writeFile(destination, content, mode); err != nil {
			return nil, err
		}
	}
	encoded, _ := json.Marshal(manifest{Version: 1, Revision: revision, SourceDigest: request.SourceDigest, Resources: resources})
	if err := writeFile(filepath.Join(temporary, ".openseal-stage.json"), encoded, 0o400); err != nil {
		return nil, err
	}
	if err := rejectSymlinks(temporary); err != nil {
		return nil, err
	}
	if err := os.Rename(temporary, target); err != nil {
		if verifyErr := verifyStage(target, request.SourceDigest, revision, resources); verifyErr != nil {
			return nil, fmt.Errorf("publish resource stage: %w", err)
		}
		_ = os.RemoveAll(temporary)
	} else {
		committed = true
	}
	return &kernelskill.ResourceStage{Root: target, Revision: revision, Adapter: AdapterID}, nil
}

func normalizeResources(input []capability.Resource) ([]capability.Resource, string, error) {
	if len(input) > maxResourceFiles {
		return nil, "", fmt.Errorf("resource file count exceeds %d", maxResourceFiles)
	}
	resources := append([]capability.Resource(nil), input...)
	total := int64(0)
	seen := make(map[string]bool, len(resources))
	for index := range resources {
		clean := path.Clean(strings.ReplaceAll(strings.TrimSpace(resources[index].Path), "\\", "/"))
		if clean == "." || clean == "" || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsRune(clean, '\x00') {
			return nil, "", fmt.Errorf("resource path %q is not a safe relative path", resources[index].Path)
		}
		if seen[clean] {
			return nil, "", fmt.Errorf("duplicate resource path %q", clean)
		}
		seen[clean] = true
		resources[index].Path = clean
		resources[index].Digest = strings.ToLower(strings.TrimSpace(resources[index].Digest))
		decodedDigest, digestErr := hex.DecodeString(resources[index].Digest)
		if digestErr != nil || len(decodedDigest) != sha256.Size {
			return nil, "", fmt.Errorf("resource %q requires a SHA-256 digest", clean)
		}
		if resources[index].Size < 0 || resources[index].Size > maxResourceBytes-total {
			return nil, "", fmt.Errorf("declared resources exceed %d bytes", maxResourceBytes)
		}
		total += resources[index].Size
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].Path < resources[j].Path })
	encoded, _ := json.Marshal(resources)
	digest := sha256.Sum256(encoded)
	return resources, hex.EncodeToString(digest[:]), nil
}

func verifyContent(resource capability.Resource, content []byte) error {
	if int64(len(content)) != resource.Size {
		return fmt.Errorf("resource %q size does not match its declaration", resource.Path)
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != resource.Digest {
		return fmt.Errorf("resource %q digest does not match its declaration", resource.Path)
	}
	return nil
}

func stageKey(request kernelskill.ResourceStageRequest) string {
	payload := strings.Join([]string{request.Scope.Kind, request.Scope.ID, request.DeploymentID, request.BindingID, request.SkillID, request.SkillVersion, request.SourceDigest}, "\x00")
	digest := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(digest[:])
}

func verifyStage(root, sourceDigest, revision string, resources []capability.Resource) error {
	encoded, err := os.ReadFile(filepath.Join(root, ".openseal-stage.json"))
	if err != nil {
		return err
	}
	var stored manifest
	if err := json.Unmarshal(encoded, &stored); err != nil || stored.Version != 1 || stored.Revision != revision || stored.SourceDigest != sourceDigest {
		return errors.New("resource stage manifest does not match activation")
	}
	if err := rejectSymlinks(root); err != nil {
		return err
	}
	for _, resource := range resources {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(resource.Path)))
		if err != nil {
			return err
		}
		if err := verifyContent(resource, content); err != nil {
			return err
		}
	}
	return nil
}

func rejectSymlinks(root string) error {
	return filepath.Walk(root, func(current string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("resource stage contains symlink %q", current)
		}
		return nil
	})
}

func writeFile(name string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create staged resource: %w", err)
	}
	if _, err = file.Write(content); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return fmt.Errorf("write staged resource: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close staged resource: %w", closeErr)
	}
	return nil
}

func withinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
