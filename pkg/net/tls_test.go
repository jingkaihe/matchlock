package net

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

func TestCAPool_Generate(t *testing.T) {
	tmpDir := t.TempDir()

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

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

func TestCAPool_LoadExisting(t *testing.T) {
	tmpDir := t.TempDir()

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	pool1, err := NewCAPool()
	if err != nil {
		t.Fatalf("First NewCAPool failed: %v", err)
	}

	serial1 := pool1.caCert.SerialNumber

	pool2, err := NewCAPool()
	if err != nil {
		t.Fatalf("Second NewCAPool failed: %v", err)
	}

	if pool2.caCert.SerialNumber.Cmp(serial1) != 0 {
		t.Error("Should load existing CA, not generate new one")
	}
}

func TestCAPool_GetCertificate(t *testing.T) {
	tmpDir := t.TempDir()

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

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
	tmpDir := t.TempDir()

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

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
	tmpDir := t.TempDir()

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

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

func TestCAPool_CACertPath(t *testing.T) {
	tmpDir := t.TempDir()

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	pool, err := NewCAPool()
	if err != nil {
		t.Fatalf("NewCAPool failed: %v", err)
	}

	path := pool.CACertPath()

	if !filepath.IsAbs(path) {
		t.Error("Path should be absolute")
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("CA cert file should exist at %s", path)
	}
}

func TestCAPool_DifferentDomains(t *testing.T) {
	tmpDir := t.TempDir()

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

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
