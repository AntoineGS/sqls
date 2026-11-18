package mymeta

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func parsedDriverTLS(t *testing.T) *mysql.Config {
	t.Helper()
	dsn, err := DriverDSN(Config{Target: "db.example:3306/app", User: "app", Password: "secret"}, nil)
	if err != nil {
		t.Fatalf("DriverDSN(): %v", err)
	}
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("mysql.ParseDSN(%q): %v", dsn, err)
	}
	if parsed.TLS == nil {
		t.Fatalf("DSN %q has no TLS configuration", dsn)
	}
	return parsed
}

func TestDriverDSNTLSIsUnverifiedWithPlaintextFallback(t *testing.T) {
	parsed := parsedDriverTLS(t)
	if !parsed.TLS.InsecureSkipVerify {
		t.Fatal("TLS verifies certificates; the preferred mode must not")
	}
	if !parsed.AllowFallbackToPlaintext {
		t.Fatal("TLS does not fall back to plaintext for servers without TLS")
	}
}

// MySQL 5.7 servers built with OpenSSL 1.0.2 offer TLS 1.2 with only DHE or
// plain-RSA key exchange; Go supports neither by default.
func TestDriverDSNTLSHandshakesWithRSAKeyExchangeOnlyServer(t *testing.T) {
	got := handshake(t, parsedDriverTLS(t).TLS, &tls.Config{
		MaxVersion:   tls.VersionTLS12,
		CipherSuites: []uint16{tls.TLS_RSA_WITH_AES_128_GCM_SHA256},
	})
	if got != tls.TLS_RSA_WITH_AES_128_GCM_SHA256 {
		t.Fatalf("negotiated %s, want TLS_RSA_WITH_AES_128_GCM_SHA256", tls.CipherSuiteName(got))
	}
}

func TestDriverDSNTLSPrefersECDHE(t *testing.T) {
	got := handshake(t, parsedDriverTLS(t).TLS, &tls.Config{MaxVersion: tls.VersionTLS12})
	switch got {
	case tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384, tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256:
	default:
		t.Fatalf("negotiated %s, want an ECDHE suite", tls.CipherSuiteName(got))
	}
}

func handshake(t *testing.T, client, server *tls.Config) uint16 {
	t.Helper()
	server = server.Clone()
	server.Certificates = []tls.Certificate{selfSignedRSA(t)}
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	deadline := time.Now().Add(10 * time.Second)
	_ = clientConn.SetDeadline(deadline)
	_ = serverConn.SetDeadline(deadline)

	serverErr := make(chan error, 1)
	go func() { serverErr <- tls.Server(serverConn, server).Handshake() }()
	conn := tls.Client(clientConn, client.Clone())
	if err := conn.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
	return conn.ConnectionState().CipherSuite
}

func selfSignedRSA(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "MySQL_Server_Auto_Generated_Server_Certificate"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
