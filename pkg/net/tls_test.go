package net

import (
	"crypto/x509"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCAPool_Generate(t *testing.T) {
	pool, err := NewCAPool()
	require.NoError(t, err)

	require.NotNil(t, pool.caCert, "CA certificate should not be nil")
	require.NotNil(t, pool.caKey, "CA key should not be nil")
	assert.True(t, pool.caCert.IsCA, "Certificate should be a CA")
}

func TestCAPool_EphemeralCA(t *testing.T) {
	pool1, err := NewCAPool()
	require.NoError(t, err)

	pool2, err := NewCAPool()
	require.NoError(t, err)

	assert.NotEqual(t, 0, pool1.caCert.SerialNumber.Cmp(pool2.caCert.SerialNumber),
		"Each NewCAPool should generate a unique CA")
}

func TestCAPool_GetCertificate(t *testing.T) {
	pool, err := NewCAPool()
	require.NoError(t, err)

	cert, err := pool.GetCertificate("example.com")
	require.NoError(t, err)
	require.NotNil(t, cert, "Certificate should not be nil")

	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)

	assert.Equal(t, "example.com", x509Cert.Subject.CommonName)
	assert.Contains(t, x509Cert.DNSNames, "example.com",
		"Certificate should have example.com in DNS names")
}

func TestCAPool_CertificateCache(t *testing.T) {
	pool, err := NewCAPool()
	require.NoError(t, err)

	cert1, _ := pool.GetCertificate("test.example.com")
	cert2, _ := pool.GetCertificate("test.example.com")

	assert.Same(t, cert1, cert2, "Certificates should be cached and identical")
}

func TestCAPool_CACertPEM(t *testing.T) {
	pool, err := NewCAPool()
	require.NoError(t, err)

	pem := pool.CACertPEM()

	require.NotEmpty(t, pem, "PEM should not be empty")
	assert.Equal(t, "-----BEGIN CERTIFICATE-----", string(pem[:27]),
		"PEM should start with certificate header")
}

func TestCAPool_DifferentDomains(t *testing.T) {
	pool, err := NewCAPool()
	require.NoError(t, err)

	domains := []string{"example.com", "test.org", "api.service.io"}

	for _, domain := range domains {
		cert, err := pool.GetCertificate(domain)
		require.NoError(t, err, "GetCertificate(%s) failed", domain)

		x509Cert, _ := x509.ParseCertificate(cert.Certificate[0])
		assert.Equal(t, domain, x509Cert.Subject.CommonName)
	}
}
