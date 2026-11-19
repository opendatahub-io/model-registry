package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/spf13/cobra"
)

// newMLflowOperationsCommand builds the operator tool for inspecting and
// explicitly resolving uncertain MLflow native operations. It reuses the
// backend's resource-only factory — the SQL compatibility connection and
// the native registry client — and never starts the proxy listener,
// migrates or initializes SQL, imports objects, or runs background
// workers: the store must already have been initialized by a running
// backend, and the configured registry target must match the one the
// store is bound to.
func newMLflowOperationsCommand(open MLflowResourceFactory) *cobra.Command {
	config := MLflowBackendConfig{
		DatabaseType:    "mysql",
		RequestTimeout:  mlflow.DefaultRequestTimeout,
		RefreshInterval: mlflow.DefaultRefreshInterval,
	}

	command := &cobra.Command{
		Use:   "mlflow-operations",
		Short: "Inspect and resolve uncertain MLflow native operations",
		Long: `Inspect and explicitly resolve journaled MLflow operations whose
native create left an uncertain outcome.

The commands open the compatibility database and the native registry
read-only through the same configuration as --backend=mlflow, verify the
store is initialized and bound to the configured registry target, and
resolve only the explicitly selected operation. They never migrate,
initialize, import, or start workers.

  list                                        show every operation's state,
                                              checkpoint, lease, and retry details
  bind --operation UUID --model NAME
      --version NUMBER                       adopt one verified native identity
  retry-create --operation UUID
      --accept-duplicate-risk                supersede an uncertain create with a
                                              fresh, linked attempt
  resolve-orphan --operation UUID
      --retain-native                        abandon an unreferenced child intent
                                              and retain its native results

Resolution never chooses the newest match, performs broad cleanup, or
deletes native duplicates: conflicts stay visible for an explicit
decision.`,
	}

	bindMLflowResourceFlags(command.PersistentFlags(), &config)

	listCommand := &cobra.Command{
		Use:   "list",
		Short: "List journaled operations with sanitized details",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withMLflowOperationService(cmd.Context(), open, config,
				func(ctx context.Context, store *mlflow.Store, _ *mlflow.Service) error {
					return listMLflowOperations(ctx, cmd.OutOrStdout(), store)
				})
		},
	}

	bindCommand := &cobra.Command{
		Use:   "bind",
		Short: "Adopt one verified native identity for an uncertain operation",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withMLflowOperationService(cmd.Context(), open, config,
				func(ctx context.Context, _ *mlflow.Store, service *mlflow.Service) error {
					return service.BindNativeIdentity(ctx, operationFlags.operation,
						operationFlags.model, operationFlags.version)
				})
		},
	}
	bindCommand.Flags().StringVar(&operationFlags.operation, "operation", "",
		"UUID of the operation to resolve (required)")
	bindCommand.Flags().StringVar(&operationFlags.model, "model", "",
		"native registered-model name of the identity to adopt (required)")
	bindCommand.Flags().StringVar(&operationFlags.version, "version", "",
		"native model-version number of the identity to adopt (required)")
	_ = bindCommand.MarkFlagRequired("operation")
	_ = bindCommand.MarkFlagRequired("model")
	_ = bindCommand.MarkFlagRequired("version")

	retryCommand := &cobra.Command{
		Use:   "retry-create",
		Short: "Supersede an uncertain native create with a fresh, linked attempt",
		Long: `Authorize a fresh native create attempt for an operation whose
dispatched create left an uncertain outcome.

The earlier dispatched request may still commit. If it does, its native
version remains visible in the registry and is never deleted by the
adapter; later observations retain an audit record of its identity.
The original operation is superseded — not
erased — and the new attempt is linked to it, keeping the dispatch
checkpoint, prior native results, and attempt history auditable.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !operationFlags.acceptDuplicateRisk {
				return fmt.Errorf(
					"retry-create requires --accept-duplicate-risk: the earlier dispatched request may still commit, leaving a visible native duplicate: %w",
					api.ErrBadRequest)
			}
			return withMLflowOperationService(cmd.Context(), open, config,
				func(ctx context.Context, _ *mlflow.Store, service *mlflow.Service) error {
					return service.RetryCreate(ctx, operationFlags.operation)
				})
		},
	}
	retryCommand.Flags().StringVar(&operationFlags.operation, "operation", "",
		"UUID of the operation to supersede (required)")
	retryCommand.Flags().BoolVar(&operationFlags.acceptDuplicateRisk, "accept-duplicate-risk", false,
		"Acknowledge that the earlier dispatched create may still commit, leaving a visible native duplicate")
	_ = retryCommand.MarkFlagRequired("operation")

	orphanCommand := &cobra.Command{
		Use:   "resolve-orphan",
		Short: "Abandon an unreferenced child while retaining native results",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !operationFlags.retainNative {
				return fmt.Errorf("resolve-orphan requires --retain-native: %w", api.ErrBadRequest)
			}
			return withMLflowOperationService(cmd.Context(), open, config, func(ctx context.Context, _ *mlflow.Store, service *mlflow.Service) error {
				return service.ResolveOrphan(ctx, operationFlags.operation, operationFlags.retainNative)
			})
		},
	}
	orphanCommand.Flags().StringVar(&operationFlags.operation, "operation", "", "UUID of the orphan child to resolve (required)")
	orphanCommand.Flags().BoolVar(&operationFlags.retainNative, "retain-native", false, "Retain all native results and abandon only this unpublished intent")
	_ = orphanCommand.MarkFlagRequired("operation")
	command.AddCommand(listCommand, bindCommand, retryCommand, orphanCommand)
	return command
}

// operationFlags holds the per-subcommand selection flags. Each
// subcommand validates its own required flags.
var operationFlags struct {
	operation           string
	model               string
	version             string
	acceptDuplicateRisk bool
	retainNative        bool
}

// withMLflowOperationService opens the backend resources through the
// injected factory, verifies the store is initialized and bound to the
// configured registry target, and runs one operator action against a
// service that starts no workers and performs no startup work.
func withMLflowOperationService(ctx context.Context, open MLflowResourceFactory,
	config MLflowBackendConfig, action func(ctx context.Context,
		store *mlflow.Store, service *mlflow.Service) error) error {
	fingerprint, err := mlflow.RegistryTargetFingerprint(config.RegistryURI)
	if err != nil {
		return fmt.Errorf("invalid mlflow registry target: %w", err)
	}

	store, native, cleanup, err := open(ctx, config)
	if err != nil {
		return err
	}
	var cleanupOnce sync.Once
	defer cleanupOnce.Do(func() { _ = cleanup() })

	adapter, err := store.GetAdapter(ctx)
	if err != nil {
		if errors.Is(err, api.ErrNotFound) {
			return fmt.Errorf("the mlflow compatibility store is not initialized; "+
				"start the mlflow backend against it once before resolving operations: %w", err)
		}
		return err
	}
	if adapter.TargetFingerprint != fingerprint {
		return fmt.Errorf("the compatibility store is bound to a different mlflow registry target: %w",
			api.ErrConflict)
	}

	service, err := mlflow.NewService(mlflow.Config{
		RegistryURI:     config.RegistryURI,
		RequestTimeout:  config.RequestTimeout,
		RefreshInterval: config.RefreshInterval,
	}, store, native)
	if err != nil {
		return err
	}
	defer func() { _ = service.Close() }()
	return action(ctx, store, service)
}

// listMLflowOperations prints every journaled operation with its
// sanitized state: identifiers, lifecycle, checkpoint, lease, retry
// scheduling, and the recorded native result. No credentials, DSNs, or
// registry URLs appear in the output.
func listMLflowOperations(ctx context.Context, out io.Writer, store *mlflow.Store) error {
	operations, err := store.ListOperations(ctx)
	if err != nil {
		return err
	}
	if len(operations) == 0 {
		_, err := fmt.Fprintln(out, "no operations")
		return err
	}
	for _, operation := range operations {
		if _, err := fmt.Fprintf(out, "operation %s\n", operation.OperationUUID); err != nil {
			return err
		}
		lines := []string{
			fmt.Sprintf("  kind:             %s", operation.Kind),
			fmt.Sprintf("  resource:         %d", operation.ResourceID),
			fmt.Sprintf("  state:            %s", operation.State),
			fmt.Sprintf("  step:             %s", operation.Step),
			fmt.Sprintf("  generation:       %d", operation.Generation),
			fmt.Sprintf("  attempts:         %d", operation.Attempts),
			fmt.Sprintf("  lease owner:      %s", derefOrDash(operation.LeaseOwner)),
			fmt.Sprintf("  lease expires:    %s", formatMillis(operation.LeaseExpiresSinceEpoch)),
			fmt.Sprintf("  next retry:       %s", formatMillis(operation.NextRetrySinceEpoch)),
			fmt.Sprintf("  failure code:     %s", derefOrDash(operation.FailureCode)),
		}
		if operation.ParentOperationUUID != nil {
			lines = append(lines, fmt.Sprintf("  parent operation: %s", *operation.ParentOperationUUID))
		}
		if operation.NativeResult != nil {
			var result mlflow.NativeBindingResult
			if json.Unmarshal([]byte(*operation.NativeResult), &result) == nil {
				lines = append(lines, fmt.Sprintf("  native result:    model=%q version=%q generation=%d", result.ModelName, result.VersionNumber, result.Generation))
			} else {
				lines = append(lines, "  native result:    recorded (legacy or undecodable)")
			}
		}
		if operation.SupersededByOperationUUID != nil {
			lines = append(lines, fmt.Sprintf("  selected attempt: %s", *operation.SupersededByOperationUUID))
		}
		var dispositions []mlflow.NativeDisposition
		if err := store.DB().WithContext(ctx).Where("operation_uuid = ?", operation.OperationUUID).Order("native_model_name, native_version_number").Find(&dispositions).Error; err != nil {
			return err
		}
		for _, retained := range dispositions {
			lines = append(lines, fmt.Sprintf("  retained native:  model=%q version=%q disposition=%s resolved-by=%s", retained.NativeModelName, retained.NativeVersionNumber, retained.Disposition, retained.ResolvingOperationUUID))
		}
		if _, err := fmt.Fprint(out, strings.Join(lines, "\n")+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// derefOrDash renders an optional string; nil prints as a dash.
func derefOrDash(value *string) string {
	if value == nil {
		return "-"
	}
	return *value
}

// formatMillis renders an optional millisecond timestamp; nil prints as
// a dash.
func formatMillis(value *int64) string {
	if value == nil {
		return "-"
	}
	return time.UnixMilli(*value).UTC().Format(time.RFC3339)
}

func init() {
	rootCmd.AddCommand(newMLflowOperationsCommand(openMLflowResources))
}
