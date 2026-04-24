package executor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

// ToolProxyHandler handles HTTP requests for tool invocation from code executors
type ToolProxyHandler struct {
	logger       *zap.SugaredLogger
	tokenService *ExecutionTokenService
	contextStore *ToolExecutionContextStore
}

// NewToolProxyHandler creates a new tool proxy handler
func NewToolProxyHandler(logger *zap.SugaredLogger, tokenService *ExecutionTokenService, contextStore *ToolExecutionContextStore) *ToolProxyHandler {
	return &ToolProxyHandler{
		logger:       logger,
		tokenService: tokenService,
		contextStore: contextStore,
	}
}

// ToolInvocationRequest represents a tool invocation request from Python SDK
type ToolInvocationRequest struct {
	ToolName  string                 `json:"tool_name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// ToolInvocationResponse represents the response to a tool invocation
type ToolInvocationResponse struct {
	Success bool        `json:"success"`
	Result  interface{} `json:"result,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// HandleToolInvocation processes a tool invocation request
// POST /api/v1/agent/executions/:runId/nodes/:nodeId/tools/invoke
func (h *ToolProxyHandler) HandleToolInvocation(w http.ResponseWriter, r *http.Request) {
	// Extract runID and nodeID from URL
	vars := mux.Vars(r)
	runID := vars["runId"]
	nodeID := vars["nodeId"]

	if runID == "" || nodeID == "" {
		h.writeError(w, http.StatusBadRequest, "runId and nodeId are required")
		return
	}

	// Extract and validate JWT token from Authorization header
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		h.writeError(w, http.StatusUnauthorized, "Authorization header required")
		return
	}

	// Extract token from "Bearer <token>"
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || parts[0] != "Bearer" {
		h.writeError(w, http.StatusUnauthorized, "Invalid Authorization header format")
		return
	}
	token := parts[1]

	// Validate JWT token
	claims, err := h.tokenService.ValidateToken(token)
	if err != nil {
		h.logger.Warnw("Invalid execution token", "error", err, "runId", runID, "nodeId", nodeID)
		h.writeError(w, http.StatusUnauthorized, fmt.Sprintf("Invalid token: %v", err))
		return
	}

	// Verify token matches the requested runID and nodeID
	if claims.RunID != runID || claims.NodeID != nodeID {
		h.logger.Warnw("Token runID/nodeID mismatch",
			"tokenRunID", claims.RunID, "requestRunID", runID,
			"tokenNodeID", claims.NodeID, "requestNodeID", nodeID)
		h.writeError(w, http.StatusForbidden, "Token does not match execution context")
		return
	}

	// Parse request body
	var req ToolInvocationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, fmt.Sprintf("Invalid request body: %v", err))
		return
	}

	if req.ToolName == "" {
		h.writeError(w, http.StatusBadRequest, "tool_name is required")
		return
	}

	// Get execution context
	execCtx, err := h.contextStore.Get(runID, nodeID)
	if err != nil {
		h.logger.Errorw("Failed to get execution context", "error", err, "runId", runID, "nodeId", nodeID)
		h.writeError(w, http.StatusNotFound, "Execution context not found")
		return
	}

	h.logger.Infow("Executing tool",
		"runId", runID,
		"nodeId", nodeID,
		"toolName", req.ToolName,
		"arguments", req.Arguments)

	// Execute tool
	result, err := execCtx.ExecuteTool(r.Context(), req.ToolName, req.Arguments)
	if err != nil {
		h.logger.Errorw("Tool execution failed",
			"error", err,
			"runId", runID,
			"nodeId", nodeID,
			"toolName", req.ToolName)
		h.writeJSON(w, http.StatusOK, ToolInvocationResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	// Return success response
	h.writeJSON(w, http.StatusOK, ToolInvocationResponse{
		Success: true,
		Result:  result,
	})
}

// writeError writes an error response
func (h *ToolProxyHandler) writeError(w http.ResponseWriter, statusCode int, message string) {
	h.writeJSON(w, statusCode, ToolInvocationResponse{
		Success: false,
		Error:   message,
	})
}

// writeJSON writes a JSON response
func (h *ToolProxyHandler) writeJSON(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		h.logger.Errorw("Failed to encode JSON response", "error", err)
	}
}
