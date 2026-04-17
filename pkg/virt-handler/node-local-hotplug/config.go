package nodelocalhotplug

import (
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"
	"kubevirt.io/client-go/log"
)

const (
	HotplugConfigFilePath = "HOTPLUG_CONFIG_FILE_PATH"
	ConfigPath            = "/etc/hotplug-config/config.yaml"
)

type Config struct {
	PersistentRegional PersistentRegional `yaml:"persistentRegional"`
	Ephemeral          Ephemeral          `yaml:"ephemeral"`
	Encryption         Encryption         `yaml:"encryption,omitempty"`
}

type PersistentRegional struct {
	CSISocketPath string `yaml:"csiSocketPath"`
}

type Ephemeral struct {
	CSISocketPath string `yaml:"csiSocketPath"`
	StagingPath   string `yaml:"stagingPath"`
}

type Encryption struct {
	ESPEnabled      bool              `yaml:"espEnabled,omitempty"`
	EncryptionValue string            `yaml:"encryptionValue,omitempty"`
	SecretReq       map[string]string `yaml:"secretReq,omitempty"`
}

func (c *Config) Validate() error {
	if c.PersistentRegional.CSISocketPath == "" {
		return fmt.Errorf("persistentRegional.csiSocketPath is required")
	}
	if c.Ephemeral.CSISocketPath == "" {
		return fmt.Errorf("ephemeral.csiSocketPath is required")
	}
	if c.Ephemeral.StagingPath == "" {
		return fmt.Errorf("ephemeral.stagingPath is required")
	}
	return nil
}

func LoadConfig() (*Config, error) {
	configPath := os.Getenv(HotplugConfigFilePath)
	if configPath == "" {
		configPath = ConfigPath
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("load hotplug config from %s: %w", configPath, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid hotplug config from %s: %w", configPath, err)
	}
	log.Log.V(3).Infof("Loaded hotplug config from %s", configPath)
	return cfg, nil
}

func loadConfig(configPath string) (*Config, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	config := &Config{}
	if len(data) != 0 {
		if err := yaml.Unmarshal(data, config); err != nil {
			return nil, err
		}
	}
	return config, nil
}
