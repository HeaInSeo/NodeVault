package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DiscoveredConfig 는 브리지 스프린트 YAML에서 지원하는 최소 필드를 나타냄.
type DiscoveredConfig struct {
	Format     string `yaml:"format" json:"format"`
	Strictness struct {
		Mode       string `yaml:"mode" json:"mode"`
		Thresholds struct {
			MaxStartSkewMs     int64 `yaml:"maxStartSkewMs" json:"maxStartSkewMs"`
			MaxEndSkewMs       int64 `yaml:"maxEndSkewMs" json:"maxEndSkewMs"`
			MaxScrapeLatencyMs int64 `yaml:"maxScrapeLatencyMs" json:"maxScrapeLatencyMs"`
		} `yaml:"thresholds" json:"thresholds"`
	} `yaml:"strictness" json:"strictness"`
	Gating struct {
		GateOnLevel string `yaml:"gateOnLevel" json:"gateOnLevel"`
	} `yaml:"gating" json:"gating"`
	Cleanup struct {
		Enabled bool   `yaml:"enabled" json:"enabled"`
		Mode    string `yaml:"mode" json:"mode"`
	} `yaml:"cleanup" json:"cleanup"`
	Write struct {
		ArtifactsDir string `yaml:"artifactsDir" json:"artifactsDir"`
	} `yaml:"write" json:"write"`
	Reliability struct {
		RecordTiming bool `yaml:"recordTiming" json:"recordTiming"`
	} `yaml:"reliability" json:"reliability"`
}

// ConfigSource 는 구성이 로드된 위치를 나타냄.
type ConfigSource struct {
	Type     string // "injected" | "env" | "discovered"
	Path     string // File path if Type is "env" or "discovered"
	Disabled bool   // True if discovery was disabled via SLINT_DISABLE_DISCOVERY=1
}

// DiscoverConfig 는 kube-slint 구성을 검색하고 로드함.
// 우선순위 규칙:
//  1. 환경 변수: SLINT_CONFIG_PATH
//  2. 자동 탐색: .slint.yaml 또는 slint.config.yaml (디렉터리 상향 탐색)
func DiscoverConfig(startDir string) (*DiscoveredConfig, ConfigSource, error) {
	if isDiscoveryDisabled() {
		return nil, ConfigSource{Type: "injected", Disabled: true}, nil
	}

	cfg, src, hasEnv, err := discoverConfigFromEnv()
	if hasEnv {
		return cfg, src, err
	}

	startDir, err = normalizeStartDir(startDir)
	if err != nil {
		return nil, ConfigSource{}, err
	}

	return discoverConfigByWalking(startDir)
}

func discoverConfigFromEnv() (*DiscoveredConfig, ConfigSource, bool, error) {
	envPath := os.Getenv("SLINT_CONFIG_PATH")
	if envPath == "" {
		return nil, ConfigSource{}, false, nil
	}
	cfg, err := loadConfigFile(envPath)
	if err != nil {
		return nil, ConfigSource{}, true, fmt.Errorf("failed to load config from SLINT_CONFIG_PATH (%s): %w", envPath, err)
	}
	return cfg, ConfigSource{Type: "env", Path: envPath}, true, nil
}

func normalizeStartDir(startDir string) (string, error) {
	if startDir != "" {
		return startDir, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current working directory: %w", err)
	}
	return dir, nil
}

func discoverConfigByWalking(startDir string) (*DiscoveredConfig, ConfigSource, error) {
	targetFiles := []string{".slint.yaml", "slint.config.yaml"}
	currentDir := startDir

	// 루트 디렉터리에 도달할 때까지 상위 디렉터리 탐색
	for {
		for _, target := range targetFiles {
			path := filepath.Join(currentDir, target)
			if fileExists(path) {
				cfg, err := loadConfigFile(path)
				if err != nil {
					return nil, ConfigSource{}, fmt.Errorf("failed to load discovered config from %s: %w", path, err)
				}
				return cfg, ConfigSource{Type: "discovered", Path: path}, nil
			}
		}

		parentDir := filepath.Dir(currentDir)
		if parentDir == currentDir {
			// 루트 도달
			break
		}
		currentDir = parentDir
	}

	// 찾지 못함, 주입/기본값으로 폴백
	return nil, ConfigSource{Type: "injected"}, nil
}

func isDiscoveryDisabled() bool {
	val := os.Getenv("SLINT_DISABLE_DISCOVERY")
	return val == "1" || strings.ToLower(val) == "true"
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

func loadConfigFile(path string) (*DiscoveredConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg DiscoveredConfig

	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".json" {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return nil, err
		}
	} else {
		// default to yaml
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return nil, err
		}
	}

	return &cfg, nil
}
