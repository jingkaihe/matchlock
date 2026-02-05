package net

import (
	"fmt"
	"os"
	"path/filepath"
)

type CAInjector struct {
	caPool *CAPool
}

func NewCAInjector(pool *CAPool) *CAInjector {
	return &CAInjector{caPool: pool}
}

func (i *CAInjector) CACertPEM() []byte {
	return i.caPool.CACertPEM()
}

func (i *CAInjector) CACertPath() string {
	return i.caPool.CACertPath()
}

func (i *CAInjector) GetEnvVars() map[string]string {
	certPath := "/etc/ssl/certs/sandbox-ca.crt"

	return map[string]string{
		"SSL_CERT_FILE":      certPath,
		"REQUESTS_CA_BUNDLE": certPath,
		"CURL_CA_BUNDLE":     certPath,
		"NODE_EXTRA_CA_CERTS": certPath,
	}
}

func (i *CAInjector) GetInstallScript() string {
	return `#!/bin/sh
set -e

CA_CERT="/tmp/sandbox-ca.crt"
DEST="/etc/ssl/certs/sandbox-ca.crt"

if [ -f "$CA_CERT" ]; then
    cp "$CA_CERT" "$DEST"
    
    # Update system CA store based on distro
    if command -v update-ca-certificates >/dev/null 2>&1; then
        # Debian/Ubuntu
        cp "$CA_CERT" /usr/local/share/ca-certificates/sandbox-ca.crt
        update-ca-certificates 2>/dev/null || true
    elif command -v update-ca-trust >/dev/null 2>&1; then
        # RHEL/CentOS/Fedora
        cp "$CA_CERT" /etc/pki/ca-trust/source/anchors/sandbox-ca.crt
        update-ca-trust extract 2>/dev/null || true
    elif [ -d /etc/ca-certificates/trust-source/anchors ]; then
        # Arch Linux
        cp "$CA_CERT" /etc/ca-certificates/trust-source/anchors/sandbox-ca.crt
        trust extract-compat 2>/dev/null || true
    fi
    
    echo "CA certificate installed successfully"
else
    echo "CA certificate not found at $CA_CERT"
    exit 1
fi
`
}

func (i *CAInjector) GetInitScript() string {
	return fmt.Sprintf(`#!/bin/sh
# Install MITM CA certificate for HTTPS interception

cat > /tmp/sandbox-ca.crt << 'CERT_EOF'
%s
CERT_EOF

%s
`, string(i.CACertPEM()), i.GetInstallScript())
}

func (i *CAInjector) WriteFiles(destDir string) error {
	certPath := filepath.Join(destDir, "sandbox-ca.crt")
	if err := os.WriteFile(certPath, i.CACertPEM(), 0644); err != nil {
		return fmt.Errorf("failed to write CA cert: %w", err)
	}

	scriptPath := filepath.Join(destDir, "install-ca.sh")
	if err := os.WriteFile(scriptPath, []byte(i.GetInstallScript()), 0755); err != nil {
		return fmt.Errorf("failed to write install script: %w", err)
	}

	initPath := filepath.Join(destDir, "init-ca.sh")
	if err := os.WriteFile(initPath, []byte(i.GetInitScript()), 0755); err != nil {
		return fmt.Errorf("failed to write init script: %w", err)
	}

	return nil
}

func (i *CAInjector) GetCurlFlags() string {
	return fmt.Sprintf("--cacert %s", i.CACertPath())
}

func (i *CAInjector) GetWgetFlags() string {
	return fmt.Sprintf("--ca-certificate=%s", i.CACertPath())
}

func (i *CAInjector) GetPythonCode() string {
	return fmt.Sprintf(`
import os
os.environ['SSL_CERT_FILE'] = '%s'
os.environ['REQUESTS_CA_BUNDLE'] = '%s'
`, "/etc/ssl/certs/sandbox-ca.crt", "/etc/ssl/certs/sandbox-ca.crt")
}

func (i *CAInjector) GetNodeCode() string {
	return fmt.Sprintf(`
process.env.NODE_EXTRA_CA_CERTS = '%s';
`, "/etc/ssl/certs/sandbox-ca.crt")
}
