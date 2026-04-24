package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/executor"
	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

type StoredFile struct {
	Id        string    `json:"id"`
	Filename  string    `json:"filename"`
	MimeType  string    `json:"mimeType"`
	Size      int64     `json:"size"`
	Path      string    `json:"-"`
	RunId     string    `json:"runId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type FileStore struct {
	logger  *zap.SugaredLogger
	dir     string
	mu      sync.RWMutex
	files   map[string]*StoredFile
	runMap  map[string][]string
}

func NewFileStore(logger *zap.SugaredLogger) *FileStore {
	dir := filepath.Join(os.TempDir(), "openseal-files")
	os.MkdirAll(dir, 0755)

	fs := &FileStore{
		logger: logger,
		dir:    dir,
		files:  make(map[string]*StoredFile),
		runMap: make(map[string][]string),
	}
	go fs.cleanupLoop()
	return fs
}

func (fs *FileStore) Store(data []byte, filename, mimeType string) (*StoredFile, error) {
	id := generateID()
	path := filepath.Join(fs.dir, id)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return nil, fmt.Errorf("failed to write file: %w", err)
	}
	stored := &StoredFile{
		Id:        id,
		Filename:  filename,
		MimeType:  mimeType,
		Size:      int64(len(data)),
		Path:      path,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}
	fs.mu.Lock()
	fs.files[id] = stored
	fs.mu.Unlock()
	return stored, nil
}

func (fs *FileStore) StoreReader(reader io.Reader, filename, mimeType string) (*StoredFile, error) {
	id := generateID()
	path := filepath.Join(fs.dir, id)

	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("failed to create file: %w", err)
	}

	n, err := io.Copy(f, reader)
	f.Close()
	if err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("failed to write file: %w", err)
	}

	stored := &StoredFile{
		Id:        id,
		Filename:  filename,
		MimeType:  mimeType,
		Size:      n,
		Path:      path,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}
	fs.mu.Lock()
	fs.files[id] = stored
	fs.mu.Unlock()
	return stored, nil
}

func (fs *FileStore) Get(fileId string) (*StoredFile, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	f, ok := fs.files[fileId]
	if !ok {
		return nil, fmt.Errorf("file not found: %s", fileId)
	}
	return f, nil
}

func (fs *FileStore) GetReader(fileId string) (io.ReadCloser, error) {
	fs.mu.RLock()
	f, ok := fs.files[fileId]
	fs.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("file not found: %s", fileId)
	}
	return os.Open(f.Path)
}

func (fs *FileStore) Delete(fileId string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	f, ok := fs.files[fileId]
	if !ok {
		return nil
	}
	os.Remove(f.Path)
	delete(fs.files, fileId)
	return nil
}

func (fs *FileStore) DeleteByRunId(runId string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fileIds := fs.runMap[runId]
	for _, fileId := range fileIds {
		if f, ok := fs.files[fileId]; ok {
			os.Remove(f.Path)
			delete(fs.files, fileId)
		}
	}
	delete(fs.runMap, runId)
}

func (fs *FileStore) AssociateWithRun(fileId, runId string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if _, ok := fs.files[fileId]; !ok {
		return fmt.Errorf("file not found: %s", fileId)
	}
	fs.files[fileId].RunId = runId
	fs.runMap[runId] = append(fs.runMap[runId], fileId)
	return nil
}

func (fs *FileStore) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		fs.cleanup()
	}
}

func (fs *FileStore) cleanup() {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	now := time.Now()
	for id, f := range fs.files {
		if f.ExpiresAt.Before(now) {
			os.Remove(f.Path)
			delete(fs.files, id)
		}
	}
}

func generateID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	}
	return hex.EncodeToString(b)
}

type FileServer struct {
	logger            *zap.SugaredLogger
	fileStore         *FileStore
	server            *http.Server
	port              int
	tokenService      *executor.ExecutionTokenService
	contextStore      *executor.ToolExecutionContextStore
	grpcSkillRegistry *executor.GRPCSkillRegistry
	sentinelURL       string
	workerID          string
	portManager       *PortManager
}

func NewFileServer(port int, logger *zap.SugaredLogger) *FileServer {
	return &FileServer{
		logger:      logger,
		fileStore:   NewFileStore(logger),
		port:        port,
		portManager: NewPortManager(logger),
	}
}

func (s *FileServer) GetPortManager() *PortManager {
	return s.portManager
}

func (s *FileServer) SetGRPCSkillRegistry(registry *executor.GRPCSkillRegistry) {
	s.grpcSkillRegistry = registry
}

func (s *FileServer) SetSentinelConfig(sentinelURL, workerID string) {
	s.sentinelURL = sentinelURL
	s.workerID = workerID
}

func (s *FileServer) GetFileURL(fileId string) string {
	return fmt.Sprintf("http://localhost:%d/files/%s", s.port, fileId)
}

func (s *FileServer) GetFileStore() *FileStore {
	return s.fileStore
}

func (s *FileServer) SetToolDependencies(tokenService *executor.ExecutionTokenService, contextStore *executor.ToolExecutionContextStore) {
	s.tokenService = tokenService
	s.contextStore = contextStore
}

func (s *FileServer) Start() error {
	router := mux.NewRouter()

	router.HandleFunc("/files/{fileId}", s.handleGetFile).Methods("GET")
	router.HandleFunc("/files", s.handleStoreFile).Methods("POST")
	router.HandleFunc("/internal/emit", s.handleEmit).Methods("POST")

	if s.tokenService != nil && s.contextStore != nil {
		router.HandleFunc("/internal/executions/{runId}/nodes/{nodeId}/tools/invoke", s.handleToolInvocation).Methods("POST")
	}

	router.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}).Methods("GET")

	router.HandleFunc("/internal/skills/register", s.handleSkillRegister).Methods("POST")
	router.HandleFunc("/internal/ports/lease", s.handleLeasePort).Methods("POST")
	router.HandleFunc("/internal/ports/release", s.handleReleasePort).Methods("POST")
	router.HandleFunc("/internal/ports/renew", s.handleRenewPort).Methods("POST")
	router.HandleFunc("/internal/ports/stats", s.handlePortStats).Methods("GET")

	s.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: router,
	}

	portManagerStopCh := make(chan struct{})
	go s.portManager.StartCleanup(portManagerStopCh)

	s.logger.Infow("starting file server", "port", s.port)
	return s.server.ListenAndServe()
}

func (s *FileServer) Stop(ctx context.Context) error {
	if s.server != nil {
		return s.server.Shutdown(ctx)
	}
	return nil
}

func (s *FileServer) handleGetFile(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	fileId := vars["fileId"]

	file, err := s.fileStore.Get(fileId)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	reader, err := s.fileStore.GetReader(fileId)
	if err != nil {
		http.Error(w, "failed to read file", http.StatusInternalServerError)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", file.MimeType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", file.Filename))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", file.Size))

	io.Copy(w, reader)
}

func (s *FileServer) handleStoreFile(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(100 << 20); err != nil {
		http.Error(w, "failed to parse form", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "no file provided", http.StatusBadRequest)
		return
	}
	defer file.Close()

	mimeType := header.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	stored, err := s.fileStore.StoreReader(file, header.Filename, mimeType)
	if err != nil {
		http.Error(w, "failed to store file", http.StatusInternalServerError)
		return
	}

	response := map[string]interface{}{
		"id":       stored.Id,
		"filename": stored.Filename,
		"mimeType": stored.MimeType,
		"size":     stored.Size,
		"url":      s.GetFileURL(stored.Id),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

type emitRequest struct {
	RunId    int         `json:"runId"`
	NodeId   string      `json:"nodeId"`
	Data     interface{} `json:"data"`
	Progress float64     `json:"progress,omitempty"`
}

func (s *FileServer) handleEmit(w http.ResponseWriter, r *http.Request) {
	var req emitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.NodeId == "" {
		http.Error(w, "nodeId is required", http.StatusBadRequest)
		return
	}

	stream := executor.GetGlobalStreamRegistry().Get(req.RunId, req.NodeId)
	if stream == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"warning": "no stream channel configured",
		})
		return
	}

	done := make(chan struct{})
	item := executor.StreamItem{
		Data:     req.Data,
		Progress: req.Progress,
		Done:     done,
	}

	if !stream.Send(item) {
		http.Error(w, "stream closed", http.StatusGone)
		return
	}

	select {
	case <-done:
		if item.Error != nil {
			http.Error(w, item.Error.Error(), http.StatusInternalServerError)
			return
		}
	case <-time.After(5 * time.Minute):
		http.Error(w, "processing timeout", http.StatusGatewayTimeout)
		return
	case <-r.Context().Done():
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
	})
}

type toolInvocationRequest struct {
	ToolName  string                 `json:"tool_name"`
	Arguments map[string]interface{} `json:"arguments"`
}

type toolInvocationResponse struct {
	Success bool        `json:"success"`
	Result  interface{} `json:"result,omitempty"`
	Error   string      `json:"error,omitempty"`
}

func (s *FileServer) handleToolInvocation(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	runID := vars["runId"]
	nodeID := vars["nodeId"]

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		respondJSON(w, http.StatusUnauthorized, toolInvocationResponse{
			Success: false,
			Error:   "Missing authorization header",
		})
		return
	}

	const bearerPrefix = "Bearer "
	if len(authHeader) < len(bearerPrefix) || authHeader[:len(bearerPrefix)] != bearerPrefix {
		respondJSON(w, http.StatusUnauthorized, toolInvocationResponse{
			Success: false,
			Error:   "Invalid authorization format",
		})
		return
	}

	token := authHeader[len(bearerPrefix):]
	claims, err := s.tokenService.ValidateToken(token)
	if err != nil {
		respondJSON(w, http.StatusUnauthorized, toolInvocationResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid token: %v", err),
		})
		return
	}

	if claims.RunID != runID || claims.NodeID != nodeID {
		respondJSON(w, http.StatusForbidden, toolInvocationResponse{
			Success: false,
			Error:   "Token does not match execution context",
		})
		return
	}

	var req toolInvocationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, toolInvocationResponse{
			Success: false,
			Error:   "Invalid request body",
		})
		return
	}

	execCtx, err := s.contextStore.Get(runID, nodeID)
	if err != nil {
		respondJSON(w, http.StatusNotFound, toolInvocationResponse{
			Success: false,
			Error:   "Execution context not found",
		})
		return
	}

	result, err := execCtx.ExecuteTool(r.Context(), req.ToolName, req.Arguments)
	if err != nil {
		respondJSON(w, http.StatusOK, toolInvocationResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	respondJSON(w, http.StatusOK, toolInvocationResponse{
		Success: true,
		Result:  result,
	})
}

func respondJSON(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(data)
}

type SkillRegistrationRequest struct {
	SkillID   string   `json:"skillId"`
	Address   string   `json:"address"`
	NodeTypes []string `json:"nodeTypes"`
}

type PortLeaseRequest struct {
	SkillID string `json:"skillId"`
}

type PortLeaseResponse struct {
	Success bool   `json:"success"`
	Port    int    `json:"port"`
	Address string `json:"address"`
}

type PortReleaseRequest struct {
	SkillID string `json:"skillId"`
	Port    int    `json:"port"`
}

type PortRenewRequest struct {
	SkillID string `json:"skillId"`
	Port    int    `json:"port"`
}

func (s *FileServer) handleLeasePort(w http.ResponseWriter, r *http.Request) {
	var req PortLeaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}

	if req.SkillID == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "skillId is required"})
		return
	}

	port, err := s.portManager.LeasePort(req.SkillID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, PortLeaseResponse{
		Success: true,
		Port:    port,
		Address: fmt.Sprintf("localhost:%d", port),
	})
}

func (s *FileServer) handleReleasePort(w http.ResponseWriter, r *http.Request) {
	var req PortReleaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}

	if req.Port == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "port is required"})
		return
	}

	if err := s.portManager.ReleasePort(req.Port); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"port":    req.Port,
	})
}

func (s *FileServer) handleRenewPort(w http.ResponseWriter, r *http.Request) {
	var req PortRenewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}

	if req.Port == 0 || req.SkillID == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "port and skillId are required"})
		return
	}

	if err := s.portManager.RenewLease(req.Port, req.SkillID); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"port":    req.Port,
	})
}

func (s *FileServer) handlePortStats(w http.ResponseWriter, r *http.Request) {
	stats := s.portManager.GetStats()
	respondJSON(w, http.StatusOK, stats)
}

func (s *FileServer) handleSkillRegister(w http.ResponseWriter, r *http.Request) {
	var req SkillRegistrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}

	if s.grpcSkillRegistry != nil {
		if err := s.grpcSkillRegistry.RegisterSkill(r.Context(), req.Address); err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"skillId":   req.SkillID,
		"address":   req.Address,
		"nodeTypes": req.NodeTypes,
	})
}
