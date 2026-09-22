package database

import (
	"errors"
	"strings"

	"github.com/sqls-server/sqls/dialect"
)

// ConnectionIdentity is the credential-free identity of an open connection:
// enough to tell whether "the same connection" from a client's point of view
// still points at the same server, database and role. It never includes
// DBConfig.Passwd or any InterBaseTLSConfig secret — the fields are named
// individually rather than serializing DBConfig wholesale — and it strips a
// "user:pass@" prefix a caller may have embedded in DataSourceName.
type ConnectionIdentity struct {
	Driver            dialect.DatabaseDriver `json:"driver"`
	Alias             string                 `json:"alias"`
	Attachment        string                 `json:"attachment"`
	Host              string                 `json:"host"`
	Port              int                    `json:"port"`
	Path              string                 `json:"path"`
	DBName            string                 `json:"dbName"`
	User              string                 `json:"user"`
	Role              string                 `json:"role"`
	Charset           string                 `json:"charset"`
	EffectiveDatabase string                 `json:"effectiveDatabase"`
}

// NewInterBaseConnectionIdentity derives cfg's identity tuple for an open
// InterBase-variant connection. conn supplies EffectiveDatabase, the database
// name the attachment actually resolved to, which can differ from cfg.DBName
// when the attachment names a path rather than an alias.
//
// Both arguments are required: a nil config or connection has no identity of
// its own, and returning a shared zero-value ConnectionIdentity for either
// would make two genuinely different "unset" callers compare equal.
func NewInterBaseConnectionIdentity(cfg *DBConfig, conn *DBConnection) (ConnectionIdentity, error) {
	if cfg == nil || conn == nil {
		return ConnectionIdentity{}, errors.New("database: connection config and connection are required for parameter identity")
	}
	attachment, err := interBaseAttachment(cfg)
	if err != nil {
		return ConnectionIdentity{}, err
	}
	charset, err := interBaseCharset(cfg)
	if err != nil {
		return ConnectionIdentity{}, err
	}
	role, err := interBaseRole(cfg)
	if err != nil {
		return ConnectionIdentity{}, err
	}
	return ConnectionIdentity{
		Driver:            cfg.Driver,
		Alias:             cfg.Alias,
		Attachment:        redactDSNCredentials(attachment),
		Host:              cfg.Host,
		Port:              cfg.Port,
		Path:              cfg.Path,
		DBName:            cfg.DBName,
		User:              cfg.User,
		Role:              role,
		Charset:           charset,
		EffectiveDatabase: conn.DatabaseName,
	}, nil
}

// redactDSNCredentials drops a "user[:password]@" prefix from a raw
// DataSourceName before it enters the identity tuple, so a credential a
// caller embedded in a hand-written DSN is excluded exactly like
// DBConfig.Passwd already is.
func redactDSNCredentials(attachment string) string {
	if idx := strings.LastIndexByte(attachment, '@'); idx >= 0 {
		return attachment[idx+1:]
	}
	return attachment
}
