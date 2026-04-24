package executor

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

func TestExecutionTokenService_GenerateToken(t *testing.T) {
	service := NewExecutionTokenService("test-secret-key")

	tests := []struct {
		name    string
		runID   string
		nodeID  string
		wantErr bool
	}{
		{
			name:    "valid token generation",
			runID:   "run-123",
			nodeID:  "node-456",
			wantErr: false,
		},
		{
			name:    "empty runID",
			runID:   "",
			nodeID:  "node-456",
			wantErr: true,
		},
		{
			name:    "empty nodeID",
			runID:   "run-123",
			nodeID:  "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := service.GenerateToken(tt.runID, tt.nodeID)

			if (err != nil) != tt.wantErr {
				t.Errorf("GenerateToken() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && token == "" {
				t.Error("GenerateToken() returned empty token")
			}
		})
	}
}

func TestExecutionTokenService_ValidateToken(t *testing.T) {
	service := NewExecutionTokenService("test-secret-key")
	runID := "run-123"
	nodeID := "node-456"

	// Generate a valid token
	validToken, err := service.GenerateToken(runID, nodeID)
	if err != nil {
		t.Fatalf("Failed to generate token: %v", err)
	}

	tests := []struct {
		name       string
		token      string
		wantRunID  string
		wantNodeID string
		wantErr    bool
	}{
		{
			name:       "valid token",
			token:      validToken,
			wantRunID:  runID,
			wantNodeID: nodeID,
			wantErr:    false,
		},
		{
			name:    "invalid token",
			token:   "invalid.token.here",
			wantErr: true,
		},
		{
			name:    "empty token",
			token:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims, err := service.ValidateToken(tt.token)

			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateToken() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if claims.RunID != tt.wantRunID {
					t.Errorf("ValidateToken() runID = %v, want %v", claims.RunID, tt.wantRunID)
				}
				if claims.NodeID != tt.wantNodeID {
					t.Errorf("ValidateToken() nodeID = %v, want %v", claims.NodeID, tt.wantNodeID)
				}
			}
		})
	}
}

func TestExecutionTokenService_TokenExpiration(t *testing.T) {
	service := NewExecutionTokenService("test-secret-key")

	// Create an expired token manually
	claims := &ExecutionTokenClaims{
		RunID:  "run-123",
		NodeID: "node-456",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)), // Expired 1 hour ago
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(service.secretKey)
	if err != nil {
		t.Fatalf("Failed to sign token: %v", err)
	}

	// Try to validate expired token
	_, err = service.ValidateToken(tokenString)
	if err == nil {
		t.Error("ValidateToken() should reject expired token")
	}
}

func TestExecutionTokenService_WrongSecret(t *testing.T) {
	service1 := NewExecutionTokenService("secret1")
	service2 := NewExecutionTokenService("secret2")

	// Generate token with service1
	token, err := service1.GenerateToken("run-123", "node-456")
	if err != nil {
		t.Fatalf("Failed to generate token: %v", err)
	}

	// Try to validate with service2 (different secret)
	_, err = service2.ValidateToken(token)
	if err == nil {
		t.Error("ValidateToken() should reject token signed with different secret")
	}
}
