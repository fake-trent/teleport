// Copyright 2022 Gravitational, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package kinit provides utilities for interacting with a KDC (Key Distribution Center) for Kerberos5, or krb5, to allow
// teleport to connect to sqlserver using x509 certificates.
package kinit

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/auth/windows"
)

const (
	// krb5ConfigEnv sets the location from which kinit will attempt to read a configuration value
	krb5ConfigEnv = "KRB5_CONFIG"
)

// ProviderI is a provider interface for a kinit context
type ProviderI interface {
	// CreateOrAppendCredentialsCache will create or append to an existing credentials cache
	CreateOrAppendCredentialsCache(context.Context) error
	// CacheName is the cache name of this cache
	CacheName() string
}

// CommandLineKInit is a command line provider for a kinit context
type CommandLineKInit struct {
	authClient auth.ClientI

	userName  string
	cacheName string

	realmName string

	kdcHostName     string
	adminServerName string

	certPath string
	keyPath  string

	ldapCertificate *x509.Certificate

	log logrus.FieldLogger
}

// NewCommandProvider will return a new command line kinit provider
func NewCommandProvider(authClient auth.ClientI, user, realm, kdcHost, adminServer string, ldapCA *x509.Certificate) *CommandLineKInit {
	return &CommandLineKInit{
		authClient:      authClient,
		userName:        user,
		cacheName:       fmt.Sprintf("%s@%s", user, realm),
		realmName:       realm,
		kdcHostName:     kdcHost,
		adminServerName: adminServer,

		ldapCertificate: ldapCA,

		certPath: fmt.Sprintf("%s.pem", user),
		keyPath:  fmt.Sprintf("%s-key.pem", user),
		log:      logrus.StandardLogger(),
	}
}

// CreateOrAppendCredentialsCache creates or appends to an existing credentials cache.
func (k *CommandLineKInit) CreateOrAppendCredentialsCache(ctx context.Context) error {

	tmp := filepath.Join(os.TempDir(), "kinit")
	err := os.MkdirAll(tmp, 0664)
	if err != nil {
		return trace.Wrap(err)
	}

	certPath := filepath.Join(tmp, fmt.Sprintf("%s.pem", k.userName))
	keyPath := filepath.Join(tmp, fmt.Sprintf("%s-key.pem", k.userName))
	userCAPath := filepath.Join(tmp, "userca.pem")

	clusterName, err := k.authClient.GetClusterName()
	if err != nil {
		return trace.Wrap(err)
	}

	certPEM, keyPEM, err := windows.CertKeyPEM(ctx, k.userName, k.realmName, time.Second*60*60, clusterName.GetClusterName(), windows.LDAPConfig{
		Addr:               k.kdcHostName,
		Domain:             k.realmName,
		Username:           k.userName,
		InsecureSkipVerify: false,
		ServerName:         k.adminServerName,
		CA:                 k.ldapCertificate,
	}, k.authClient)

	userCA, err := k.authClient.GetCertAuthority(ctx, types.CertAuthID{
		Type:       types.UserCA,
		DomainName: clusterName.GetClusterName(),
	}, true)
	if err != nil {
		return trace.Wrap(err)
	}

	// get the user CA certificate bytes
	var caCert []byte
	keyPairs := userCA.GetActiveKeys().TLS
	for _, keyPair := range keyPairs {
		if keyPair.KeyType == types.PrivateKeyType_RAW {
			caCert = keyPair.Cert
		}
	}

	if caCert == nil {
		return trace.Wrap(errors.New("no certificate authority was found in userCA active keys"))
	}

	// store files in temp dir
	err = os.WriteFile(certPath, certPEM, 0644)
	if err != nil {
		return trace.Wrap(err)
	}

	err = os.WriteFile(keyPath, keyPEM, 0644)
	if err != nil {
		return trace.Wrap(err)
	}

	err = os.WriteFile(userCAPath, caCert, 0644)
	if err != nil {
		return trace.Wrap(err)
	}

	krbConfPath := filepath.Join(tmp, fmt.Sprintf("krb_%s", k.userName))
	err = k.WriteKRB5Config(krbConfPath)
	if err != nil {
		return trace.Wrap(err)
	}

	// I'm not a fan of this or of writing all these files in the first place, but in testing kinit, it does not have
	// the ability to access key data over stdin and requires these files to function for x509 auth
	defer func() {
		_ = os.RemoveAll(tmp)
	}()

	cmd := exec.CommandContext(ctx,
		"kinit",
		"-X", fmt.Sprintf("X509_anchors=FILE:%s", userCAPath),
		"-X", fmt.Sprintf("X509_user_identity=FILE:%s,%s", certPath, keyPath), k.userName,
		"-c", k.cacheName)
	cmd.Env = append(os.Environ(), []string{fmt.Sprintf("%s=%s", krb5ConfigEnv, krbConfPath)}...)

	data, err := cmd.CombinedOutput()
	if err != nil {
		return trace.Wrap(err)
	}

	if !bytes.Contains(data, []byte(fmt.Sprintf(`Storing %s@%s -> `, k.userName, strings.ToUpper(k.realmName)))) {
		k.log.Debug(string(data))
		return trace.Wrap(fmt.Errorf("unable to store credentials for user: %s, output: %s", k.userName, string(data)))
	}

	return nil
}

// CacheName returns the cache name of this cache
func (k *CommandLineKInit) CacheName() string {
	return k.cacheName
}

// krb5ConfigString returns a config suitable for a kdc
func (k *CommandLineKInit) krb5ConfigString() string {
	return fmt.Sprintf(`[libdefaults]
 default_realm = %s
 rdns = false


[realms]
 %s = {
  kdc = %s
  admin_server = %s
  pkinit_eku_checking = kpServerAuth
  pkinit_kdc_hostname = %s
 }`, k.realmName, k.realmName, k.kdcHostName, k.adminServerName, k.kdcHostName)
}

// WriteKRB5Config writes a krb configuration to path
func (k *CommandLineKInit) WriteKRB5Config(path string) error {
	return os.WriteFile(path, []byte(k.krb5ConfigString()), 0644)
}