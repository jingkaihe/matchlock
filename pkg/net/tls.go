package net

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"sync"
	"time"
)

type CAPool struct {
	caCert    *x509.Certificate
	caKey     *rsa.PrivateKey
	certCache sync.Map
}

func NewCAPool() (*CAPool, error) {
	pool := &CAPool{}

	if err := pool.generateCA(); err != nil {
		return nil, err
	}

	return pool, nil
}

func (p *CAPool) generateCA() error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	p.caKey = key

	serialNumber, _ := rand.Int(rand.Reader, big.NewInt(1<<62))

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Sandbox MITM CA"},
			CommonName:   "Sandbox MITM CA",
		},
		NotBefore:             time.Now().Add(-5 * time.Minute), // Allow for clock skew
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
		NotBefore:   time.Now().Add(-5 * time.Minute), // Allow for clock skew
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
