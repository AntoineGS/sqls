// Package mymeta defines the shared metadata identity and MySQL driver DSN
// contract for MySQL connections.
package mymeta

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/sqls-server/sqls/metadata"
)

const defaultPort = 3306

// Config contains the target and login settings for one MySQL connection.
type Config struct {
	Target         string
	User, Password string
	ConnectTimeout time.Duration
}

// Target is the parsed form of a MySQL catalog target. Port is always set,
// defaulting to 3306 when the target omits it.
type Target struct {
	User, Host string
	Port       int
	Schema     string
}

// ParseTarget parses a target of the form [user@]host[:port]/schema.
func ParseTarget(raw string) (Target, error) {
	if raw == "" || raw != strings.TrimSpace(raw) {
		return Target{}, invalidTarget("blank or whitespace-padded target")
	}

	slash := strings.IndexByte(raw, '/')
	if slash < 0 || slash != strings.LastIndexByte(raw, '/') {
		return Target{}, invalidTarget("target must contain one schema separator")
	}
	authority, schema := raw[:slash], raw[slash+1:]
	if strings.TrimSpace(schema) == "" {
		return Target{}, invalidTarget("target has no schema")
	}

	user := ""
	address := authority
	if at := strings.IndexByte(authority, '@'); at >= 0 {
		if at != strings.LastIndexByte(authority, '@') {
			return Target{}, invalidTarget("target has an invalid authority")
		}
		user = authority[:at]
		address = authority[at+1:]
		if user == "" || strings.ContainsRune(user, ':') || user != strings.TrimSpace(user) || strings.ContainsAny(user, " \t\r\n") {
			return Target{}, invalidTarget("target has an invalid authority user")
		}
	}

	host, port, err := parseAddress(address)
	if err != nil {
		return Target{}, err
	}
	return Target{User: user, Host: host, Port: port, Schema: schema}, nil
}

// Addr returns the host and port in the form expected by the MySQL TCP driver.
func (t Target) Addr() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
}

// DriverDSN builds a go-sql-driver/mysql v1.10.0 DSN. Extra query parameters
// are appended after the defaults so they take precedence when parsed by the
// driver.
func DriverDSN(cfg Config, extra url.Values) (string, error) {
	target, err := ParseTarget(cfg.Target)
	if err != nil {
		return "", err
	}

	timeout := cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	user := effectiveUser(cfg.User, target.User)
	if strings.ContainsRune(user, ':') {
		return "", fmt.Errorf("mymeta: MySQL username cannot contain ':'")
	}

	driverConfig := mysql.NewConfig()
	driverConfig.User = user
	driverConfig.Passwd = cfg.Password
	driverConfig.Net = "tcp"
	driverConfig.Addr = target.Addr()
	driverConfig.DBName = target.Schema
	driverConfig.TLSConfig = "preferred"
	driverConfig.Timeout = timeout
	driverConfig.ClientFoundRows = true
	driverConfig.Params = map[string]string{"charset": "utf8mb4"}

	dsn := driverConfig.FormatDSN()
	if len(extra) > 0 {
		dsn += "&" + extra.Encode()
	}
	return dsn, nil
}

// KeyFor returns the canonical shared metadata identity for cfg's target.
func KeyFor(cfg Config) (metadata.Key, error) {
	target, err := ParseTarget(cfg.Target)
	if err != nil {
		return metadata.Key{}, err
	}
	address := net.JoinHostPort(strings.ToLower(target.Host), strconv.Itoa(target.Port))
	return metadata.Key{Engine: "mysql", URL: "mysql://" + address + "/" + target.Schema}, nil
}

// ScopeFor returns the authenticated metadata visibility for cfg. Passwords
// are intentionally excluded from metadata scopes.
func ScopeFor(cfg Config) (metadata.Scope, error) {
	target, err := ParseTarget(cfg.Target)
	if err != nil {
		return metadata.Scope{}, err
	}
	principal := effectiveUser(cfg.User, target.User)
	if principal == "" {
		return metadata.Scope{}, metadata.ErrScopeRequired
	}
	return metadata.Scope{Principal: principal, TextPolicy: "utf8mb4"}, nil
}

func effectiveUser(configUser, targetUser string) string {
	if user := strings.TrimSpace(configUser); user != "" {
		return user
	}
	return strings.TrimSpace(targetUser)
}

func parseAddress(address string) (string, int, error) {
	port := defaultPort
	host := address
	portText := ""

	if strings.HasPrefix(address, "[") {
		end := strings.IndexByte(address, ']')
		if end < 0 {
			return "", 0, invalidTarget("target has an invalid host")
		}
		host = address[1:end]
		if net.ParseIP(host) == nil {
			return "", 0, invalidTarget("target has an invalid host")
		}
		remainder := address[end+1:]
		if remainder != "" {
			if !strings.HasPrefix(remainder, ":") {
				return "", 0, invalidTarget("target has an invalid port")
			}
			portText = remainder[1:]
		}
	} else {
		if strings.ContainsAny(address, "[]") || strings.Count(address, ":") > 1 {
			return "", 0, invalidTarget("target has an invalid host")
		}
		if colon := strings.LastIndexByte(address, ':'); colon >= 0 {
			host = address[:colon]
			portText = address[colon+1:]
		}
	}

	if host == "" || host != strings.TrimSpace(host) || strings.ContainsAny(host, " \t\r\n/@") {
		return "", 0, invalidTarget("target has no valid host")
	}
	if portText != "" {
		for _, digit := range portText {
			if digit < '0' || digit > '9' {
				return "", 0, invalidTarget("target has an invalid port")
			}
		}
		parsedPort, err := strconv.Atoi(portText)
		if err != nil || parsedPort < 1 || parsedPort > 65535 {
			return "", 0, invalidTarget("target has an invalid port")
		}
		port = parsedPort
	} else if strings.HasSuffix(address, ":") {
		return "", 0, invalidTarget("target has an invalid port")
	}
	return host, port, nil
}

func invalidTarget(reason string) error {
	return fmt.Errorf("%w: %s", metadata.ErrInvalidTarget, reason)
}
