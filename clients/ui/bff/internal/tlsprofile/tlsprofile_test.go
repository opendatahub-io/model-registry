package tlsprofile

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	restclient "k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestResolveAPIServerModernProfile(t *testing.T) {
	result, err := ResolveAPIServer(&configv1.APIServer{
		Spec: configv1.APIServerSpec{
			TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileModernType},
			TLSAdherence:       configv1.TLSAdherencePolicyStrictAllComponents,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &tls.Config{}
	for _, opt := range result.TLSOpts {
		opt(cfg)
	}
	if cfg.MinVersion != tls.VersionTLS13 || result.MinVersion != cfg.MinVersion {
		t.Fatalf("TLS minimum: TLSOpts=%v BFF=%v, want TLS 1.3", cfg.MinVersion, result.MinVersion)
	}
	if len(cfg.CipherSuites) != 0 || len(result.CipherSuites) != 0 {
		t.Fatalf("TLS 1.3 cipher suites must remain Go-managed, got controller=%v BFF=%v", cfg.CipherSuites, result.CipherSuites)
	}
}

func TestResolveAPIServerOldProfile(t *testing.T) {
	result, err := ResolveAPIServer(&configv1.APIServer{
		Spec: configv1.APIServerSpec{
			TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileOldType},
			TLSAdherence:       configv1.TLSAdherencePolicyStrictAllComponents,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &tls.Config{}
	for _, opt := range result.TLSOpts {
		opt(cfg)
	}
	if cfg.MinVersion != tls.VersionTLS10 || result.MinVersion != cfg.MinVersion {
		t.Fatalf("TLS minimum: TLSOpts=%v BFF=%v, want TLS 1.0 (Old honored exactly, never downgraded)", cfg.MinVersion, result.MinVersion)
	}
	// The Old profile's 3DES and RSA key-exchange suites only appear in
	// tls.InsecureCipherSuites(); their presence proves the usable-cipher
	// lookup includes them instead of silently dropping Old's suites.
	if !slices.Contains(cfg.CipherSuites, tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA) {
		t.Fatalf("Old cipher list = %v, want the 3DES suite present", cfg.CipherSuites)
	}
}

func TestResolveAPIServerCustomCipherSubset(t *testing.T) {
	result, err := ResolveAPIServer(&configv1.APIServer{
		Spec: configv1.APIServerSpec{
			TLSSecurityProfile: &configv1.TLSSecurityProfile{
				Type: configv1.TLSProfileCustomType,
				Custom: &configv1.CustomTLSProfile{
					TLSProfileSpec: configv1.TLSProfileSpec{
						MinTLSVersion: configv1.VersionTLS12,
						Ciphers: []string{
							"TLS_AES_128_GCM_SHA256",
							"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
						},
					},
				},
			},
			TLSAdherence: configv1.TLSAdherencePolicyStrictAllComponents,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &tls.Config{}
	for _, opt := range result.TLSOpts {
		opt(cfg)
	}
	want := []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}
	if cfg.MinVersion != tls.VersionTLS12 || result.MinVersion != cfg.MinVersion {
		t.Fatalf("TLS minimum: controller=%v BFF=%v, want TLS 1.2", cfg.MinVersion, result.MinVersion)
	}
	if !slices.Equal(cfg.CipherSuites, want) || !slices.Equal(result.CipherSuites, want) {
		t.Fatalf("cipher suites: controller=%v BFF=%v, want %v", cfg.CipherSuites, result.CipherSuites, want)
	}
}

func TestResolveAPIServerDropsRC4Ciphers(t *testing.T) {
	resolve := func(ciphers []string) (*Result, error) {
		return ResolveAPIServer(&configv1.APIServer{
			Spec: configv1.APIServerSpec{
				TLSSecurityProfile: &configv1.TLSSecurityProfile{
					Type:   configv1.TLSProfileCustomType,
					Custom: &configv1.CustomTLSProfile{TLSProfileSpec: configv1.TLSProfileSpec{MinTLSVersion: configv1.VersionTLS12, Ciphers: ciphers}},
				},
				TLSAdherence: configv1.TLSAdherencePolicyStrictAllComponents,
			},
		})
	}

	// A mixed list keeps its usable subset with RC4 entries dropped.
	result, err := resolve([]string{"TLS_RSA_WITH_RC4_128_SHA", "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"})
	if err != nil {
		t.Fatalf("mixed list: unexpected error: %v", err)
	}
	want := []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}
	if !slices.Equal(result.CipherSuites, want) {
		t.Fatalf("mixed list: cipher suites %v, want %v", result.CipherSuites, want)
	}
	if !slices.Equal(result.UnsupportedCipherSuites, []uint16{tls.TLS_RSA_WITH_RC4_128_SHA}) {
		t.Fatalf("mixed list: unsupported cipher suites %v, want RC4", result.UnsupportedCipherSuites)
	}

	// An all-RC4 list has no usable suites and fails closed.
	if _, err := resolve([]string{"TLS_RSA_WITH_RC4_128_SHA"}); err == nil {
		t.Fatal("all-RC4 list: expected an error, got nil")
	}
}

func TestResolveAPIServerRetainsImplementedCustomCiphers(t *testing.T) {
	// Custom profiles may request any cipher implemented by the runtime.
	result, err := ResolveAPIServer(&configv1.APIServer{
		Spec: configv1.APIServerSpec{
			TLSSecurityProfile: &configv1.TLSSecurityProfile{
				Type: configv1.TLSProfileCustomType,
				Custom: &configv1.CustomTLSProfile{TLSProfileSpec: configv1.TLSProfileSpec{
					MinTLSVersion: configv1.VersionTLS12,
					Ciphers:       []string{"TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA", "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"},
				}},
			},
			TLSAdherence: configv1.TLSAdherencePolicyStrictAllComponents,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []uint16{tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}
	if !slices.Equal(result.CipherSuites, want) {
		t.Fatalf("cipher suites %v, want %v", result.CipherSuites, want)
	}
}

func TestResolveAPIServerEmptyProfileUsesIntermediate(t *testing.T) {
	result, err := ResolveAPIServer(&configv1.APIServer{
		Spec: configv1.APIServerSpec{TLSAdherence: configv1.TLSAdherencePolicyStrictAllComponents},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.KeepExisting || result.MinVersion != tls.VersionTLS12 {
		t.Fatalf("empty profile result = %+v, want enforced Intermediate TLS 1.2", result)
	}
}

func TestResolveAPIServerUnsetAndLegacyKeepExisting(t *testing.T) {
	for _, policy := range []configv1.TLSAdherencePolicy{
		configv1.TLSAdherencePolicyNoOpinion,
		configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly,
	} {
		t.Run(string(policy), func(t *testing.T) {
			result, err := ResolveAPIServer(&configv1.APIServer{
				Spec: configv1.APIServerSpec{
					TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileModernType},
					TLSAdherence:       policy,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !result.KeepExisting {
				t.Fatalf("KeepExisting = false for policy %q", policy)
			}
		})
	}
}

func TestResolveAPIServerUnknownAdherenceEnforcesProfile(t *testing.T) {
	result, err := ResolveAPIServer(&configv1.APIServer{
		Spec: configv1.APIServerSpec{
			TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileModernType},
			TLSAdherence:       "FuturePolicy",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.KeepExisting || result.MinVersion != tls.VersionTLS13 {
		t.Fatalf("unknown adherence policy did not enforce profile: %+v", result)
	}
}

func TestResolveAPIServerEmptyCiphersRequireStrictAdherence(t *testing.T) {
	for _, policy := range []configv1.TLSAdherencePolicy{
		configv1.TLSAdherencePolicyStrictAllComponents,
		"FuturePolicy",
	} {
		t.Run(string(policy), func(t *testing.T) {
			_, err := ResolveAPIServer(&configv1.APIServer{Spec: configv1.APIServerSpec{
				TLSSecurityProfile: &configv1.TLSSecurityProfile{
					Type: configv1.TLSProfileCustomType,
					Custom: &configv1.CustomTLSProfile{TLSProfileSpec: configv1.TLSProfileSpec{
						MinTLSVersion: configv1.VersionTLS12,
					}},
				},
				TLSAdherence: policy,
			}})
			if err == nil || !strings.Contains(err.Error(), "no cipher suites") {
				t.Fatalf("ResolveAPIServer error = %v, want no cipher suites error", err)
			}
		})
	}

	t.Run("NoOpinion", func(t *testing.T) {
		result, err := ResolveAPIServer(&configv1.APIServer{Spec: configv1.APIServerSpec{
			TLSSecurityProfile: &configv1.TLSSecurityProfile{
				Type: configv1.TLSProfileCustomType,
				Custom: &configv1.CustomTLSProfile{TLSProfileSpec: configv1.TLSProfileSpec{
					MinTLSVersion: configv1.VersionTLS12,
				}},
			},
			TLSAdherence: configv1.TLSAdherencePolicyNoOpinion,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if !result.KeepExisting {
			t.Fatalf("KeepExisting = false, want true: %+v", result)
		}
	})
}

func TestResolveFromClientReadsAPIServerOnce(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := configv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	reads := 0
	apiServer := &configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: configv1.APIServerSpec{
			TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileModernType},
			TLSAdherence:       configv1.TLSAdherencePolicyStrictAllComponents,
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(apiServer).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, inner client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			reads++
			return inner.Get(ctx, key, obj, opts...)
		},
	}).Build()
	if _, err := resolveFromClient(context.Background(), cl); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("APIServer Get calls = %d, want one", reads)
	}
}

func TestResolveFromClientDeniedReadFails(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := configv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	for name, readErr := range map[string]error{
		"forbidden":    apierrors.NewForbidden(schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "cluster", errors.New("denied")),
		"unauthorized": apierrors.NewUnauthorized("denied"),
		"bad request":  apierrors.NewBadRequest("malformed"),
		"unexpected":   errors.New("unexpected decode failure"),
	} {
		t.Run(name, func(t *testing.T) {
			cl := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return readErr
				},
			}).Build()
			if _, err := resolveFromClient(context.Background(), cl); err == nil {
				t.Fatal("denied APIServer read unexpectedly succeeded")
			}
		})
	}
}

func TestResolveDeniedAPIServerReadFailsStartup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api":
			_, _ = w.Write([]byte(`{"kind":"APIVersions","apiVersion":"v1","versions":["v1"]}`))
		case "/apis":
			_, _ = w.Write([]byte(`{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"config.openshift.io","versions":[{"groupVersion":"config.openshift.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"config.openshift.io/v1","version":"v1"}}]}`))
		case "/apis/config.openshift.io/v1":
			_, _ = w.Write([]byte(`{"kind":"APIResourceList","groupVersion":"config.openshift.io/v1","resources":[{"name":"apiservers","singularName":"apiserver","namespaced":false,"kind":"APIServer","verbs":["get"]}]}`))
		case "/apis/config.openshift.io/v1/apiservers/cluster":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"denied","reason":"Forbidden","code":403}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	_, err := Resolve(&restclient.Config{Host: server.URL})
	if err == nil || !strings.Contains(err.Error(), "read OpenShift APIServer TLS policy") {
		t.Fatalf("Resolve error = %v, want APIServer read failure", err)
	}
}

func TestResolveFromClientMissingAPIKeepsExisting(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := configv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	result, err := resolveFromClient(context.Background(), cl)
	if err != nil {
		t.Fatal(err)
	}
	if !result.KeepExisting || result.HasOpenShiftConfigAPI || NewWatcher(result, nil) != nil {
		t.Fatalf("missing API result = %+v, want keep-existing non-OpenShift result", result)
	}
}

func TestResolveFromClientNoMatchKeepsExisting(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := configv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return &apimeta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{
				Group: "config.openshift.io", Version: "v1", Resource: "apiservers",
			}}
		},
	}).Build()
	result, err := resolveFromClient(context.Background(), cl)
	if err != nil {
		t.Fatal(err)
	}
	if !result.KeepExisting || result.HasOpenShiftConfigAPI || NewWatcher(result, nil) != nil {
		t.Fatalf("no-match result = %+v, want keep-existing non-OpenShift result", result)
	}
}

func TestResolveFromClientTransientReadFallsBackAndWatches(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := configv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return apierrors.NewServiceUnavailable("temporary")
		},
	}).Build()
	result, err := resolveFromClient(context.Background(), cl)
	if err != nil {
		t.Fatal(err)
	}
	if result.KeepExisting || !result.HasOpenShiftConfigAPI || result.MinVersion != tls.VersionTLS12 || result.FallbackReason == "" || NewWatcher(result, nil) == nil {
		t.Fatalf("transient read result = %+v, want Intermediate fallback and watcher", result)
	}
}

