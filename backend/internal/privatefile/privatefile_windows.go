//go:build windows

package privatefile

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// verify 枚举文件 DACL 里的显式授予项。Windows 上 os.Chmod 只影响只读位，
// 真正的读取权限来自 ACE，所以必须逐项核对授予对象。
func verify(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("读取凭据文件 DACL: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("解析凭据文件 DACL: %w", err)
	}
	if dacl == nil {
		// nil DACL 不是"没有权限"，而是不做任何限制：所有主体都能读。
		return fmt.Errorf("%w: %s 没有 DACL，任何主体都可读取", ErrNotPrivate, path)
	}
	accepted, err := acceptedPrincipals()
	if err != nil {
		return err
	}

	ace := (*windows.ACE_HEADER)(unsafe.Pointer(uintptr(unsafe.Pointer(dacl)) + unsafe.Sizeof(windows.ACL{})))
	for index := 0; index < int(dacl.AceCount); index++ {
		if uintptr(ace.AceSize) < unsafe.Sizeof(windows.ACE_HEADER{}) {
			return fmt.Errorf("凭据文件 %s 的第 %d 条 ACE 长度非法", path, index)
		}
		switch ace.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			allowed := (*windows.ACCESS_ALLOWED_ACE)(unsafe.Pointer(ace))
			sid := (*windows.SID)(unsafe.Pointer(&allowed.SidStart))
			if accepted.matches(sid) {
				break
			}
			return fmt.Errorf("%w: %s 第 %d 条 ACE 向 %s 授予了访问权", ErrNotPrivate, path, index, describeSID(sid))
		case windows.ACCESS_DENIED_ACE_TYPE:
			// 显式拒绝只会收紧权限，不影响私有性。
		default:
			// 对象型与系统审计型 ACE 的内存布局不同。这里宁可明确报错，
			// 也不要返回一个"看起来通过、其实没校验"的结论。
			return fmt.Errorf("凭据文件 %s 第 %d 条 ACE 类型为 %d，无法校验其私有性", path, index, ace.AceType)
		}
		ace = (*windows.ACE_HEADER)(unsafe.Pointer(uintptr(unsafe.Pointer(ace)) + uintptr(ace.AceSize)))
	}
	return nil
}

// principalSet 是允许读取凭据文件的主体集合。
//
// 本机用户是唯一预期的读者。SYSTEM、内置管理员与 CREATOR OWNER 是 Windows 上无法
// 回避的系统主体（备份、恢复与升级流程都以它们运行），保留它们不等于向同一台机器
// 上的其他登录用户开放——那类主体是 BUILTIN\Users、NT AUTHORITY\Authenticated Users
// 与 EVERYONE，本文件会把它们判为不私有。
type principalSet struct {
	user    *windows.SID
	system  *windows.SID
	admins  *windows.SID
	creator *windows.SID
}

func acceptedPrincipals() (*principalSet, error) {
	// 令牌 SID 归进程令牌缓冲区所有，这里只做比较，不释放也不跨调用保存。
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("解析当前用户 SID: %w", err)
	}
	set := &principalSet{user: user.User.Sid}
	if set.system, err = windows.CreateWellKnownSid(windows.WinLocalSystemSid); err != nil {
		return nil, fmt.Errorf("构造 SYSTEM SID: %w", err)
	}
	if set.admins, err = windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid); err != nil {
		return nil, fmt.Errorf("构造内置管理员 SID: %w", err)
	}
	if set.creator, err = windows.CreateWellKnownSid(windows.WinCreatorOwnerSid); err != nil {
		return nil, fmt.Errorf("构造 CREATOR OWNER SID: %w", err)
	}
	return set, nil
}

func (set *principalSet) matches(sid *windows.SID) bool {
	for _, candidate := range []*windows.SID{set.user, set.system, set.admins, set.creator} {
		if candidate != nil && sid.Equals(candidate) {
			return true
		}
	}
	return false
}

func describeSID(sid *windows.SID) string {
	if account, domain, _, err := sid.LookupAccount(""); err == nil && account != "" {
		if domain != "" {
			return domain + "\\" + account
		}
		return account
	}
	return sid.String()
}
