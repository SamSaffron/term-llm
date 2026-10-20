//go:build !js && !wasip1

package main

func newShellExecutor() ShellExecutor { return &unavailableShellExecutor{} }
