//go:build !windows

package transport

func diskRoot() string { return "/" }
