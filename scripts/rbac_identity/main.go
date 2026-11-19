package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

type resource struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Subjects []struct {
		Kind      string `yaml:"kind"`
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"subjects"`
	Spec struct {
		Template struct {
			Spec struct {
				ServiceAccountName string `yaml:"serviceAccountName"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: rbac_identity <rendered-manifest-directory>")
		os.Exit(2)
	}

	files, err := filepath.Glob(filepath.Join(os.Args[1], "*.yaml"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	sort.Strings(files)

	failed := false
	for _, file := range files {
		resources, err := decode(file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", filepath.Base(file), err)
			failed = true
			continue
		}
		if !check(file, resources) {
			failed = true
		}
	}

	if failed {
		os.Exit(1)
	}
}

func decode(path string) ([]resource, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var resources []resource
	decoder := yaml.NewDecoder(file)
	for {
		var item resource
		err := decoder.Decode(&item)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if item.Kind != "" {
			resources = append(resources, item)
		}
	}
	return resources, nil
}

func check(path string, resources []resource) bool {
	serviceAccounts := map[string]struct{}{}
	for _, item := range resources {
		if item.Kind == "ServiceAccount" && item.Metadata.Name != "" {
			serviceAccounts[item.Metadata.Name] = struct{}{}
		}
	}
	if len(serviceAccounts) == 0 {
		return true
	}

	failed := false
	for _, item := range resources {
		switch item.Kind {
		case "RoleBinding", "ClusterRoleBinding":
			for _, subject := range item.Subjects {
				if subject.Kind != "ServiceAccount" {
					continue
				}
				if _, ok := serviceAccounts[subject.Name]; ok {
					continue
				}
				fmt.Fprintf(os.Stderr, "%s: %s/%s binds ServiceAccount %s/%s, which the render does not create\n",
					filepath.Base(path), item.Kind, item.Metadata.Name, subject.Namespace, subject.Name)
				failed = true
			}
		case "Deployment", "StatefulSet", "DaemonSet":
			name := item.Spec.Template.Spec.ServiceAccountName
			if name == "" {
				continue
			}
			if _, ok := serviceAccounts[name]; ok {
				continue
			}
			fmt.Fprintf(os.Stderr, "%s: %s/%s runs as ServiceAccount %s, which the render does not create\n",
				filepath.Base(path), item.Kind, item.Metadata.Name, name)
			failed = true
		}
	}
	return !failed
}
