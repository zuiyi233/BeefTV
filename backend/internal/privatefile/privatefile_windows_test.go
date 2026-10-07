//go:build windows

package privatefile_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"infinite-canvas/backend/internal/privatefile"
)

// 默认 DACL 只授予当前用户与系统主体，这正是 0600 在 Windows 上想表达的同一件事。
// 该用例同时验证 ACE 遍历本身：它会逐条比较真实 ACE，直到走完而不报错。
func TestVerifyAcceptsUserOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner_token")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Verify(path); err != nil {
		t.Fatalf("只授予本机用户与系统主体的文件必须通过: %v", err)
	}
}

// 负例：NULL DACL 不是"没有任何权限"，而是不做限制——任何主体都能读写。
// 凭据文件落到这种状态必须被拒绝，否则这套校验就是空转。
func TestVerifyRejectsMissingDacl(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner_token")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, nil, nil); err != nil {
		t.Skipf("无法清除文件 DACL（需要 WRITE_DAC），跳过负例: %v", err)
	}
	err := privatefile.Verify(path)
	if err == nil {
		t.Fatal("没有 DACL 的凭据文件必须被拒绝")
	}
	if !errors.Is(err, privatefile.ErrNotPrivate) {
		t.Fatalf("应报告私有性失败，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "DACL") {
		t.Fatalf("错误必须说清是没有访问控制，得到 %q", err)
	}
}
