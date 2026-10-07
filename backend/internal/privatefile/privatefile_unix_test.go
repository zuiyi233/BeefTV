//go:build !windows

package privatefile_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"infinite-canvas/backend/internal/privatefile"
)

func TestVerifyAcceptsUserOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner_token")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Verify(path); err != nil {
		t.Fatalf("0600 文件必须通过: %v", err)
	}
}

func TestVerifyRejectsGroupReadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner_token")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Verify(path); !errors.Is(err, privatefile.ErrNotPrivate) {
		t.Fatalf("0644 文件必须被判定为不私有，得到 %v", err)
	}
}
