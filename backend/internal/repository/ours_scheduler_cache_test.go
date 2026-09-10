package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// fork 私有护栏：调度快照缓存（sched:meta:<id>，分组名单从这里读）必须保留
// 登记开关与本组标记。filterSchedulerCredentials 是白名单，漏了这两个键时
// 路由名单看不到登记，分档、健康降级、会话回家全部静默失效——而管理端
// scheduler_scores 直接读库，照样显示正确，所以只能靠这条测试兜住。
func TestOursSchedulerMetadataKeepsTieringCredentials(t *testing.T) {
	account := service.Account{
		ID: 1,
		Credentials: map[string]any{
			"ours_tiering":     true,
			"ours_home_groups": "11,12,13",
			"model_mapping":    map[string]any{"gpt-5.6-sol": "gpt-5.6-sol"},
			"access_token":     "secret-token",
			"refresh_token":    "secret-refresh",
		},
	}

	meta := buildSchedulerMetadataAccount(account)
	require.Equal(t, true, meta.Credentials["ours_tiering"])
	require.Equal(t, "11,12,13", meta.Credentials["ours_home_groups"])
	require.NotNil(t, meta.Credentials["model_mapping"])
	require.NotContains(t, meta.Credentials, "access_token", "白名单不得放进 token")
	require.NotContains(t, meta.Credentials, "refresh_token", "白名单不得放进 token")
}

// 端到端：经过 JSON 序列化（写入 Redis 的真实形态）后，service 层仍能识别登记与本组。
func TestOursSchedulerMetadataRoundTripKeepsEnrollment(t *testing.T) {
	account := service.Account{
		ID:       7,
		Platform: service.PlatformOpenAI,
		Credentials: map[string]any{
			"ours_tiering":     true,
			"ours_home_groups": "11,12,13",
		},
	}
	_, metaPayload, err := marshalSchedulerCacheAccount(account)
	require.NoError(t, err)

	decoded, err := decodeCachedAccount(string(metaPayload))
	require.NoError(t, err)
	require.Equal(t, true, decoded.Credentials["ours_tiering"])
	require.Equal(t, "11,12,13", decoded.Credentials["ours_home_groups"])
}
