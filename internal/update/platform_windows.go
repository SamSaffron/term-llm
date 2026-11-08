//go:build windows

package update

func releaseArchiveSuffix() string { return ".zip" }

func releaseBinarySuffix() string { return ".exe" }
