package stratum

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// APIMinerSettings implements MinerSettingsStore by calling the pool API
type APIMinerSettings struct {
	apiURL string
	client *http.Client
}

// NewAPIMinerSettings creates a new API-backed miner settings store
func NewAPIMinerSettings(apiURL string) *APIMinerSettings {
	return &APIMinerSettings{
		apiURL: apiURL,
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

// GetMinerSettings fetches miner settings from the API
func (a *APIMinerSettings) GetMinerSettings(minerID string) (*MinerSettings, error) {
	url := fmt.Sprintf("%s/api/v1/miners/%s/settings", a.apiURL, minerID)
	
	resp, err := a.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	var result struct {
		Exists     bool    `json:"exists"`
		Address    string  `json:"address"`
		SoloMining bool    `json:"solo_mining"`
		ManualDiff float64 `json:"manual_diff"`
		Vardiff    bool    `json:"vardiff"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	
	// Return settings even if not explicitly configured (defaults)
	return &MinerSettings{
		MinerID:    minerID,
		SoloMining: result.SoloMining,
		ManualDiff: result.ManualDiff,
	}, nil
}
