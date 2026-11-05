package daemon

import (
	"crypto/ecdsa"
	"crypto/subtle"
	"net/http"
	"strings"

	"algod.top/internal/api"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"algod.top/internal/algod"
	"algod.top/internal/fsx"
)

// tlsConfig returns the listener's TLS config: a configured certificate,
// a persisted self-signed one, or nil for plain HTTP.
func (d *Daemon) tlsConfig() (*tls.Config, error) {
	cfg := d.Config()
	cert, key, mode := cfg.Web.TLSCert, cfg.Web.TLSKey, cfg.Web.TLS
	clientCA := ""
	if d.o.Agent {
		cert, key, clientCA = cfg.Agent.TLSCert, cfg.Agent.TLSKey, cfg.Agent.ClientCA
		if cert == "" {
			mode = "self-signed"
		}
	}
	var tc *tls.Config
	switch {
	case cert != "" && key != "":
		c, err := tls.LoadX509KeyPair(algod.ExpandHome(cert), algod.ExpandHome(key))
		if err != nil {
			return nil, fmt.Errorf("tls: %w", err)
		}
		tc = &tls.Config{Certificates: []tls.Certificate{c}, MinVersion: tls.VersionTLS12}
	case mode == "self-signed":
		c, err := SelfSigned(filepath.Join(d.db.Dir(), "state"), d.hostNm)
		if err != nil {
			return nil, err
		}
		tc = &tls.Config{Certificates: []tls.Certificate{c}, MinVersion: tls.VersionTLS12}
	default:
		return nil, nil
	}
	if clientCA != "" {
		pem, err := os.ReadFile(algod.ExpandHome(clientCA))
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(pem)
		tc.ClientCAs = pool
		tc.ClientAuth = tls.VerifyClientCertIfGiven
	}
	tc.NextProtos = []string{"h2", "http/1.1"}
	return tc, nil
}

// SelfSigned generates and persists an ECDSA certificate under dir.
func SelfSigned(dir, host string) (tls.Certificate, error) {
	certPath, keyPath := filepath.Join(dir, "tls-cert.pem"), filepath.Join(dir, "tls-key.pem")
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return c, nil
	}
	certPEM, keyPEM, err := GenerateCert(host, nil, nil, false)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := fsx.WriteAtomic(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := fsx.WriteAtomic(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// GenerateCert makes a certificate. With parent set it is signed by that
// CA, for mutual TLS between hub and agent. isCA makes a CA certificate.
func GenerateCert(host string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, isCA bool) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host, Organization: []string{"algod.top"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{host, "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	if isCA {
		tmpl.IsCA = true
		tmpl.BasicConstraintsValid = true
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	signer, signKey := tmpl, key
	if parent != nil {
		signer, signKey = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signKey)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), nil
}

// agentGate requires the agent token or a verified client certificate on
// every request to a TLS agent, not only on the pull endpoint. A hub is
// the only client a TLS agent has.
func agentGate(tokenHash string, mtls bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok := false
		if mtls && r.TLS != nil && len(r.TLS.VerifiedChains) > 0 {
			ok = true
		}
		if tokenHash != "" {
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") && subtle.ConstantTimeCompare([]byte(api.HashToken(strings.TrimPrefix(h, "Bearer "))), []byte(tokenHash)) == 1 {
				ok = true
			}
		}
		if !ok {
			http.Error(w, "agent: token or client certificate required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
