// Package privatefile 校验"只有本机用户可读"这条不变量。
//
// 生产代码统一用 os.Chmod(path, 0o600) 写凭据文件，但 Windows 的 Chmod 只能表达
// 只读位，权限实际由文件 DACL 决定。因此"0600"在两个平台上不是同一个保证，必须在
// Windows 上改查 DACL，否则发布平台上的凭据保护从未被验证。
package privatefile

import "errors"

// ErrNotPrivate 表示文件的访问控制允许预期之外的主体读取。
var ErrNotPrivate = errors.New("凭据文件权限不私有")

// Verify 检查 path 指向的文件只对本机用户与必要的系统主体开放。
// POSIX 平台比较权限位，Windows 平台枚举 DACL 中的显式授予项。
func Verify(path string) error { return verify(path) }
