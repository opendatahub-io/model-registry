package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"os"
	"slices"
	"testing"

	"github.com/kubeflow/hub/ui/bff/internal/tlsprofile"
	configv1 "github.com/openshift/api/config/v1"
	restclient "k8s.io/client-go/rest"
)

func TestInitializeTLSPolicyFailsClosedOnResolveError(t *testing.T) {
	wantErr := errors.New("forbidden")
	watcherStarted := false
	_, err := initializeTLSPolicy("cert", "key", false, tlsPolicyDependencies{
		getConfig: func() (*restclient.Config, error) {
			return &restclient.Config{}, nil
		},
		resolve: func(*restclient.Config) (*tlsprofile.Result, error) {
			return nil, wantErr
		},
		startWatcher: func(*restclient.Config, *tlsprofile.Result) (chan error, context.CancelFunc, error) {
			watcherStarted = true
			return nil, nil, nil
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("initializeTLSPolicy error = %v, want %v", err, wantErr)
	}
	if watcherStarted {
		t.Fatal("watcher started after TLS policy resolution failed")
	}
}

func TestInitializeTLSPolicyStartsWatcherAfterTransientFallback(t *testing.T) {
	wantPolicy := &tlsprofile.Result{HasOpenShiftConfigAPI: true, FallbackReason: "timeout"}
	wantDone := make(chan error, 1)
	wantStop := func() {}
	watcherStarted := false
	runtime, err := initializeTLSPolicy("cert", "key", false, tlsPolicyDependencies{
		getConfig: func() (*restclient.Config, error) {
			return &restclient.Config{}, nil
		},
		resolve: func(*restclient.Config) (*tlsprofile.Result, error) {
			return wantPolicy, nil
		},
		startWatcher: func(_ *restclient.Config, policy *tlsprofile.Result) (chan error, context.CancelFunc, error) {
			watcherStarted = policy == wantPolicy
			return wantDone, wantStop, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.policy != wantPolicy || runtime.done != wantDone || runtime.stop == nil || !watcherStarted {
		t.Fatalf("TLS runtime = %+v, want policy, watcher channel, and stop function", runtime)
	}
}

func TestWaitForShutdown(t *testing.T) {
	tests := []struct {
		name      string
		serverErr error
		policyErr error
		wantExit  int
		wantError bool
	}{
		{name: "server error", serverErr: errors.New("listen failed"), wantExit: 1, wantError: true},
		{name: "server closed", serverErr: http.ErrServerClosed},
		{name: "watcher error", policyErr: errors.New("watch failed"), wantExit: 1, wantError: true},
		{name: "policy change", policyErr: context.Canceled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shutdownCh := make(chan os.Signal, 1)
			var serverDone <-chan error
			var policyDone <-chan error
			if tt.serverErr != nil {
				ch := make(chan error, 1)
				ch <- tt.serverErr
				serverDone = ch
			}
			if tt.policyErr != nil {
				ch := make(chan error, 1)
				ch <- tt.policyErr
				policyDone = ch
			}

			gotExit, err := waitForShutdown(shutdownCh, serverDone, policyDone)
			if gotExit != tt.wantExit {
				t.Fatalf("exit code = %d, want %d", gotExit, tt.wantExit)
			}
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v, want error %t", err, tt.wantError)
			}
		})
	}
}

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

func TestBuildTLSConfigDisablesHTTP2WithoutCompatibleCipherSuite(t *testing.T) {
	result, err := tlsprofile.ResolveAPIServer(&configv1.APIServer{Spec: configv1.APIServerSpec{
		TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileCustomType, Custom: &configv1.CustomTLSProfile{
			TLSProfileSpec: configv1.TLSProfileSpec{
				MinTLSVersion: configv1.VersionTLS12,
				Ciphers:       []string{"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384"},
			},
		}},
		TLSAdherence: configv1.TLSAdherencePolicyStrictAllComponents,
	}})
	if err != nil {
		t.Fatal(err)
	}

	if !shouldDisableHTTP2(result) {
		t.Fatal("shouldDisableHTTP2 = false, want true")
	}

	cfg := buildTLSConfig(result)
	if !slices.Equal(cfg.NextProtos, []string{"http/1.1"}) {
		t.Fatalf("NextProtos = %v, want [http/1.1]", cfg.NextProtos)
	}
}
