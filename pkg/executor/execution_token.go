package executor

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// ExecutionTokenClaims represents the JWT claims for a code execution token
type ExecutionTokenClaims struct {
	RunID  string `json:"run_id"`
	NodeID string `json:"node_id"`
	jwt.RegisteredClaims
}

// ExecutionTokenService handles JWT token generation and validation for code execution
type ExecutionTokenService struct {
	secretKey []byte
}

// NewExecutionTokenService creates a new token service
func NewExecutionTokenService(secretKey string) *ExecutionTokenService {
	return &ExecutionTokenService{
		secretKey: []byte(secretKey),
	}
}

// GenerateToken creates a new JWT token for a code execution
// Token is scoped to a specific runID and nodeID and expires after 1 hour
func (s *ExecutionTokenService) GenerateToken(runID, nodeID string) (string, error) {
	if runID == "" || nodeID == "" {
		return "", fmt.Errorf("runID and nodeID are required")
	}

	claims := &ExecutionTokenClaims{
		RunID:  runID,
		NodeID: nodeID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(s.secretKey)
}

// ValidateToken validates a JWT token and returns the claims
func (s *ExecutionTokenService) ValidateToken(tokenString string) (*ExecutionTokenClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &ExecutionTokenClaims{}, func(token *jwt.Token) (interface{}, error) {
		// Verify signing method
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return s.secretKey, nil
	})

	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	if claims, ok := token.Claims.(*ExecutionTokenClaims); ok && token.Valid {
		return claims, nil
	}

	return nil, fmt.Errorf("invalid token claims")
}
