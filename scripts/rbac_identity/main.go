package main

import (
	"errors"
	"flag"
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
	requireOwned := flag.Bool("require-owned-service-accounts", false,
		"treat ServiceAccount references as unresolved when the render creates no ServiceAccounts")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: rbac_identity [-require-owned-service-accounts] <rendered-manifest-path>...")
	}
	flag.Parse()

	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	var files []string
	for _, arg := range flag.Args() {
		info, err := os.Stat(arg)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if !info.IsDir() {
			files = append(files, arg)
			continue
		}
		matches, err := filepath.Glob(filepath.Join(arg, "*.yaml"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		files = append(files, matches...)
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
		if !check(file, resources, *requireOwned) {
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

func check(path string, resources []resource, requireOwned bool) bool {
	serviceAccounts := map[string]struct{}{}
	for _, item := range resources {
		if item.Kind == "ServiceAccount" && item.Metadata.Name != "" {
			serviceAccounts[item.Metadata.Name] = struct{}{}
		}
	}
	// A partial overlay can legitimately reference accounts created by the
	// overlay it is applied on top of, so an empty account set is only an
	// error when the caller declares that the render must be self-contained.
	if len(serviceAccounts) == 0 && !requireOwned {
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
