// Package fbmeta normalizes Firebird attachment strings into shared metadata
// registry keys and builds driver DSNs for Firebird 2.5 metadata attachments.
package fbmeta

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/sqls-server/sqls/metadata"
)

const (
	engine      = "firebird"
	scheme      = "fb://"
	defaultPort = 3050
)

func stripScheme(raw string) string {
	if len(raw) >= len(scheme) && strings.EqualFold(raw[:len(scheme)], scheme) {
		return raw[len(scheme):]
	}
	return raw
}

func rejectSurroundingWhitespace(raw string) error {
	if raw != strings.TrimSpace(raw) {
		return fmt.Errorf("%w: Firebird target has surrounding whitespace", metadata.ErrInvalidTarget)
	}
	return nil
}

// NormalizeTarget derives a metadata.Key from a raw Firebird attachment
// string. It recognizes and strips a case-insensitive "fb://" prefix, then
// reapplies the canonical lower-case prefix. Like Attachment, it rejects
// surrounding whitespace so the registry key and connection target cannot
// identify different attachments.
func NormalizeTarget(raw string) (metadata.Key, error) {
	if err := rejectSurroundingWhitespace(raw); err != nil {
		return metadata.Key{}, err
	}
	rest := stripScheme(raw)
	if strings.TrimSpace(rest) == "" {
		return metadata.Key{}, fmt.Errorf("%w: blank Firebird target", metadata.ErrInvalidTarget)
	}
	return metadata.Key{Engine: engine, URL: scheme + rest}, nil
}

// Attachment splits a classic Firebird attachment string into host, port and
// database path. Local paths use localhost and the default port. Remote
// attachments have the form host[:port]:path, with the port written as
// host/port:path. Surrounding whitespace is rejected, matching
// NormalizeTarget's key semantics.
func Attachment(target string) (string, int, string, error) {
	if err := rejectSurroundingWhitespace(target); err != nil {
		return "", 0, "", err
	}
	rest := stripScheme(target)
	if strings.TrimSpace(rest) == "" {
		return "", 0, "", fmt.Errorf("%w: blank Firebird target", metadata.ErrInvalidTarget)
	}
	colon := strings.IndexByte(rest, ':')
	// No colon, a leading "/" or a one-letter prefix (drive letter) is local.
	if colon < 0 || strings.HasPrefix(rest, "/") || colon == 1 {
		return "localhost", defaultPort, rest, nil
	}
	server, path := rest[:colon], rest[colon+1:]
	if path == "" {
		return "", 0, "", fmt.Errorf("%w: Firebird target has no database path", metadata.ErrInvalidTarget)
	}
	host, port := server, defaultPort
	if slash := strings.IndexByte(server, '/'); slash >= 0 {
		host = server[:slash]
		p, err := strconv.Atoi(server[slash+1:])
		if err != nil || p < 1 || p > 65535 {
			return "", 0, "", fmt.Errorf("%w: invalid Firebird port %q", metadata.ErrInvalidTarget, server[slash+1:])
		}
		port = p
	}
	if host == "" {
		return "", 0, "", fmt.Errorf("%w: Firebird target has no host", metadata.ErrInvalidTarget)
	}
	return host, port, path, nil
}

// DriverDSN builds a nakagami/firebirdsql DSN from a classic Firebird
// attachment string. Required Firebird 2.5 metadata attachment options are
// set here so consumers share the same driver policy. extra may add driver
// options, but cannot replace values controlled by this package.
func DriverDSN(target, user, password, role string, sqlDialect int, extra url.Values) (string, error) {
	host, port, path, err := Attachment(target)
	if err != nil {
		return "", err
	}
	if sqlDialect != 1 {
		sqlDialect = 3
	}
	q := url.Values{
		"charset":          {"NONE"},
		"explicit_tx_only": {"true"},
		"sql_type_names":   {"true"},
		"auth_plugin_name": {"Legacy_Auth"},
		"auth_plugin_list": {"Legacy_Auth"},
		"wire_crypt":       {"false"},
		"sql_dialect":      {strconv.Itoa(sqlDialect)},
	}
	if role != "" {
		q.Set("role", role)
	}
	for key, values := range extra {
		if isReservedDriverOption(key) {
			return "", fmt.Errorf("fbmeta: extra driver option %q is reserved", key)
		}
		q[key] = values
	}
	// The driver requires a leading URL slash for relative names and Windows
	// paths. Absolute Unix paths already have one; adding another would change
	// the database name seen by the driver.
	dsnPath := path
	if !strings.HasPrefix(dsnPath, "/") {
		dsnPath = "/" + dsnPath
	}
	u := url.URL{
		Scheme:   "firebird",
		User:     url.UserPassword(user, password),
		Host:     net.JoinHostPort(host, strconv.Itoa(port)),
		Path:     dsnPath,
		RawQuery: q.Encode(),
	}
	dsn := u.String()
	dbName, err := driverDatabaseName(dsn)
	if err != nil {
		return "", fmt.Errorf("%w: invalid Firebird database path %q: %v", metadata.ErrInvalidTarget, path, err)
	}
	if dbName != path {
		return "", fmt.Errorf("%w: Firebird database path %q is not preserved by the driver DSN parser", metadata.ErrInvalidTarget, path)
	}
	return dsn, nil
}

func isReservedDriverOption(key string) bool {
	switch strings.ToLower(key) {
	case "auth_plugin_name", "auth_plugin_list", "charset", "explicit_tx_only", "sql_type_names", "wire_crypt", "sql_dialect", "role":
		return true
	default:
		return false
	}
}

// driverDatabaseName mirrors the URL path handling in
// firebirdsql/dsn.go:73-81. The driver does not export parseDSN, so checking
// this transformation keeps paths from silently attaching to another name.
func driverDatabaseName(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	dbName := u.Path
	if len(dbName) > 1 && !strings.ContainsRune(dbName[1:], '/') {
		dbName = dbName[1:]
	}
	if len(dbName) > 2 && strings.ContainsRune(dbName[2:], ':') {
		dbName = dbName[1:]
	}
	if strings.TrimLeft(dbName, "/") == "" {
		return "", fmt.Errorf("driver DSN has no database name")
	}
	return dbName, nil
}
