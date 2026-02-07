package net

import (
	"crypto/x509"
	"testing"
)

func TestCAPool_Generate(t *testing.T) {
	pool, err := NewCAPool()
	if err != nil {
		t.Fatalf("NewCAPool failed: %v", err)
	}

	if pool.caCert == nil {
		t.Error("CA certificate should not be nil")
	}

	if pool.caKey == nil {
		t.Error("CA key should not be nil")
	}

	if !pool.caCert.IsCA {
		t.Error("Certificate should be a CA")
	}
}

func TestCAPool_EphemeralCA(t *testing.T) {
	pool1, err := NewCAPool()
	if err != nil {
		t.Fatalf("First NewCAPool failed: %v", err)
	}

	pool2, err := NewCAPool()
	if err != nil {
		t.Fatalf("Second NewCAPool failed: %v", err)
	}

	if pool1.caCert.SerialNumber.Cmp(pool2.caCert.SerialNumber) == 0 {
		t.Error("Each NewCAPool should generate a unique CA")
	}
}

func TestCAPool_GetCertificate(t *testing.T) {
	pool, err := NewCAPool()
	if err != nil {
		t.Fatalf("NewCAPool failed: %v", err)
	}

	cert, err := pool.GetCertificate("example.com")
	if err != nil {
		t.Fatalf("GetCertificate failed: %v", err)
	}

	if cert == nil {
		t.Fatal("Certificate should not be nil")
	}

	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("Failed to parse certificate: %v", err)
	}

	if x509Cert.Subject.CommonName != "example.com" {
		t.Errorf("Expected CN=example.com, got %s", x509Cert.Subject.CommonName)
	}

	found := false
	for _, name := range x509Cert.DNSNames {
		if name == "example.com" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Certificate should have example.com in DNS names")
	}
}

func TestCAPool_CertificateCache(t *testing.T) {
	pool, err := NewCAPool()
	if err != nil {
		t.Fatalf("NewCAPool failed: %v", err)
	}

	cert1, _ := pool.GetCertificate("test.example.com")
	cert2, _ := pool.GetCertificate("test.example.com")

	if cert1 != cert2 {
		t.Error("Certificates should be cached and identical")
	}
}

func TestCAPool_CACertPEM(t *testing.T) {
	pool, err := NewCAPool()
	if err != nil {
		t.Fatalf("NewCAPool failed: %v", err)
	}

	pem := pool.CACertPEM()

	if len(pem) == 0 {
		t.Error("PEM should not be empty")
	}

	if string(pem[:27]) != "-----BEGIN CERTIFICATE-----" {
		t.Error("PEM should start with certificate header")
	}
}

func TestCAPool_DifferentDomains(t *testing.T) {
	pool, err := NewCAPool()
	if err != nil {
		t.Fatalf("NewCAPool failed: %v", err)
	}

	domains := []string{"example.com", "test.org", "api.service.io"}

	for _, domain := range domains {
		cert, err := pool.GetCertificate(domain)
		if err != nil {
			t.Errorf("GetCertificate(%s) failed: %v", domain, err)
			continue
		}

		x509Cert, _ := x509.ParseCertificate(cert.Certificate[0])
		if x509Cert.Subject.CommonName != domain {
			t.Errorf("Expected CN=%s, got %s", domain, x509Cert.Subject.CommonName)
		}
	}
}
