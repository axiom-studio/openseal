//go:build !linux && !darwin && !windows

package client

import "context"

// NewService on a system without a supported service manager refuses every
// action with what to do instead.
func NewService(ServiceOptions, Store) ServiceManager { return unsupportedService{} }

type unsupportedService struct{}

func (unsupportedService) err() error {
	return &ErrNoServiceManager{Message: "helloseal-bridge can't install a background service on this system. Run helloseal-bridge start yourself."}
}
func (u unsupportedService) Install(context.Context) error { return u.err() }
func (unsupportedService) Uninstall(context.Context) error { return nil }
func (unsupportedService) Restart(context.Context) error   { return ErrServiceNotInstalled }
func (unsupportedService) Logs() string                    { return "" }
func (unsupportedService) Notes() []string                 { return nil }
func (unsupportedService) Status(context.Context) (ServiceStatus, error) {
	return ServiceStatus{}, nil
}
