//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type updateAccountCredsRepoStub struct {
	mockAccountRepoForGemini
	account     *Account
	updateCalls int
}

func (r *updateAccountCredsRepoStub) GetByID(ctx context.Context, id int64) (*Account, error) {
	return r.account, nil
}

func (r *updateAccountCredsRepoStub) Update(ctx context.Context, account *Account) error {
	r.updateCalls++
	r.account = account
	return nil
}

func TestUpdateAccount_PreservesSensitiveCredsWhenIncomingOmits(t *testing.T) {
	accountID := int64(202)
	repo := &updateAccountCredsRepoStub{
		account: &Account{
			ID:       accountID,
			Platform: PlatformAnthropic,
			Type:     AccountTypeOAuth,
			Status:   StatusActive,
			Credentials: map[string]any{
				"refresh_token": "rt-existing",
				"access_token":  "at-existing",
				"id_token":      "id-existing",
				"base_url":      "https://old.example.com",
			},
		},
	}
	svc := &adminServiceImpl{accountRepo: repo}

	// 模拟前端编辑：仅修改 base_url，没有传 token（脱敏后前端 spread 拿不到敏感键）
	updated, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Credentials: map[string]any{
			"base_url": "https://new.example.com",
		},
	})

	require.NoError(t, err)
	require.NotNil(t, updated)
	require.Equal(t, 1, repo.updateCalls)

	// 敏感键应保留
	require.Equal(t, "rt-existing", repo.account.Credentials["refresh_token"])
	require.Equal(t, "at-existing", repo.account.Credentials["access_token"])
	require.Equal(t, "id-existing", repo.account.Credentials["id_token"])
	// 非敏感键被替换
	require.Equal(t, "https://new.example.com", repo.account.Credentials["base_url"])
}

func TestUpdateAccount_RemovingPoolRetryStatusOverridePreservesCredentialsAndUsesDefaults(t *testing.T) {
	accountID := int64(205)
	current := map[string]any{
		"api_key":                      "synthetic-api-key",
		"refresh_token":                "synthetic-refresh-token",
		"access_token":                 "synthetic-access-token",
		"base_url":                     "https://deployed.example.invalid",
		"pool_mode":                    true,
		"pool_mode_retry_count":        float64(1),
		"pool_mode_retry_status_codes": []any{float64(400), float64(401), float64(403), float64(404), float64(429), float64(500), float64(502), float64(503), float64(504)},
		"model_mapping":                map[string]any{"gpt-test": "gpt-deployed"},
		"temp_unschedulable_enabled":   true,
		"temp_unschedulable_rules":     []any{map[string]any{"error_code": float64(503), "keywords": []any{"synthetic"}}},
		"custom_key":                   "custom-value",
	}
	repo := &updateAccountCredsRepoStub{account: &Account{
		ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: current,
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	incoming := map[string]any{
		"base_url":                   current["base_url"],
		"pool_mode":                  current["pool_mode"],
		"pool_mode_retry_count":      current["pool_mode_retry_count"],
		"model_mapping":              current["model_mapping"],
		"temp_unschedulable_enabled": current["temp_unschedulable_enabled"],
		"temp_unschedulable_rules":   current["temp_unschedulable_rules"],
		"custom_key":                 current["custom_key"],
	}

	updated, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{Credentials: incoming})
	require.NoError(t, err)
	require.NotNil(t, updated)
	require.Equal(t, 1, repo.updateCalls)

	persisted := repo.account.Credentials
	require.NotContains(t, persisted, "pool_mode_retry_status_codes")
	require.Equal(t, "synthetic-api-key", persisted["api_key"])
	require.Equal(t, "synthetic-refresh-token", persisted["refresh_token"])
	require.Equal(t, "synthetic-access-token", persisted["access_token"])
	expected := map[string]any{}
	for key, value := range current {
		if key != "pool_mode_retry_status_codes" {
			expected[key] = value
		}
	}
	require.Equal(t, expected, persisted)
	require.True(t, repo.account.IsPoolModeRetryableStatus(401))
	require.True(t, repo.account.IsPoolModeRetryableStatus(403))
	require.True(t, repo.account.IsPoolModeRetryableStatus(429))
	for _, status := range []int{400, 404, 500, 502, 503, 504} {
		require.False(t, repo.account.IsPoolModeRetryableStatus(status), "status %d", status)
	}
}

func TestUpdateAccount_ExplicitNewTokenOverwrites(t *testing.T) {
	accountID := int64(203)
	repo := &updateAccountCredsRepoStub{
		account: &Account{
			ID:       accountID,
			Platform: PlatformAnthropic,
			Type:     AccountTypeOAuth,
			Status:   StatusActive,
			Credentials: map[string]any{
				"refresh_token": "rt-old",
				"api_key":       "sk-old",
			},
		},
	}
	svc := &adminServiceImpl{accountRepo: repo}

	updated, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Credentials: map[string]any{
			"refresh_token": "rt-new",
			// api_key 没传 → 应保留旧值
		},
	})
	require.NoError(t, err)
	require.NotNil(t, updated)

	require.Equal(t, "rt-new", repo.account.Credentials["refresh_token"])
	require.Equal(t, "sk-old", repo.account.Credentials["api_key"])
}

func TestUpdateAccount_EmptyCredentialsSkipsUpdate(t *testing.T) {
	accountID := int64(204)
	repo := &updateAccountCredsRepoStub{
		account: &Account{
			ID:       accountID,
			Platform: PlatformAnthropic,
			Type:     AccountTypeOAuth,
			Status:   StatusActive,
			Credentials: map[string]any{
				"refresh_token": "rt-existing",
			},
		},
	}
	svc := &adminServiceImpl{accountRepo: repo}

	_, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Credentials: map[string]any{}, // len == 0 → 闸门跳过
		Name:        "renamed",
	})
	require.NoError(t, err)

	require.Equal(t, "rt-existing", repo.account.Credentials["refresh_token"], "空 credentials 不应触碰已有 token")
	require.Equal(t, "renamed", repo.account.Name)
}
