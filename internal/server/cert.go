package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Browsers trust a self-signed certificate for WebTransport by its SHA-256
// hash (serverCertificateHashes) only if it is ECDSA and valid for at most
// 14 days. So the self-signed certificate lives 13 days and is replaced a
// day before it expires; the page asks for the current hash when it connects.
const (
	certLifetime = 13 * 24 * time.Hour
	certRenewal  = 24 * time.Hour
)

type certSource struct {
	mu       sync.Mutex
	dir      string // "" with a user certificate
	hosts    []string
	cert     *tls.Certificate
	leaf     *x509.Certificate
	userCert bool
}

func newCertSource(certFile, keyFile, dir, addr string) (*certSource, error) {
	if certFile != "" || keyFile != "" {
		if certFile == "" || keyFile == "" {
			return nil, errors.New("--cert and --key go together")
		}
		c, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		return &certSource{cert: &c, userCert: true}, nil
	}
	cs := &certSource{dir: dir, hosts: certHosts(addr)}
	if err := cs.load(); err != nil {
		return nil, err
	}
	return cs, nil
}

// certHosts are the names the self-signed certificate is for.
func certHosts(addr string) []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if h, _, err := net.SplitHostPort(addr); err == nil && h != "" && h != "0.0.0.0" && h != "::" {
		hosts = append(hosts, h)
	}
	if name, err := os.Hostname(); err == nil {
		hosts = append(hosts, name)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				hosts = append(hosts, n.IP.String())
			}
		}
	}
	return hosts
}

func (c *certSource) tlsConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: c.get}
}

func (c *certSource) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.userCert && time.Until(c.leaf.NotAfter) < certRenewal {
		if err := c.generate(); err != nil {
			return nil, err
		}
	}
	return c.cert, nil
}

// hash is the base64 SHA-256 of the self-signed certificate ("" with a
// user certificate, which browsers verify the usual way).
func (c *certSource) hash() string {
	if c.userCert {
		return ""
	}
	if _, err := c.get(nil); err != nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sum := sha256.Sum256(c.cert.Certificate[0])
	return base64.StdEncoding.EncodeToString(sum[:])
}

// load reuses the saved certificate while it has more than a day left.
func (c *certSource) load() error {
	cf, kf := filepath.Join(c.dir, "cert.pem"), filepath.Join(c.dir, "key.pem")
	if pair, err := tls.LoadX509KeyPair(cf, kf); err == nil {
		if leaf, err := x509.ParseCertificate(pair.Certificate[0]); err == nil && time.Until(leaf.NotAfter) > certRenewal {
			c.cert, c.leaf = &pair, leaf
			return nil
		}
	}
	return c.generate()
}

func (c *certSource) generate() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "persona server"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certLifetime - time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range c.hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	c.cert, c.leaf = &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf
	if c.dir == "" {
		return nil
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}
