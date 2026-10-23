package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// SubscriptionPolicy is separate from delivery state: an older live consumer
// rewriting State must not erase the identity's explicit opt-outs.
type SubscriptionPolicy struct {
	V     int                         `json:"v"`
	Trims map[string]SubscriptionTrim `json:"trims"`
}

type SubscriptionTrim struct {
	Dropped    []string `json:"dropped"`
	Reason     string   `json:"reason"`
	RecordedAt string   `json:"recorded_at"`
}

func (p Paths) SubscriptionPolicyFile(name string) string {
	return filepath.Join(p.Base, "state", name+".subscription-trims.json")
}

func LoadSubscriptionPolicy(p Paths, name string) (SubscriptionPolicy, error) {
	path := p.SubscriptionPolicyFile(name)
	if err := checkSecretFile(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return SubscriptionPolicy{V: 1, Trims: map[string]SubscriptionTrim{}}, nil
		}
		return SubscriptionPolicy{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return SubscriptionPolicy{}, err
	}
	var policy SubscriptionPolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		return policy, fmt.Errorf("invalid subscription policy JSON: %w", err)
	}
	if policy.V != 1 {
		return policy, fmt.Errorf("unsupported subscription policy version %d", policy.V)
	}
	if policy.Trims == nil {
		policy.Trims = map[string]SubscriptionTrim{}
	}
	return policy, nil
}

// Callers serialize policy changes with the per-identity subscription-policy
// runtime lock. Readers see the previous or complete new policy, mode 0600.
func SaveSubscriptionPolicy(p Paths, name string, policy SubscriptionPolicy) error {
	path := p.SubscriptionPolicyFile(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return err
	}
	return writeSecretAtomic(path, name, append(data, '\n'))
}
