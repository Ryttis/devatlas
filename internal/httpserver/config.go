package httpserver

import (
	"fmt"
	"net"
	"time"
)

// Default listener settings. The default port was chosen after checking the
// development workstation for occupied ports; change it with the serve flags
// if it conflicts on another machine.
const (
	DefaultHost = "127.0.0.1"
	DefaultPort = 8642

	DefaultShutdownTimeout = 10 * time.Second
)

// Config holds the HTTP server settings.
type Config struct {
	// Host is the IP address to bind. It must be a loopback address such as
	// 127.0.0.1 or ::1; DevAtlas never listens on other interfaces.
	Host string
	// Port is the TCP port to bind. 0 asks the operating system for a free
	// port; the chosen address is logged after the listener is open.
	Port int
	// ShutdownTimeout bounds how long in-flight requests may run after a
	// shutdown signal before the server closes forcibly.
	ShutdownTimeout time.Duration
}

// DefaultConfig returns the default loopback configuration.
func DefaultConfig() Config {
	return Config{
		Host:            DefaultHost,
		Port:            DefaultPort,
		ShutdownTimeout: DefaultShutdownTimeout,
	}
}

// Validate checks the configuration without opening any socket.
func (c Config) Validate() error {
	if err := validateLoopbackHost(c.Host); err != nil {
		return err
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("port %d is outside the valid range 0-65535", c.Port)
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("shutdown timeout must be positive, got %s", c.ShutdownTimeout)
	}
	return nil
}

// Addr returns the host:port string the server binds to.
func (c Config) Addr() string {
	return net.JoinHostPort(c.Host, fmt.Sprint(c.Port))
}

// validateLoopbackHost accepts only literal loopback IP addresses. Host names
// (including "localhost") are rejected on purpose: a resolver could map them
// to a non-loopback address, and the whole point of this check is that the
// server is never reachable from other machines.
func validateLoopbackHost(host string) error {
	if host == "" {
		return fmt.Errorf("bind host is empty; use a loopback IP such as %s", DefaultHost)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("bind host %q is not an IP address; DevAtlas binds only to a loopback IP such as %s or ::1", host, DefaultHost)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("bind host %s is not a loopback address; DevAtlas binds only to loopback (for example %s or ::1)", host, DefaultHost)
	}
	return nil
}
