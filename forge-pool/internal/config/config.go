package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// PoolConfig holds all configurable pool settings
type PoolConfig struct {
	StratumPort int       `json:"stratum_port"`
	PoolWallet  string    `json:"pool_wallet"`
	PoolName    string    `json:"pool_name"`
	PoolFee     float64   `json:"pool_fee"`
	SoloFee     float64   `json:"solo_fee"`
	MinPayout   float64   `json:"min_payout"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// GetDefaults returns default configuration
func GetDefaults() *PoolConfig {
	return &PoolConfig{
		StratumPort: 3333,
		PoolWallet:  "",
		PoolName:    "My Forge Pool",
		PoolFee:     1.0,
		SoloFee:     0.5,
		MinPayout:   5.0,
		UpdatedAt:   time.Now(),
	}
}

// LoadConfig loads configuration from the data directory
func LoadConfig(dataDir string) (*PoolConfig, error) {
	configPath := filepath.Join(dataDir, "config", "pool-config.json")

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return GetDefaults(), nil
		}
		return nil, err
	}

	var cfg PoolConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// SaveConfig saves configuration to the data directory
func SaveConfig(dataDir string, cfg *PoolConfig) error {
	configDir := filepath.Join(dataDir, "config")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return err
	}

	configPath := filepath.Join(configDir, "pool-config.json")

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(configPath, data, 0644)
}

// ValidateConfig validates the configuration values
func ValidateConfig(cfg *PoolConfig) error {
	if cfg.StratumPort < 1024 || cfg.StratumPort > 65535 {
		return errors.New("stratum port must be between 1024 and 65535")
	}

	if cfg.PoolFee < 0 || cfg.PoolFee > 10 {
		return errors.New("pool fee must be between 0 and 10 percent")
	}

	if cfg.SoloFee < 0 || cfg.SoloFee > 10 {
		return errors.New("solo fee must be between 0 and 10 percent")
	}

	if cfg.MinPayout < 0.1 || cfg.MinPayout > 1000 {
		return errors.New("minimum payout must be between 0.1 and 1000")
	}

	if cfg.PoolName == "" {
		cfg.PoolName = "My Forge Pool"
	}

	return nil
}
