package environments

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"time"

	"k8s.io/apimachinery/pkg/util/httpstream"
	httpstreamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport/spdy"
)

type openingDialer struct {
	ctx    context.Context
	config *rest.Config
	url    *url.URL
}

func (d openingDialer) Dial(protocols ...string) (httpstream.Connection, string, error) {
	config := rest.CopyConfig(d.config)
	dial := config.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	var rawConnection net.Conn
	stopCancellation := func() bool { return true }
	config.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		rawConnection = connection
		// The SPDY transport does not observe request cancellation while reading the upgrade response.
		stopCancellation = context.AfterFunc(d.ctx, func() { _ = connection.Close() })
		return connection, nil
	}
	established := false
	defer func() {
		stopCancellation()
		if !established && rawConnection != nil {
			_ = rawConnection.Close()
		}
	}()
	transport, err := rest.TransportFor(config)
	if err != nil {
		return nil, "", err
	}
	upgrader, err := httpstreamspdy.NewRoundTripperWithConfig(httpstreamspdy.RoundTripperConfig{UpgradeTransport: transport, PingPeriod: 5 * time.Second})
	if err != nil {
		return nil, "", err
	}
	wrapped, err := rest.HTTPWrappersForConfig(config, upgrader)
	if err != nil {
		return nil, "", err
	}
	request, err := http.NewRequestWithContext(d.ctx, http.MethodPost, d.url.String(), nil)
	if err != nil {
		return nil, "", err
	}
	connection, protocol, err := spdy.Negotiate(upgrader, &http.Client{Transport: wrapped}, request, protocols...)
	if err != nil {
		if d.ctx.Err() != nil {
			return nil, "", d.ctx.Err()
		}
		return nil, "", err
	}
	// Once upgraded, the opening deadline must not limit the lifetime of the tunnel.
	if !stopCancellation() || d.ctx.Err() != nil {
		_ = connection.Close()
		return nil, "", d.ctx.Err()
	}
	established = true
	return connection, protocol, nil
}
