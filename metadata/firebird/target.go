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

// NormalizeTarget derives a metadata.Key from a raw Firebird attachment
// string. It recognizes and strips a case-insensitive "fb://" prefix, then
// reapplies the canonical lower-case prefix; every other byte of the
// attachment is preserved exactly.
func NormalizeTarget(raw string) (metadata.Key, error) {
	rest := stripScheme(raw)
	if strings.TrimSpace(rest) == "" {
		return metadata.Key{}, fmt.Errorf("%w: blank Firebird target", metadata.ErrInvalidTarget)
	}
	return metadata.Key{Engine: engine, URL: scheme + rest}, nil
}

// Attachment splits a classic Firebird attachment string into host, port and
// database path. Local paths use localhost and the default port. Remote
// attachments have the form host[:port]:path, with the port written as
// host/port:path.
func Attachment(target string) (string, int, string, error) {
	rest := strings.TrimSpace(stripScheme(target))
	if rest == "" {
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
// set here so consumers share the same driver policy; extra may add or replace
// query options.
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
		q[key] = values
	}
	// The driver removes one leading slash for aliases and Windows paths, but
	// keeps the root slash on multi-segment Unix paths. Add the URL path's
	// leading slash only to relative and Windows path forms.
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
	return u.String(), nil
}