func TestResolveAPIServerInvalidProfileFails(t *testing.T) {
	for name, profile := range map[string]*configv1.TLSSecurityProfile{
		"unknown type":         {Type: configv1.TLSProfileType("FutureProfile")},
		"missing custom":       {Type: configv1.TLSProfileCustomType},
		"invalid minimum":      {Type: configv1.TLSProfileCustomType, Custom: &configv1.CustomTLSProfile{TLSProfileSpec: configv1.TLSProfileSpec{MinTLSVersion: "TLS1.4"}}},
		"unusable cipher list": {Type: configv1.TLSProfileCustomType, Custom: &configv1.CustomTLSProfile{TLSProfileSpec: configv1.TLSProfileSpec{MinTLSVersion: configv1.VersionTLS12, Ciphers: []string{"TLS_AES_128_GCM_SHA256"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ResolveAPIServer(&configv1.APIServer{Spec: configv1.APIServerSpec{
				TLSSecurityProfile: profile,
				TLSAdherence:       configv1.TLSAdherencePolicyStrictAllComponents,
			}})
			if err == nil {
				t.Fatal("invalid profile unexpectedly accepted")
			}
		})
	}
}

func TestResolveAPIServerValidatesProfileEvenWhenKeepingExisting(t *testing.T) {
	_, err := ResolveAPIServer(&configv1.APIServer{Spec: configv1.APIServerSpec{
		TLSSecurityProfile: &configv1.TLSSecurityProfile{
			Type: configv1.TLSProfileCustomType,
			Custom: &configv1.CustomTLSProfile{TLSProfileSpec: configv1.TLSProfileSpec{
				MinTLSVersion: "VersionTLS99",
			}},
		},
		TLSAdherence: configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly,
	}})
	if err == nil {
		t.Fatal("malformed profile was accepted under Legacy adherence")
	}
}

func TestNewWatcherCancelsOnPolicyChanges(t *testing.T) {
	result, err := ResolveAPIServer(&configv1.APIServer{Spec: configv1.APIServerSpec{
		TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileIntermediateType},
		TLSAdherence:       configv1.TLSAdherencePolicyStrictAllComponents,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if NewWatcher(result, nil) == nil {
		t.Fatal("expected watcher for OpenShift config API")
	}
	for _, name := range []string{"profile", "adherence"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			watcher := NewWatcher(result, cancel)
			if name == "profile" {
				watcher.OnProfileChange(context.Background(), result.Profile, configv1.TLSProfileSpec{MinTLSVersion: configv1.VersionTLS13})
			} else {
				watcher.OnAdherencePolicyChange(context.Background(), result.AdherencePolicy, configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly)
			}
			select {
			case <-ctx.Done():
			default:
				t.Fatal("policy change did not cancel process context")
			}
		})
	}
}
