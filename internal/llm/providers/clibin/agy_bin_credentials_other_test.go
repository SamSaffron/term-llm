//go:build !darwin

package clibin

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAgyCopyCredentialsAllowsNativeCredentialLookup(t *testing.T) {
	realHome := t.TempDir()
	privateHome := t.TempDir()
	p := NewAgyBinProvider("", nil)
	p.realHome, p.agyHome = realHome, privateHome

	if err := p.copyCredentials(); err != nil {
		t.Fatalf("native credential lookup rejected without a legacy token: %v", err)
	}
	entries, err := os.ReadDir(privateHome)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("native credential lookup should not copy user files: %v", entries)
	}
	if p.realHome == p.agyHome {
		t.Fatal("native credential lookup replaced the isolated HOME")
	}
}

func TestAgyBinHasCredentialsDefersNativeLoginCheckToInstalledCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	binDir := t.TempDir()
	t.Setenv("PATH", binDir)
	if AgyBinHasCredentials() {
		t.Fatal("agy must be installed to advertise native login support")
	}
	name := "agy"
	if runtime.GOOS == "windows" {
		name += ".exe"
		t.Setenv("PATHEXT", ".EXE")
	}
	if err := os.WriteFile(filepath.Join(binDir, name), nil, 0o700); err != nil {
		t.Fatal(err)
	}
	if !AgyBinHasCredentials() {
		t.Fatal("installed agy should be offered without requiring its legacy token file")
	}
}

func TestAgyNativeLoginFailureIsStillReported(t *testing.T) {
	p := NewAgyBinProvider("", nil)
	err := p.commandError(errors.New("exit status 3"), []string{"Authentication expired. Please log back in."}, nil)
	var userErr *UserFacingProviderError
	if !errors.As(err, &userErr) || userErr.Summary != "agy is not logged in" {
		t.Fatalf("commandError = %v, want native login instructions", err)
	}
}
