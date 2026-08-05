//go:build !windows
// +build !windows

package uv

import (
	"os"
	"testing"
)

func TestReader(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Errorf("expected no error, but got %s", err)
	}
	defer pw.Close()
	defer pr.Close()

	pollReader, err := newPollReader(pr)
	if err != nil {
		t.Errorf("expected no error, but got %s", err)
	}

	msg := "hello"
	n, err := pw.Write([]byte(msg))
	if n != len(msg) {
		t.Errorf("expected %d bytes written but got %d", len(msg), n)
	}
	if err != nil {
		t.Errorf("expected no error, but got %s", err)
	}

	if !pollReader.Cancel() {
		t.Errorf("expected cancellation to be success")
	}

	p := make([]byte, 1)
	n, err = pollReader.Read(p)
	if n != 0 {
		t.Errorf("expected 0 bytes read but got %d", n)
	}
	if err != ErrCanceled {
		t.Errorf("expected cancel error but got %s", err)
	}

	// Test that cancellation did not consume input and a new reader can still
	// read the complete message.
	pollReader, err = newPollReader(pr)
	if err != nil {
		t.Errorf("expected no error, but got %s", err)
	}
	p = make([]byte, len(msg))
	n, err = pollReader.Read(p)
	if n != len(msg) {
		t.Errorf("expected %d bytes read but got %d", len(msg), n)
	}
	if err != nil {
		t.Errorf("expected no error, but got %s", err)
	}
	if string(p[:n]) != msg[:n] {
		t.Errorf("expected to read %q but got %q", msg[:n], string(p[:n]))
	}
}
