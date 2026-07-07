// Package sslmgr loads X.509 certificates and private keys from the
// site_domains table and exposes a tls.Config-compatible GetCertificate
// callback for SNI-based selection. Reload() rebuilds the in-memory cache;
// the admin GUI calls it after every cert add/update/delete so changes go
// live without restarting didapi.
package sslmgr

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Domain mirrors the site_domains row plus the parsed cert metadata that's
// useful in the admin UI.
type Domain struct {
	ID            int64
	Hostname      string
	CertPEM       string
	KeyPEM        string
	CertSubject   string
	CertIssuer    string
	CertExpiresAt *time.Time
	IsDefault     bool
	Notes         string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type Manager struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	mu          sync.RWMutex
	bySNI       map[string]*tls.Certificate
	defaultCert *tls.Certificate
	domains     []Domain
}

func New(pool *pgxpool.Pool, log *slog.Logger) *Manager {
	return &Manager{pool: pool, log: log, bySNI: map[string]*tls.Certificate{}}
}

// IsConfigured returns true when at least one usable cert has been loaded.
// main.go uses it to decide whether to start the HTTPS listener at all.
func (m *Manager) IsConfigured() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.bySNI) > 0
}

// Domains returns a snapshot of the loaded rows for the admin UI.
func (m *Manager) Domains() []Domain {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Domain, len(m.domains))
	copy(out, m.domains)
	return out
}

// GetCertificate is the tls.Config callback. Picks the cert matching the
// SNI hostname; falls back to the row marked is_default; finally any cert
// (so older clients without SNI still get *something*).
func (m *Manager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	name := strings.ToLower(strings.TrimSpace(hello.ServerName))
	if name != "" {
		if c, ok := m.bySNI[name]; ok {
			return c, nil
		}
		// Wildcard match: foo.example.com → *.example.com
		if i := strings.Index(name, "."); i > 0 {
			if c, ok := m.bySNI["*"+name[i:]]; ok {
				return c, nil
			}
		}
	}
	if m.defaultCert != nil {
		return m.defaultCert, nil
	}
	for _, c := range m.bySNI {
		return c, nil
	}
	return nil, errors.New("no TLS certificate configured")
}

// Reload pulls every site_domains row, parses the PEM, and replaces the
// in-memory map atomically. Best-effort: rows with bad PEM are logged and
// skipped, so one broken cert doesn't drop the rest.
func (m *Manager) Reload(ctx context.Context) error {
	rows, err := m.pool.Query(ctx, `
		SELECT id, hostname, COALESCE(cert_pem,''), COALESCE(key_pem,''),
		       COALESCE(cert_subject,''), COALESCE(cert_issuer,''),
		       cert_expires_at, is_default, COALESCE(notes,''),
		       created_at, updated_at
		  FROM site_domains
		 ORDER BY hostname`)
	if err != nil {
		return err
	}
	defer rows.Close()

	nextBySNI := map[string]*tls.Certificate{}
	var nextDefault *tls.Certificate
	var domains []Domain
	for rows.Next() {
		var d Domain
		if err := rows.Scan(&d.ID, &d.Hostname, &d.CertPEM, &d.KeyPEM,
			&d.CertSubject, &d.CertIssuer, &d.CertExpiresAt, &d.IsDefault,
			&d.Notes, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return err
		}
		domains = append(domains, d)
		if d.CertPEM == "" || d.KeyPEM == "" {
			continue
		}
		cert, err := tls.X509KeyPair([]byte(d.CertPEM), []byte(d.KeyPEM))
		if err != nil {
			m.log.Warn("sslmgr: skipping bad cert", "hostname", d.Hostname, "err", err)
			continue
		}
		host := strings.ToLower(d.Hostname)
		nextBySNI[host] = &cert
		if d.IsDefault {
			nextDefault = &cert
		}
	}

	m.mu.Lock()
	m.bySNI = nextBySNI
	m.defaultCert = nextDefault
	m.domains = domains
	m.mu.Unlock()

	m.log.Info("sslmgr loaded", "domains", len(domains), "certs", len(nextBySNI))
	return nil
}

// CertMeta is the parsed public-info subset we expose in the admin UI when
// validating an upload, and what we persist back to site_domains.
type CertMeta struct {
	Subject   string
	Issuer    string
	NotBefore time.Time
	NotAfter  time.Time
	DNSNames  []string
}

// ParseCertPEM decodes the first CERTIFICATE block in pemBytes and returns
// its summary metadata. Used by the admin handler so the GUI can display
// expiry / subject before saving.
func ParseCertPEM(pemBytes []byte) (CertMeta, error) {
	var out CertMeta
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return out, fmt.Errorf("no CERTIFICATE PEM block found")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return out, fmt.Errorf("parse certificate: %w", err)
	}
	out.Subject = c.Subject.String()
	out.Issuer = c.Issuer.String()
	out.NotBefore = c.NotBefore
	out.NotAfter = c.NotAfter
	out.DNSNames = c.DNSNames
	return out, nil
}

// ValidateKeyPEM returns nil if keyBytes contains a recognizable private
// key block. We don't care which algorithm — tls.X509KeyPair is the final
// arbiter and runs at Reload — but a blatant non-key value should fail
// fast at the GUI so the admin sees the issue before saving.
func ValidateKeyPEM(keyBytes []byte) error {
	block, _ := pem.Decode(keyBytes)
	if block == nil {
		return fmt.Errorf("no PEM block found")
	}
	switch block.Type {
	case "PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY":
		return nil
	}
	return fmt.Errorf("unexpected PEM block type %q (expected PRIVATE KEY / RSA PRIVATE KEY / EC PRIVATE KEY)", block.Type)
}
