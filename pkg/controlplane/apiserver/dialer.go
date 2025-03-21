package apiserver

import (
	"context"
	"crypto/tls"
	"errors"
	mathrand "math/rand"
	"net"
	"net/http"
	"time"

	"k8s.io/klog/v2"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	kubeappoptions "k8s.io/kubernetes/cmd/kube-apiserver/app/options"
	kubeoptions "k8s.io/kubernetes/pkg/kubeapiserver/options"
)

func CreateOutboundDialer(p kubeoptions.IPNetSlice) (*http.Transport, error) {
	proxyDialerFn := createAllowlistDialer(p)
	// This must be set because it gets plumbed to node proxy handler later.
	kubeappoptions.ProxyCIDRAllowlist = p

	proxyTLSClientConfig := &tls.Config{InsecureSkipVerify: true}

	proxyTransport := utilnet.SetTransportDefaults(&http.Transport{
		DialContext:     proxyDialerFn,
		TLSClientConfig: proxyTLSClientConfig,
	})
	return proxyTransport, nil
}

func createAllowlistDialer(allowlist kubeoptions.IPNetSlice) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		start := time.Now()
		id := mathrand.Int63() // So you can match begins/ends in the log.
		klog.Infof("[%x: %v] Dialing...", id, addr)
		defer func() {
			klog.Infof("[%x: %v] Dialed in %v.", id, addr, time.Since(start))
		}()

		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, errors.New("Invalid address")
		}

		if !allowlist.Contains(host) {
			return nil, errors.New("Address is not allowed")
		}
		dialer := &net.Dialer{}
		return dialer.DialContext(ctx, network, addr)
	}
}
