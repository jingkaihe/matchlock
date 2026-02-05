package net

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type CAPool struct {
	caCert     *x509.Certificate
	caKey      *rsa.PrivateKey
	certCache  sync.Map
	cacheDir   string
}

func NewCAPool() (*CAPool, error) {
	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".sandbox", "mitm")
	os.MkdirAll(cacheDir, 0700)

	pool := &CAPool{cacheDir: cacheDir}

	caCertPath := filepath.Join(cacheDir, "ca.crt")
	caKeyPath := filepath.Join(cacheDir, "ca.key")

	if _, err := os.Stat(caCertPath); err == nil {
		if err := pool.loadCA(caCertPath, caKeyPath); err == nil {
			return pool, nil
		}
	}

	if err := pool.generateCA(); err != nil {
		return nil, err
	}

	if err := pool.saveCA(caCertPath, caKeyPath); err != nil {
		return nil, err
	}

	return pool, nil
}

func (p *CAPool) loadCA(certPath, keyPath string) error {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return err
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}

	certBlock, _ := pem.Decode(certPEM)
	p.caCert, err = x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return err
	}

	keyBlock, _ := pem.Decode(keyPEM)
	p.caKey, err = x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return err
	}

	return nil
}

func (p *CAPool) generateCA() error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	p.caKey = key

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Sandbox MITM CA"},
			CommonName:   "Sandbox MITM CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}

	p.caCert, err = x509.ParseCertificate(certDER)
	return err
}

func (p *CAPool) saveCA(certPath, keyPath string) error {
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: p.caCert.Raw,
	})
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return err
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(p.caKey),
	})
	return os.WriteFile(keyPath, keyPEM, 0600)
}

func (p *CAPool) GetCertificate(serverName string) (*tls.Certificate, error) {
	if cached, ok := p.certCache.Load(serverName); ok {
		return cached.(*tls.Certificate), nil
	}

	cert, err := p.generateCertificate(serverName)
	if err != nil {
		return nil, err
	}

	p.certCache.Store(serverName, cert)
	return cert, nil
}

func (p *CAPool) generateCertificate(serverName string) (*tls.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	serialNumber, _ := rand.Int(rand.Reader, big.NewInt(1<<62))

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: serverName,
		},
		DNSNames:    []string{serverName},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().AddDate(1, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		return nil, err
	}

	return &tls.Certificate{
		Certificate: [][]byte{certDER, p.caCert.Raw},
		PrivateKey:  key,
	}, nil
}

func (p *CAPool) CACertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: p.caCert.Raw,
	})
}

func (p *CAPool) CACertPath() string {
	return filepath.Join(p.cacheDir, "ca.crt")
}
