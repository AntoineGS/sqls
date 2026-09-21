package database

import (
	"errors"
	"fmt"
	"os"

	"github.com/sqls-server/sqls/dialect"
	"golang.org/x/crypto/ssh"
)

type Proto string

const (
	ProtoTCP  Proto = "tcp"
	ProtoUDP  Proto = "udp"
	ProtoUnix Proto = "unix"
	ProtoHTTP Proto = "http"
)

type DBConfig struct {
	Alias          string                 `json:"alias" yaml:"alias"`
	Driver         dialect.DatabaseDriver `json:"driver" yaml:"driver"`
	DataSourceName string                 `json:"dataSourceName" yaml:"dataSourceName"`
	Proto          Proto                  `json:"proto" yaml:"proto"`
	User           string                 `json:"user" yaml:"user"`
	Passwd         string                 `json:"passwd" yaml:"passwd"`
	Host           string                 `json:"host" yaml:"host"`
	Port           int                    `json:"port" yaml:"port"`
	Path           string                 `json:"path" yaml:"path"`
	DBName         string                 `json:"dbName" yaml:"dbName"`
	Params         map[string]string      `json:"params" yaml:"params"`
	SSHCfg         *SSHConfig             `json:"sshConfig" yaml:"sshConfig"`
	// Dialect selects the server-side SQL dialect. Only the interbase driver
	// supports it: 0 auto-detects from the database, 1 and 3 pin a dialect.
	Dialect   int              `json:"dialect" yaml:"dialect"`
	InterBase *InterBaseConfig `json:"interbase" yaml:"interbase"`
}

func (c *DBConfig) Validate() error {
	if c == nil {
		return errors.New("connection config is nil")
	}
	if c.Driver == "" {
		return errors.New("required: connections[].driver")
	}
	if c.Dialect != 0 && c.Driver != dialect.DatabaseDriverInterBase {
		return errors.New("invalid: connections[].dialect is only supported by the interbase driver")
	}
	if c.InterBase != nil && c.Driver != dialect.DatabaseDriverInterBase {
		return errors.New("invalid: connections[].interbase is only supported by the interbase driver")
	}

	switch c.Driver {
	case
		dialect.DatabaseDriverMySQL,
		dialect.DatabaseDriverMySQL8,
		dialect.DatabaseDriverMySQL57,
		dialect.DatabaseDriverMySQL56,
		dialect.DatabaseDriverPostgreSQL,
		dialect.DatabaseDriverVertica:
		if c.DataSourceName == "" && c.Proto == "" {
			return errors.New("required: connections[].dataSourceName or connections[].proto")
		}

		if c.DataSourceName == "" && c.Proto != "" {
			if c.User == "" {
				return errors.New("required: connections[].user")
			}
			switch c.Proto {
			case ProtoTCP, ProtoUDP, ProtoHTTP:
				if c.Host == "" {
					return errors.New("required: connections[].host")
				}
			case ProtoUnix:
				if c.Path == "" {
					return errors.New("required: connections[].path")
				}
			default:
				return errors.New("invalid: connections[].proto")
			}
			if c.SSHCfg != nil {
				return c.SSHCfg.Validate()
			}
		}
	case dialect.DatabaseDriverSQLite3:
	case dialect.DatabaseDriverH2:
		if c.DataSourceName == "" {
			return errors.New("required: connections[].dataSourceName")
		}
	case dialect.DatabaseDriverMssql:
		if c.DataSourceName == "" && c.Proto == "" {
			return errors.New("required: connections[].dataSourceName or connections[].proto")
		}
		if c.DataSourceName == "" && c.Proto != "" {
			if c.User == "" {
				return errors.New("required: connections[].user")
			}
			switch c.Proto {
			case ProtoTCP:
				if c.Host == "" {
					return errors.New("required: connections[].host")
				}
			case ProtoUDP, ProtoUnix, ProtoHTTP:
			default:
				return errors.New("invalid: connections[].proto")
			}
		}
	case dialect.DatabaseDriverOracle:
		if c.DataSourceName == "" && c.Proto == "" {
			return errors.New("required: connections[].dataSourceName or connections[].proto")
		}
		if c.DataSourceName == "" {
			if c.User == "" {
				return errors.New("required: connections[].user")
			}
			if c.Passwd == "" {
				return errors.New("required: connections[].Passwd")
			}
			if c.Host == "" {
				return errors.New("required: connections[].Host")
			}
			if c.Port <= 0 {
				return errors.New("required: connections[].Port")
			}
			if c.DBName == "" {
				return errors.New("required: connections[].DBName")
			}
		}
	case dialect.DatabaseDriverClickhouse:
		if c.DataSourceName == "" && c.Proto == "" {
			return errors.New("required: connections[].dataSourceName or connections[].proto")
		}

		if c.DataSourceName == "" && c.Proto != "" {
			if c.User == "" {
				return errors.New("required: connections[].user")
			}
			switch c.Proto {
			case ProtoTCP, ProtoHTTP:
				if c.Host == "" {
					return errors.New("required: connections[].host")
				}
			case ProtoUDP, ProtoUnix:
			default:
				return errors.New("invalid: connections[].proto")
			}
			if c.SSHCfg != nil {
				return c.SSHCfg.Validate()
			}
		}
	case dialect.DatabaseDriverInterBase:
		if c.User == "" {
			return errors.New("required: connections[].user")
		}
		if c.SSHCfg != nil {
			return errors.New("InterBase connections via SSH are not supported")
		}
		switch c.Dialect {
		case 0, 1, 3:
		default:
			return errors.New("invalid: connections[].dialect must be 0 (auto), 1, or 3")
		}
		if _, err := interBaseAttachment(c); err != nil {
			return err
		}
		if _, err := interBaseCharset(c); err != nil {
			return err
		}
		if _, err := interBaseRole(c); err != nil {
			return err
		}
		if _, err := interBaseConnectTimeout(c); err != nil {
			return err
		}
		if _, err := interBaseTLS(c); err != nil {
			return err
		}

	default:
		return errors.New("invalid: connections[].driver")
	}
	return nil
}

