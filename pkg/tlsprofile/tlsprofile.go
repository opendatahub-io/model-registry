package tlsprofile

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	tlspkg "github.com/openshift/controller-runtime-common/pkg/tls"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	restclient "k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const bootstrapTimeout = 10 * time.Second

// Result contains cluster TLS policy in both controller-runtime and net/http forms.
type Result struct {
	TLSOpts               []func(*tls.Config)
	MinVersion            uint16
	CipherSuites          []uint16
	Profile               configv1.TLSProfileSpec
	AdherencePolicy       configv1.TLSAdherencePolicy
	HasOpenShiftConfigAPI bool
	KeepExisting          bool
	FallbackReason        string
}

// Resolve reads the cluster profile and adherence policy from one APIServer object.
func Resolve(cfg *restclient.Config) (*Result, error) {
	if cfg == nil {
		return nil, errors.New("kubernetes REST config is nil")
	}
	bootstrapConfig := restclient.CopyConfig(cfg)
	bootstrapConfig.Timeout = bootstrapTimeout
	scheme := runtime.NewScheme()
	if err := configv1.Install(scheme); err != nil {
		return nil, fmt.Errorf("register OpenShift config API: %w", err)
	}
	bootstrapClient, err := client.New(bootstrapConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create bounded TLS policy client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), bootstrapTimeout)
	defer cancel()
	return resolveFromClient(ctx, bootstrapClient)
}

func resolveFromClient(ctx context.Context, c client.Client) (*Result, error) {
	apiServer := &configv1.APIServer{}
	err := c.Get(ctx, client.ObjectKey{Name: "cluster"}, apiServer)
	if err != nil {
		switch {
		case apimeta.IsNoMatchError(err), apierrors.IsNotFound(err):
			return &Result{KeepExisting: true}, nil
		case isTransient(err):
			intermediate := &configv1.APIServer{Spec: configv1.APIServerSpec{
				TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileIntermediateType},
				TLSAdherence:       configv1.TLSAdherencePolicyStrictAllComponents,
			}}
			result, resolveErr := ResolveAPIServer(intermediate)
			if resolveErr != nil {
				return nil, resolveErr
			}
			result.HasOpenShiftConfigAPI = true
			result.FallbackReason = err.Error()
			return result, nil
		default:
			return nil, fmt.Errorf("read OpenShift APIServer TLS policy: %w", err)
		}
	}
	return ResolveAPIServer(apiServer)
}

func isTransient(err error) bool {
	var networkErr net.Error
	return apierrors.IsServiceUnavailable(err) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err) || errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &networkErr) && networkErr.Timeout())
}

// ResolveAPIServer validates and resolves an already-read APIServer object.
func ResolveAPIServer(apiServer *configv1.APIServer) (*Result, error) {
	if apiServer == nil {
		return nil, errors.New("APIServer object is nil")
	}
	profile, err := profileSpec(apiServer.Spec.TLSSecurityProfile)
	if err != nil {
		return nil, err
	}
	result := &Result{
		Profile:               profile,
		AdherencePolicy:       apiServer.Spec.TLSAdherence,
		HasOpenShiftConfigAPI: true,
	}
	minVersion, err := minimumTLSVersion(profile.MinTLSVersion)
	if err != nil {
		return nil, err
	}
	profileTLSOpts, _ := tlspkg.NewTLSConfigFromProfile(profile)
	probe := &tls.Config{}
	profileTLSOpts(probe)
	result.MinVersion = minVersion
	if minVersion < tls.VersionTLS13 {
		usable := usableCipherSuites(probe.CipherSuites)
		if len(profile.Ciphers) > 0 && len(usable) == 0 {
			return nil, fmt.Errorf("TLS profile has no usable cipher suites for TLS <= 1.2")
		}
		result.CipherSuites = usable
	}
	if apiServer.Spec.TLSAdherence == configv1.TLSAdherencePolicyNoOpinion ||
		apiServer.Spec.TLSAdherence == configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly {
		result.KeepExisting = true
		return result, nil
	}

	if minVersion < tls.VersionTLS13 && len(profile.Ciphers) == 0 {
		return nil, fmt.Errorf("TLS profile has no cipher suites for TLS <= 1.2")
	}

	result.TLSOpts = []func(*tls.Config){profileTLSOpts}
	if minVersion < tls.VersionTLS13 && len(profile.Ciphers) > 0 {
		usable := result.CipherSuites
		result.TLSOpts = append(result.TLSOpts, func(c *tls.Config) { c.CipherSuites = usable })
	}
	return result, nil
}

func profileSpec(profile *configv1.TLSSecurityProfile) (configv1.TLSProfileSpec, error) {
	if profile == nil || profile.Type == "" {
		return *configv1.TLSProfiles[configv1.TLSProfileIntermediateType], nil
	}
	if profile.Type == configv1.TLSProfileCustomType {
		if profile.Custom == nil {
			return configv1.TLSProfileSpec{}, errors.New("custom TLS profile is missing its custom settings")
		}
		return profile.Custom.TLSProfileSpec, nil
	}
	if spec, ok := configv1.TLSProfiles[profile.Type]; ok {
		return *spec, nil
	}
	return configv1.TLSProfileSpec{}, fmt.Errorf("unsupported TLS profile type %q", profile.Type)
}

func minimumTLSVersion(version configv1.TLSProtocolVersion) (uint16, error) {
	switch version {
	case "":
		return tls.VersionTLS12, nil
	case configv1.VersionTLS10:
		return tls.VersionTLS10, nil
	case configv1.VersionTLS11:
		return tls.VersionTLS11, nil
	case configv1.VersionTLS12:
		return tls.VersionTLS12, nil
	case configv1.VersionTLS13:
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("unsupported minimum TLS version %q", version)
	}
}

func usableCipherSuites(requested []uint16) []uint16 {
	implemented := make(map[uint16]struct{})
	for _, suite := range tls.CipherSuites() {
		implemented[suite.ID] = struct{}{}
	}
	for _, suite := range tls.InsecureCipherSuites() {
		if !strings.Contains(suite.Name, "RC4") {
			implemented[suite.ID] = struct{}{}
		}
	}
	var usable []uint16
	for _, id := range requested {
		if id >= 0x1300 && id <= 0x13ff {
			continue
		}
		if _, ok := implemented[id]; ok {
			usable = append(usable, id)
		}
	}
	return usable
}

// NewWatcher creates the restart watcher when the config API is available.
func NewWatcher(result *Result, cancel context.CancelFunc) *tlspkg.SecurityProfileWatcher {
	if result == nil || !result.HasOpenShiftConfigAPI {
		return nil
	}
	return &tlspkg.SecurityProfileWatcher{
		InitialTLSProfileSpec:     result.Profile,
		InitialTLSAdherencePolicy: result.AdherencePolicy,
		OnProfileChange: func(context.Context, configv1.TLSProfileSpec, configv1.TLSProfileSpec) {
			if cancel != nil {
				cancel()
			}
		},
		OnAdherencePolicyChange: func(context.Context, configv1.TLSAdherencePolicy, configv1.TLSAdherencePolicy) {
			if cancel != nil {
				cancel()
			}
		},
	}
}
