package main

import (
	"crypto/tls"
	"testing"

	"github.com/kubeflow/hub/pkg/tlsprofile"
	configv1 "github.com/openshift/api/config/v1"
)

func TestControllerTLSOptionsApplyProfileAndPreserveALPN(t *testing.T) {
	policy, err := tlsprofile.ResolveAPIServer(&configv1.APIServer{Spec: configv1.APIServerSpec{
		TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileModernType},
		TLSAdherence:       configv1.TLSAdherencePolicyStrictAllComponents,
	}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &tls.Config{}
	for _, opt := range controllerTLSOptions(false, policy) {
		opt(cfg)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %v, want TLS 1.3", cfg.MinVersion)
	}
	if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "http/1.1" {
		t.Fatalf("NextProtos = %v, want existing HTTP/2-disabled ALPN", cfg.NextProtos)
	}
}

func TestControllerTLSOptionsKeepHistoricalConfig(t *testing.T) {
	policy, err := tlsprofile.ResolveAPIServer(&configv1.APIServer{Spec: configv1.APIServerSpec{
		TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileModernType},
	}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &tls.Config{}
	for _, opt := range controllerTLSOptions(false, policy) {
		opt(cfg)
	}
	if cfg.MinVersion != 0 || len(cfg.CipherSuites) != 0 || len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "http/1.1" {
		t.Fatalf("historical config changed: %+v", cfg)
	}
}