type SSHConfig struct {
	Host       string `json:"host" yaml:"host"`
	Port       int    `json:"port" yaml:"port"`
	User       string `json:"user" yaml:"user"`
	PassPhrase string `json:"passPhrase" yaml:"passPhrase"`
	PrivateKey string `json:"privateKey" yaml:"privateKey"`
}

func (s *SSHConfig) Validate() error {
	if s.Host == "" {
		return errors.New("required: connections[]sshConfig.host")
	}
	if s.User == "" {
		return errors.New("required: connections[].sshConfig.user")
	}
	if s.PrivateKey == "" {
		return errors.New("required: connections[].sshConfig.privateKey")
	}
	return nil
}

func (s *SSHConfig) Endpoint() string {
	port := s.Port
	if port == 0 {
		port = 22
	}
	return fmt.Sprintf("%s:%d", s.Host, port)
}

func (s *SSHConfig) ClientConfig() (*ssh.ClientConfig, error) {
	buffer, err := os.ReadFile(s.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("cannot read SSH private key file, PrivateKey=%s, %w", s.PrivateKey, err)
	}

	var key ssh.Signer
	if s.PassPhrase != "" {
		key, err = ssh.ParsePrivateKeyWithPassphrase(buffer, []byte(s.PassPhrase))
		if err != nil {
			return nil, fmt.Errorf("cannot parse SSH private key file with passphrase, PrivateKey=%s, %w", s.PrivateKey, err)
		}
	} else {
		key, err = ssh.ParsePrivateKey(buffer)
		if err != nil {
			return nil, fmt.Errorf("cannot parse SSH private key file, PrivateKey=%s, %w", s.PrivateKey, err)
		}
	}

	sshConfig := &ssh.ClientConfig{
		User:            s.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	return sshConfig, nil
}

// InterBaseConfig holds settings that only the InterBase driver understands.
// The nested block keeps InterBase-only keys out of the shared DBConfig surface,
// matching the existing sshConfig precedent.
type InterBaseConfig struct {
	Role string `json:"role" yaml:"role"`
	// ConnectTimeout bounds the native attachment handshake. It is a Go duration
	// string, for example "10s"; empty leaves the InterBase client default. It is
	// a string because YAML has no duration type and a bare integer is ambiguous.
	ConnectTimeout string              `json:"connectTimeout" yaml:"connectTimeout"`
	TLS            *InterBaseTLSConfig `json:"tls" yaml:"tls"`
}

// InterBaseTLSConfig holds the InterBase native client TLS attachment options.
// Enabling TLS encrypts the connection; it is not proof of server identity. See
// the TLS note in README.md before relying on it.
type InterBaseTLSConfig struct {
	Enabled              bool   `json:"enabled" yaml:"enabled"`
	ServerPublicFile     string `json:"serverPublicFile" yaml:"serverPublicFile"`
	ServerPublicPath     string `json:"serverPublicPath" yaml:"serverPublicPath"`
	ClientCertFile       string `json:"clientCertFile" yaml:"clientCertFile"`
	ClientPassPhrase     string `json:"clientPassPhrase" yaml:"clientPassPhrase"`
	ClientPassPhraseFile string `json:"clientPassPhraseFile" yaml:"clientPassPhraseFile"`
}
