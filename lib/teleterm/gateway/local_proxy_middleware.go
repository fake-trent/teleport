// Copyright 2022 Gravitational, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gateway

import (
	"context"
	"net"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/sirupsen/logrus"

	alpn "github.com/gravitational/teleport/lib/srv/alpnproxy"
	"github.com/gravitational/teleport/lib/utils"
)

type localProxyMiddleware struct {
	onExpiredCert func(context.Context) error
	log           *logrus.Entry
	clock         clockwork.Clock
}

// OnNewConnection calls m.onExpiredCert if the cert used by the local proxy has expired.
// This is a very basic reimplementation of client.DBCertChecker.OnNewConnection. DBCertChecker
// supports per-session MFA while for now Connect needs to just check for expired certs.
//
// In the future, DBCertChecker is going to be extended so that it's used by both tsh and Connect
// and this middleware will be removed.
func (m *localProxyMiddleware) OnNewConnection(ctx context.Context, lp *alpn.LocalProxy, conn net.Conn) (err error) {
	m.log.Debug("Checking local proxy certs")

	certs := lp.GetCerts()
	if len(certs) == 0 {
		return trace.Wrap(trace.NotFound("local proxy has no TLS certificates configured"))
	}

	cert, err := utils.TLSCertToX509(certs[0])
	if err != nil {
		return trace.Wrap(err)
	}

	err = utils.VerifyCertificateExpiry(cert, m.clock)
	if err != nil {
		m.log.WithError(err).Debug("Gateway certificates have expired")

		onExpiredCertErr := m.onExpiredCert(ctx)
		if onExpiredCertErr != nil {
			return trace.NewAggregate(err, onExpiredCertErr)
		}
	}

	return nil
}

// OnStart is a noop. client.DBCertChecker.OnStart checks cert validity too. However in Connect
// there's no flow which would allow the user to create a local proxy without valid
// certs.
func (m *localProxyMiddleware) OnStart(context.Context, *alpn.LocalProxy) error {
	return nil
}
