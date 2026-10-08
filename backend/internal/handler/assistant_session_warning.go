package handler

import (
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"infinite-canvas/backend/internal/app"
	"infinite-canvas/backend/internal/assistant"
)

// sessionWarningTTL 与 UI 会话同量级：这条提示只在当前进程里有意义，
// 后端重启后由下一次真实回合重新观测，不能当成持久状态或权限。
const sessionWarningTTL = 30 * time.Minute

const sessionWarningCapacity = 256

type sessionWarningEntry struct {
	reason string
	seenAt time.Time
}

// sessionWarningStore 只记录宿主已经告诉后端的事实：这条画布上的会话
// 因为换过模型而无法继续。数据来源是 /assistant/chat 本来就要读的 turn_end
// 行，以及宿主拒绝这次对话时的 reason，因此状态查询不需要再打一次宿主。
type sessionWarningStore struct {
	mu      sync.Mutex
	entries map[string]sessionWarningEntry
	now     func() time.Time
}

func newSessionWarningStore() *sessionWarningStore {
	return &sessionWarningStore{entries: map[string]sessionWarningEntry{}, now: time.Now}
}

func sessionWarningKey(userID, canvasID string) string {
	return userID + "\x00" + strings.TrimSpace(canvasID)
}

// observeTurnEnd 由已经结束的回合调用。只有换模型原因需要记下；其余终态
// （成功、取消、超时、别的失败）都说明这条会话在当前模型下真的跑过，旧提示必须失效。
func (s *sessionWarningStore) observeTurnEnd(userID, canvasID, errorReason string) {
	if errorReason != assistant.ReasonSessionModelChanged {
		s.clear(userID, canvasID)
		return
	}
	s.record(userID, canvasID, errorReason)
}

// observeRejection 用于宿主直接拒绝这次对话：只有原因确实是换模型时才记下，
// 其它拒绝（宿主不可达、会话忙）不能抹掉仍然成立的提示。
func (s *sessionWarningStore) observeRejection(userID, canvasID, reason string) {
	if reason == assistant.ReasonSessionModelChanged {
		s.record(userID, canvasID, reason)
	}
}

func (s *sessionWarningStore) record(userID, canvasID, reason string) {
	if s == nil {
		return
	}
	userID = strings.TrimSpace(userID)
	if userID == "" || strings.TrimSpace(canvasID) == "" {
		return
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.entries {
		if now.Sub(entry.seenAt) > sessionWarningTTL {
			delete(s.entries, key)
		}
	}
	if len(s.entries) >= sessionWarningCapacity {
		// 容量到顶只丢最旧的提示：它是可丢的说明，不能因为它记不下而影响回合本身。
		oldestKey, oldest := "", now
		for key, entry := range s.entries {
			if entry.seenAt.Before(oldest) {
				oldestKey, oldest = key, entry.seenAt
			}
		}
		if oldestKey != "" {
			delete(s.entries, oldestKey)
		}
	}
	s.entries[sessionWarningKey(userID, canvasID)] = sessionWarningEntry{reason: reason, seenAt: now}
}

// clear 用于新建会话和正常结束的回合：删除只按精确键，不做前缀匹配，
// 避免一个画布的动作抹掉另一个画布的提示。
func (s *sessionWarningStore) clear(userID, canvasID string) {
	if s == nil {
		return
	}
	userID = strings.TrimSpace(userID)
	if userID == "" || strings.TrimSpace(canvasID) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, sessionWarningKey(userID, canvasID))
}

// get 是纯内存读，并只在到期后失效；不发起任何上游请求。
func (s *sessionWarningStore) get(userID, canvasID string) (string, bool) {
	if s == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(canvasID) == "" {
		return "", false
	}
	key := sessionWarningKey(userID, canvasID)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, found := s.entries[key]
	if !found {
		return "", false
	}
	if s.now().Sub(entry.seenAt) > sessionWarningTTL {
		delete(s.entries, key)
		return "", false
	}
	return entry.reason, true
}

// statusSessionWarning 只在界面显式带上 canvasId 时才附带会话提示：
// /assistant/status 本身是免身份的，解析不出当前用户就什么也不带，
// 既不新增 403，也不让状态查询因为归属信息缺失而等待或失败。
func statusSessionWarning(c *gin.Context, svc *app.Service, warnings *sessionWarningStore) (string, bool) {
	canvasID := strings.TrimSpace(c.Query("canvasId"))
	if canvasID == "" {
		return "", false
	}
	user, err := currentUser(c, svc)
	if err != nil || user == nil {
		return "", false
	}
	return warnings.get(user.ID, canvasID)
}
