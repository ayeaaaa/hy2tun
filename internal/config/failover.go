package config

import (
	"fmt"
	"time"
)

const (
	DefaultFailoverConnectTimeout    = 10 * time.Second
	DefaultFailoverHeartbeatInterval = 10 * time.Second
	DefaultFailoverHeartbeatTimeout  = 30 * time.Second
	DefaultFailoverProbeInterval     = 30 * time.Second
	DefaultFailoverMinTCPHold        = 60 * time.Second
	DefaultFailoverRecoverySuccesses = 3
)

// FailoverConfig tunes the hy2+tcp fallback and recovery policy for one
// server. Zero fields inherit the defaults above. It is optional and omitted
// from serialized configs when unset.
type FailoverConfig struct {
	ConnectTimeout    Duration `yaml:"connect_timeout,omitempty" json:"connect_timeout,omitempty"`
	HeartbeatInterval Duration `yaml:"heartbeat_interval,omitempty" json:"heartbeat_interval,omitempty"`
	HeartbeatTimeout  Duration `yaml:"heartbeat_timeout,omitempty" json:"heartbeat_timeout,omitempty"`
	ProbeInterval     Duration `yaml:"probe_interval,omitempty" json:"probe_interval,omitempty"`
	MinTCPHold        Duration `yaml:"min_tcp_hold,omitempty" json:"min_tcp_hold,omitempty"`
	RecoverySuccesses int      `yaml:"recovery_successes,omitempty" json:"recovery_successes,omitempty"`
}

// Effective fills zero fields with the conservative defaults.
func (f FailoverConfig) Effective() FailoverConfig {
	if f.ConnectTimeout == 0 {
		f.ConnectTimeout = Duration(DefaultFailoverConnectTimeout)
	}
	if f.HeartbeatInterval == 0 {
		f.HeartbeatInterval = Duration(DefaultFailoverHeartbeatInterval)
	}
	if f.HeartbeatTimeout == 0 {
		f.HeartbeatTimeout = Duration(DefaultFailoverHeartbeatTimeout)
	}
	if f.ProbeInterval == 0 {
		f.ProbeInterval = Duration(DefaultFailoverProbeInterval)
	}
	if f.MinTCPHold == 0 {
		f.MinTCPHold = Duration(DefaultFailoverMinTCPHold)
	}
	if f.RecoverySuccesses == 0 {
		f.RecoverySuccesses = DefaultFailoverRecoverySuccesses
	}
	return f
}

// Validate checks explicit values and the effective heartbeat ordering.
func (f FailoverConfig) Validate(prefix string) error {
	check := func(name string, value Duration, minimum time.Duration) error {
		if value < 0 {
			return fmt.Errorf("%s.%s must not be negative", prefix, name)
		}
		if value > 0 && time.Duration(value) < minimum {
			return fmt.Errorf("%s.%s must be at least %s", prefix, name, minimum)
		}
		return nil
	}
	if err := check("connect_timeout", f.ConnectTimeout, time.Second); err != nil {
		return err
	}
	if err := check("heartbeat_interval", f.HeartbeatInterval, time.Second); err != nil {
		return err
	}
	if err := check("heartbeat_timeout", f.HeartbeatTimeout, 2*time.Second); err != nil {
		return err
	}
	if err := check("probe_interval", f.ProbeInterval, time.Second); err != nil {
		return err
	}
	if err := check("min_tcp_hold", f.MinTCPHold, time.Second); err != nil {
		return err
	}
	if f.RecoverySuccesses < 0 {
		return fmt.Errorf("%s.recovery_successes must not be negative", prefix)
	}
	if f.RecoverySuccesses > 10 {
		return fmt.Errorf("%s.recovery_successes must not exceed 10", prefix)
	}
	effective := f.Effective()
	if effective.HeartbeatTimeout <= effective.HeartbeatInterval {
		return fmt.Errorf("%s.heartbeat_timeout must be greater than heartbeat_interval", prefix)
	}
	return nil
}

func cloneFailover(value *FailoverConfig) *FailoverConfig {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}
