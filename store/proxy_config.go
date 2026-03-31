package store

import (
	"encoding/json"
	"os"
)

type ProxyConfig struct {
	ProxyURL                        string `json:"proxyURL"`
	CacheEnabled                    bool   `json:"cacheEnabled"`
	CacheTTLSeconds                 int    `json:"cacheTTLSeconds"`
	BusinessCacheHitRate            int    `json:"businessCacheHitRate"`
	ClientCacheHitRate              int    `json:"clientCacheHitRate"`
	CacheHitRateJitter              int    `json:"cacheHitRateJitter"`
	CacheMaxHitRate                 int    `json:"cacheMaxHitRate"`
	ResponsesApiWebSearchEnabled    bool   `json:"responsesApiWebSearchEnabled"`
	ResponsesFunctionApplyPatchEnabled bool `json:"responsesFunctionApplyPatchEnabled"`
	PreferNativeMessagesByModel     bool   `json:"preferNativeMessagesByModel"`
}

func GetProxyConfig() (ProxyConfig, error) {
	data, err := os.ReadFile(ProxyConfigFile())
	if err != nil {
		if os.IsNotExist(err) {
			return defaultProxyConfig(), nil
		}
		return ProxyConfig{}, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return defaultProxyConfig(), nil
	}

	var cfg ProxyConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		cfg = defaultProxyConfig()
	}
	if _, ok := raw["cacheTTLSeconds"]; !ok {
		cfg.CacheTTLSeconds = 300
	}
	if _, ok := raw["cacheEnabled"]; !ok {
		cfg.CacheEnabled = cfg.CacheTTLSeconds > 0
	}
	if _, ok := raw["businessCacheHitRate"]; !ok {
		cfg.BusinessCacheHitRate = 4
	}
	if _, ok := raw["clientCacheHitRate"]; !ok {
		cfg.ClientCacheHitRate = 2
	}
	if _, ok := raw["cacheHitRateJitter"]; !ok {
		cfg.CacheHitRateJitter = 8
	}
	if _, ok := raw["cacheMaxHitRate"]; !ok {
		cfg.CacheMaxHitRate = 92
	}
	if _, ok := raw["responsesApiWebSearchEnabled"]; !ok {
		cfg.ResponsesApiWebSearchEnabled = true
	}
	if _, ok := raw["responsesFunctionApplyPatchEnabled"]; !ok {
		cfg.ResponsesFunctionApplyPatchEnabled = true
	}
	if _, ok := raw["preferNativeMessagesByModel"]; !ok {
		cfg.PreferNativeMessagesByModel = true
	}
	if cfg.CacheTTLSeconds < 0 {
		cfg.CacheTTLSeconds = 0
	}
	cfg = normalizeProxyConfig(cfg)
	if !cfg.CacheEnabled {
		cfg.CacheTTLSeconds = 0
	}
	return cfg, nil
}

func UpdateProxyConfig(cfg ProxyConfig) error {
	cfg = normalizeProxyConfig(cfg)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ProxyConfigFile(), data, 0644)
}

func defaultProxyConfig() ProxyConfig {
	return ProxyConfig{
		CacheEnabled:         true,
		CacheTTLSeconds:      300,
		BusinessCacheHitRate: 4,
		ClientCacheHitRate:   2,
		CacheHitRateJitter:   8,
		CacheMaxHitRate:      92,
		ResponsesApiWebSearchEnabled: true,
		ResponsesFunctionApplyPatchEnabled: true,
		PreferNativeMessagesByModel: true,
	}
}

func normalizeProxyConfig(cfg ProxyConfig) ProxyConfig {
	if cfg.CacheTTLSeconds < 0 {
		cfg.CacheTTLSeconds = 0
	}
	cfg.CacheEnabled = cfg.CacheEnabled || cfg.CacheTTLSeconds > 0
	if !cfg.CacheEnabled {
		cfg.CacheTTLSeconds = 0
	}
	cfg.BusinessCacheHitRate = clampProxyPercent(cfg.BusinessCacheHitRate, 4)
	cfg.ClientCacheHitRate = clampProxyPercent(cfg.ClientCacheHitRate, 2)
	cfg.CacheHitRateJitter = clampProxyPercent(cfg.CacheHitRateJitter, 8)
	cfg.CacheMaxHitRate = clampProxyMaxRate(cfg.CacheMaxHitRate)
	return cfg
}

func clampProxyPercent(value int, fallback int) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	if value == 0 && fallback > 0 {
		return 0
	}
	return value
}

func clampProxyMaxRate(value int) int {
	if value <= 0 {
		return 92
	}
	if value > 100 {
		return 100
	}
	return value
}
