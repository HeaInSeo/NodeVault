package kubeutil

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/HeaInSeo/kube-slint/pkg/slo"
	"gopkg.in/yaml.v3"
)

type crbMeta struct {
	Name string `yaml:"name"`
}

type crbRoleRef struct {
	APIGroup string `yaml:"apiGroup"`
	Kind     string `yaml:"kind"`
	Name     string `yaml:"name"`
}

type crbSubject struct {
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

type clusterRoleBindingDoc struct {
	APIVersion string       `yaml:"apiVersion"`
	Kind       string       `yaml:"kind"`
	Metadata   crbMeta      `yaml:"metadata"`
	RoleRef    crbRoleRef   `yaml:"roleRef"`
	Subjects   []crbSubject `yaml:"subjects"`
}

// ApplyClusterRoleBinding applies a ClusterRoleBinding in an idempotent way (kubectl apply).
// - logger may be nil (no-op).
// - r may be nil (uses DefaultRunner).
func ApplyClusterRoleBinding(
	ctx context.Context,
	logger slo.Logger,
	r CmdRunner,
	name string,
	clusterRole string,
	ns string,
	sa string,
) error {
	logger = slo.NewLogger(logger)
	if r == nil {
		r = DefaultRunner{}
	}

	logger.Logf(
		"apply ClusterRoleBinding name=%q role=%q sa=%s/%s",
		name,
		clusterRole,
		ns,
		sa,
	)

	doc := clusterRoleBindingDoc{
		APIVersion: "rbac.authorization.k8s.io/v1",
		Kind:       "ClusterRoleBinding",
		Metadata:   crbMeta{Name: name},
		RoleRef: crbRoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     clusterRole,
		},
		Subjects: []crbSubject{
			{Kind: "ServiceAccount", Name: sa, Namespace: ns},
		},
	}

	manifest, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("marshal ClusterRoleBinding: %w", err)
	}

	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(string(manifest))

	stdout, err := r.Run(ctx, logger, cmd)

	if s := strings.TrimSpace(stdout); s != "" {
		logger.Logf("%s", strings.TrimRight(s, "\n"))
	}
	if err != nil {
		return fmt.Errorf("kubectl apply clusterrolebinding failed: %w", err)
	}
	return nil
}
