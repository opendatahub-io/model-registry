package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDecodeAndCheck(t *testing.T) {
	tests := []struct {
		name         string
		manifest     string
		requireOwned bool
		want         bool
	}{
		{
			name: "resolved binding and workload identity",
			manifest: `apiVersion: v1
kind: ServiceAccount
metadata:
  name: model-registry
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: read
subjects:
  - kind: ServiceAccount
    name: model-registry
    namespace: system
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller
spec:
  template:
    spec:
      serviceAccountName: model-registry
`,
			want: true,
		},
		{
			name: "binding names an unrendered service account",
			manifest: `apiVersion: v1
kind: ServiceAccount
metadata:
  name: model-registry
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: read
subjects:
  - kind: ServiceAccount
    name: controller-manager
    namespace: system
`,
			want: false,
		},
		{
			name: "workload names an unrendered service account",
			manifest: `apiVersion: v1
kind: ServiceAccount
metadata:
  name: model-registry
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller
spec:
  template:
    spec:
      serviceAccountName: controller-manager
`,
			want: false,
		},
		{
			name: "overlay does not own the service accounts",
			manifest: `apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: read
subjects:
  - kind: ServiceAccount
    name: platform-created
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller
spec:
  template:
    spec:
      serviceAccountName: platform-created
`,
			want: true,
		},
		{
			name: "complete overlay lost its service account",
			manifest: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: model-registry-deployment
spec:
  template:
    spec:
      serviceAccountName: model-registry-server
`,
			requireOwned: true,
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rendered.yaml")
			if err := os.WriteFile(path, []byte(tt.manifest), 0o600); err != nil {
				t.Fatal(err)
			}

			resources, err := decode(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := check(path, resources, tt.requireOwned); got != tt.want {
				t.Fatalf("check() = %v, want %v", got, tt.want)
			}
		})
	}
}
