//go:build !linux && !darwin && !windows

package cli

func installService(string) error { return errNoServiceManager }
func uninstallService() error     { return errNoServiceManager }
func serviceInstalled() bool      { return false }
