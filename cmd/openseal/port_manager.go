package main

import (
	"fmt"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"
)

// PortRange defines the range of ports available for skill allocation
const (
	PortRangeStart = 50000
	PortRangeEnd   = 60000
)

// PortLease represents a leased port
type PortLease struct {
	Port      int
	SkillID   string
	Address   string
	LeasedAt  time.Time
	ExpiresAt time.Time
}

// PortManager manages the pool of available ports for skills
type PortManager struct {
	logger       *zap.SugaredLogger
	available    map[int]bool
	leased       map[int]*PortLease
	mu           sync.RWMutex
	leaseTimeout time.Duration
}

// NewPortManager creates a new port manager
func NewPortManager(logger *zap.SugaredLogger) *PortManager {
	available := make(map[int]bool)
	for port := PortRangeStart; port <= PortRangeEnd; port++ {
		available[port] = true
	}

	return &PortManager{
		logger:       logger,
		available:    available,
		leased:       make(map[int]*PortLease),
		leaseTimeout: 5 * time.Minute, // Default lease timeout
	}
}

// LeasePort requests a port for a skill
// Returns the port number and an error if no ports are available
func (pm *PortManager) LeasePort(skillID string) (int, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Find first available port
	for port, isAvailable := range pm.available {
		if !isAvailable {
			continue
		}

		// Check if port is actually free on the system
		if !pm.isPortFree(port) {
			pm.logger.Warnw("port marked as available but is in use", "port", port)
			pm.available[port] = false
			continue
		}

		// Lease the port
		pm.available[port] = false
		lease := &PortLease{
			Port:      port,
			SkillID:   skillID,
			LeasedAt:  time.Now(),
			ExpiresAt: time.Now().Add(pm.leaseTimeout),
		}
		pm.leased[port] = lease

		pm.logger.Infow("leased port to skill",
			"skillId", skillID,
			"port", port,
			"expiresAt", lease.ExpiresAt)

		return port, nil
	}

	// No ports available - try to clean up expired leases
	pm.cleanupExpiredLeases()

	// Try again after cleanup
	for port, isAvailable := range pm.available {
		if isAvailable && pm.isPortFree(port) {
			pm.available[port] = false
			lease := &PortLease{
				Port:      port,
				SkillID:   skillID,
				LeasedAt:  time.Now(),
				ExpiresAt: time.Now().Add(pm.leaseTimeout),
			}
			pm.leased[port] = lease

			pm.logger.Infow("leased port after cleanup",
				"skillId", skillID,
				"port", port)

			return port, nil
		}
	}

	return 0, fmt.Errorf("no ports available in range %d-%d", PortRangeStart, PortRangeEnd)
}

// ReleasePort releases a port back to the pool
func (pm *PortManager) ReleasePort(port int) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	lease, exists := pm.leased[port]
	if !exists {
		pm.logger.Warnw("attempted to release unleased port", "port", port)
		return nil
	}

	pm.logger.Infow("releasing port",
		"skillId", lease.SkillID,
		"port", port)

	delete(pm.leased, port)
	pm.available[port] = true

	return nil
}

// RenewLease extends the lease timeout for a port
func (pm *PortManager) RenewLease(port int, skillID string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	lease, exists := pm.leased[port]
	if !exists {
		return fmt.Errorf("port %d is not leased", port)
	}

	if lease.SkillID != skillID {
		return fmt.Errorf("skill %s does not own port %d", skillID, port)
	}

	lease.ExpiresAt = time.Now().Add(pm.leaseTimeout)
	pm.logger.Debugw("renewed port lease",
		"skillId", skillID,
		"port", port,
		"newExpiry", lease.ExpiresAt)

	return nil
}

// GetLease returns the lease info for a port
func (pm *PortManager) GetLease(port int) *PortLease {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	return pm.leased[port]
}

// GetSkillPort returns the port for a skill
func (pm *PortManager) GetSkillPort(skillID string) (int, bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	for port, lease := range pm.leased {
		if lease.SkillID == skillID {
			return port, true
		}
	}

	return 0, false
}

// GetStats returns port management statistics
func (pm *PortManager) GetStats() map[string]interface{} {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	return map[string]interface{}{
		"totalPorts":   PortRangeEnd - PortRangeStart + 1,
		"available":    len(pm.available),
		"leased":       len(pm.leased),
		"utilization":  float64(len(pm.leased)) / float64(PortRangeEnd-PortRangeStart+1) * 100,
	}
}

// ListLeases returns all active leases
func (pm *PortManager) ListLeases() []*PortLease {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	leases := make([]*PortLease, 0, len(pm.leased))
	for _, lease := range pm.leased {
		leases = append(leases, lease)
	}

	return leases
}

// cleanupExpiredLeases removes expired leases (must be called with lock held)
func (pm *PortManager) cleanupExpiredLeases() {
	now := time.Now()
	for port, lease := range pm.leased {
		if lease.ExpiresAt.Before(now) {
			pm.logger.Infow("cleaning up expired lease",
				"skillId", lease.SkillID,
				"port", port,
				"expiredAt", lease.ExpiresAt)
			delete(pm.leased, port)
			pm.available[port] = true
		}
	}
}

// isPortFree checks if a port is actually free on the system
func (pm *PortManager) isPortFree(port int) bool {
	address := fmt.Sprintf(":%d", port)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return false
	}
	listener.Close()
	return true
}

// StartCleanup starts a background goroutine that cleans up expired leases
func (pm *PortManager) StartCleanup(stopCh <-chan struct{}) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			pm.mu.Lock()
			pm.cleanupExpiredLeases()
			pm.mu.Unlock()
		case <-stopCh:
			return
		}
	}
}
