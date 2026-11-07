//go:build !windows

package update

func releaseArchiveSuffix() string { return ".tar.gz" }

func releaseBinarySuffix() string { return "" }
