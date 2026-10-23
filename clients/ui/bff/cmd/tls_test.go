package main

import (
	"crypto/tls"
	"slices"
	"testing"

	"github.com/kubeflow/hub/pkg/tlsprofile"
	configv1 "github.com/openshift/api/config/v1"
)

func TestBuildTLSConfigAppliesClusterProfile(t *testing.T) {
	result, err := tlsprofile.ResolveAPIServer(&configv1.APIServer{Spec: configv1.APIServerSpec{
		TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileCustomType, Custom: &configv1.CustomTLSProfile{
			TLSProfileSpec: configv1.TLSProfileSpec{
				MinTLSVersion: configv1.VersionTLS12,
				Ciphers:       []string{"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"},
			},
		}},
		TLSAdherence: configv1.TLSAdherencePolicyStrictAllComponents,
	}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := buildTLSConfig(result)
	want := []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}
	if cfg.MinVersion != tls.VersionTLS12 || !slices.Equal(cfg.CipherSuites, want) {
		t.Fatalf("TLS config = min %v suites %v, want TLS 1.2 and %v", cfg.MinVersion, cfg.CipherSuites, want)
	}
	if !slices.Equal(cfg.NextProtos, []string{"h2", "http/1.1"}) {
		t.Fatalf("NextProtos = %v, want h2 and http/1.1", cfg.NextProtos)
	}
}

func TestBuildTLSConfigPreservesHistoricalDefault(t *testing.T) {
	result, err := tlsprofile.ResolveAPIServer(&configv1.APIServer{Spec: configv1.APIServerSpec{
		TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileIntermediateType},
	}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := buildTLSConfig(result)
	if cfg.MinVersion != tls.VersionTLS13 || len(cfg.CipherSuites) != 0 {
		t.Fatalf("legacy TLS config = min %v suites %v, want TLS 1.3 with Go-managed suites", cfg.MinVersion, cfg.CipherSuites)
	}
}

func TestBuildTLSConfigAppliesModernProfile(t *testing.T) {
	result, err := tlsprofile.ResolveAPIServer(&configv1.APIServer{Spec: configv1.APIServerSpec{
		TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileModernType},
		TLSAdherence:       configv1.TLSAdherencePolicyStrictAllComponents,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg := buildTLSConfig(result); cfg.MinVersion != tls.VersionTLS13 || len(cfg.CipherSuites) != 0 {
		t.Fatalf("modern TLS config = min %v suites %v, want TLS 1.3 with Go-managed suites", cfg.MinVersion, cfg.CipherSuites)
	}
}
